// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/servers/sandrun-api/internal/api"
	"github.com/delinoio/mono/servers/sandrun-api/internal/testenv"
)

func TestInvitationsRevokedWhenCreatorLosesMemberManagement(t *testing.T) {
	for _, change := range []string{"removed", "left", "demoted"} {
		t.Run(change, func(t *testing.T) {
			env := testenv.New(t)
			ctx := t.Context()
			org := env.NewOrg("owner")
			admin := env.User(org.Admin).Member
			owner := env.User(org.Owner).Member
			setRole := func(role sandrunv1.Role) {
				t.Helper()
				if _, err := owner.UpdateMemberRole(ctx, connect.NewRequest(&sandrunv1.UpdateMemberRoleRequest{
					OrganizationId: org.ID, UserId: org.Admin, Role: role,
				})); err != nil {
					t.Fatal(err)
				}
			}

			expired := newMembershipInvitation(t, env, org.ID, org.Admin, sandrunv1.Role_ROLE_ADMIN)
			env.Join(org.ID, org.Admin, "other-member", sandrunv1.Role_ROLE_MEMBER)
			env.Clock.Advance(api.InvitationLifetime)

			revoked := newMembershipInvitation(t, env, org.ID, org.Admin, sandrunv1.Role_ROLE_MEMBER)
			revokedID := revoked.GetInvitation().GetInvitationId()
			if _, err := owner.RevokeInvitation(ctx, connect.NewRequest(&sandrunv1.RevokeInvitationRequest{
				OrganizationId: org.ID, InvitationId: revokedID,
			})); err != nil {
				t.Fatal(err)
			}
			explicitRevokeTime := env.Clock.Now()
			env.Clock.Advance(time.Second)

			adminInvite := newMembershipInvitation(t, env, org.ID, org.Admin, sandrunv1.Role_ROLE_ADMIN)
			memberInvite := newMembershipInvitation(t, env, org.ID, org.Admin, sandrunv1.Role_ROLE_MEMBER)
			ownerInviteID := newMembershipInvitation(t, env, org.ID, org.Owner, sandrunv1.Role_ROLE_MEMBER).GetInvitation().GetInvitationId()
			otherOrg := env.NewOrg("other-owner")
			env.Join(otherOrg.ID, otherOrg.Owner, org.Admin, sandrunv1.Role_ROLE_ADMIN)
			otherOrgInviteID := newMembershipInvitation(t, env, otherOrg.ID, org.Admin, sandrunv1.Role_ROLE_MEMBER).GetInvitation().GetInvitationId()

			actor := org.Owner
			recipient := org.Admin
			remove := connect.NewRequest(&sandrunv1.RemoveMemberRequest{OrganizationId: org.ID, UserId: org.Admin})
			switch change {
			case "removed":
				if _, err := owner.RemoveMember(ctx, remove); err != nil {
					t.Fatal(err)
				}
			case "left":
				actor = org.Admin
				if _, err := admin.RemoveMember(ctx, remove); err != nil {
					t.Fatal(err)
				}
			case "demoted":
				recipient = "prospective-member"
				setRole(sandrunv1.Role_ROLE_ADMIN)
				assertPendingInvitationIDs(t, env, org, adminInvite.GetInvitation().GetInvitationId(), memberInvite.GetInvitation().GetInvitationId(), ownerInviteID)
				setRole(sandrunv1.Role_ROLE_MEMBER)
				assertPendingInvitationIDs(t, env, org, ownerInviteID)
				setRole(sandrunv1.Role_ROLE_ADMIN)
			}

			assertPendingInvitationIDs(t, env, org, ownerInviteID)
			assertPendingInvitationIDs(t, env, otherOrg, otherOrgInviteID)
			for _, inv := range []*sandrunv1.CreateInvitationResponse{adminInvite, memberInvite} {
				assertInvitationRevoked(t, env, recipient, inv)
			}
			assertInvitationRevocationEvents(t, env, org, map[string]string{
				adminInvite.GetInvitation().GetInvitationId():  actor,
				memberInvite.GetInvitation().GetInvitationId(): actor,
				revokedID: org.Owner,
			})

			var acceptedUnrevoked int
			if err := env.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM invitations
				WHERE organization_id = $1 AND created_by_user_id = $2 AND accept_time IS NOT NULL AND revoke_time IS NULL`,
				org.ID, org.Admin).Scan(&acceptedUnrevoked); err != nil || acceptedUnrevoked != 1 {
				t.Fatalf("accepted invitations still unrevoked = %d, err = %v", acceptedUnrevoked, err)
			}
			var expiredRevoke, explicitRevoke *time.Time
			if err := env.DB.Pool.QueryRow(ctx, "SELECT revoke_time FROM invitations WHERE invitation_id = $1", expired.GetInvitation().GetInvitationId()).Scan(&expiredRevoke); err != nil || expiredRevoke != nil {
				t.Fatalf("expired invitation revoke_time = %v, err = %v", expiredRevoke, err)
			}
			if err := env.DB.Pool.QueryRow(ctx, "SELECT revoke_time FROM invitations WHERE invitation_id = $1", revokedID).Scan(&explicitRevoke); err != nil || explicitRevoke == nil || !explicitRevoke.Equal(explicitRevokeTime) {
				t.Fatalf("previously revoked invitation revoke_time = %v, want %s, err = %v", explicitRevoke, explicitRevokeTime, err)
			}
		})
	}
}

func TestCreateInvitationDuringMembershipChange(t *testing.T) {
	for _, tc := range []struct {
		name       string
		createWins bool
		demote     bool
		wantCode   connect.Code
	}{
		{name: "removal first", wantCode: connect.CodeNotFound},
		{name: "demotion first", demote: true, wantCode: connect.CodePermissionDenied},
		{name: "creation before removal", createWins: true},
		{name: "creation before demotion", createWins: true, demote: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := testenv.New(t)
			org := env.NewOrg("owner")
			admin := env.User(org.Admin).Member
			owner := env.User(org.Owner).Member
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			holder, err := env.DB.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer holder.Rollback(context.Background())
			holderPID := holder.Conn().PgConn().PID()
			var existing *sandrunv1.CreateInvitationResponse
			if tc.createWins {
				_, err = holder.Exec(ctx, "LOCK TABLE invitations IN SHARE MODE")
			} else {
				existing = newMembershipInvitation(t, env, org.ID, org.Admin, sandrunv1.Role_ROLE_MEMBER)
				_, err = holder.Exec(ctx, "SELECT 1 FROM invitations WHERE invitation_id = $1 FOR UPDATE", existing.GetInvitation().GetInvitationId())
			}
			if err != nil {
				t.Fatal(err)
			}

			type createResult struct {
				response *connect.Response[sandrunv1.CreateInvitationResponse]
				err      error
			}
			created := make(chan createResult, 1)
			changed := make(chan error, 1)
			create := func() {
				response, err := admin.CreateInvitation(ctx, connect.NewRequest(&sandrunv1.CreateInvitationRequest{
					OrganizationId: org.ID, Role: sandrunv1.Role_ROLE_ADMIN,
				}))
				created <- createResult{response, err}
			}
			change := func() {
				var err error
				if tc.demote {
					_, err = owner.UpdateMemberRole(ctx, connect.NewRequest(&sandrunv1.UpdateMemberRoleRequest{
						OrganizationId: org.ID, UserId: org.Admin, Role: sandrunv1.Role_ROLE_MEMBER,
					}))
				} else {
					_, err = owner.RemoveMember(ctx, connect.NewRequest(&sandrunv1.RemoveMemberRequest{OrganizationId: org.ID, UserId: org.Admin}))
				}
				changed <- err
			}

			if tc.createWins {
				go create()
				creatorPID := waitForInvitationLock(t, ctx, env, holderPID, "INSERT INTO invitations%")
				go change()
				waitForInvitationLock(t, ctx, env, creatorPID, "SELECT membership_id::text, role FROM memberships%FOR UPDATE")
			} else {
				go change()
				changePID := waitForInvitationLock(t, ctx, env, holderPID, "UPDATE invitations SET revoke_time%")
				go create()
				waitForInvitationLock(t, ctx, env, changePID, "SELECT role FROM memberships%FOR SHARE")
			}
			if err := holder.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-changed; err != nil {
				t.Fatal(err)
			}
			result := <-created
			wantCode(t, "concurrent invitation creation", result.err, tc.wantCode)
			assertPendingInvitationIDs(t, env, org)
			revoked := existing
			if tc.createWins {
				revoked = result.response.Msg
			}
			assertInvitationRevoked(t, env, "prospective-member", revoked)
			assertInvitationRevocationEvents(t, env, org, map[string]string{revoked.GetInvitation().GetInvitationId(): org.Owner})
			var invitations int
			if err := env.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM invitations WHERE organization_id = $1 AND created_by_user_id = $2", org.ID, org.Admin).Scan(&invitations); err != nil || invitations != 1 {
				t.Fatalf("invitations from concurrent creator = %d, want 1, err = %v", invitations, err)
			}
		})
	}
}

// Match the SQL and the blocking backend so an unrelated lock cannot satisfy the test.
func waitForInvitationLock(t *testing.T, ctx context.Context, env *testenv.Env, blocker uint32, query string) uint32 {
	t.Helper()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		var waiter uint32
		err := env.DB.Pool.QueryRow(ctx, `SELECT pid FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'
			AND query LIKE $1 AND $2 = ANY(pg_blocking_pids(pid))`, query, blocker).Scan(&waiter)
		if err == nil {
			return waiter
		}
		if err != pgx.ErrNoRows {
			t.Fatalf("observe lock for %q: %v", query, err)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("no session blocked on %q: %v", query, ctx.Err())
		case <-tick.C:
		}
	}
}

func newMembershipInvitation(t *testing.T, env *testenv.Env, orgID, creator string, role sandrunv1.Role) *sandrunv1.CreateInvitationResponse {
	t.Helper()
	resp, err := env.User(creator).Member.CreateInvitation(t.Context(), connect.NewRequest(&sandrunv1.CreateInvitationRequest{
		OrganizationId: orgID, Role: role,
	}))
	if err != nil {
		t.Fatal(err)
	}
	return resp.Msg
}

func assertPendingInvitationIDs(t *testing.T, env *testenv.Env, org testenv.Org, want ...string) {
	t.Helper()
	resp, err := env.User(org.Owner).Member.ListInvitations(t.Context(), connect.NewRequest(&sandrunv1.ListInvitationsRequest{OrganizationId: org.ID}))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, inv := range resp.Msg.GetInvitations() {
		got = append(got, inv.GetInvitationId())
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("pending invitation IDs = %v, want %v", got, want)
	}
}

func assertInvitationRevoked(t *testing.T, env *testenv.Env, user string, inv *sandrunv1.CreateInvitationResponse) {
	t.Helper()
	id := inv.GetInvitation().GetInvitationId()
	var revoked bool
	if err := env.DB.Pool.QueryRow(t.Context(), "SELECT revoke_time IS NOT NULL FROM invitations WHERE invitation_id = $1", id).Scan(&revoked); err != nil || !revoked {
		t.Fatalf("invitation %s revoked = %t, err = %v", id, revoked, err)
	}
	_, err := env.User(user).Member.AcceptInvitation(t.Context(), connect.NewRequest(&sandrunv1.AcceptInvitationRequest{InvitationSecret: inv.GetInvitationSecret()}))
	wantCode(t, "accept revoked invitation", err, connect.CodeNotFound)
}

func assertInvitationRevocationEvents(t *testing.T, env *testenv.Env, org testenv.Org, want map[string]string) {
	t.Helper()
	resp, err := env.User(org.Owner).Audit.ListAuditEvents(t.Context(), connect.NewRequest(&sandrunv1.ListAuditEventsRequest{OrganizationId: org.ID}))
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range resp.Msg.GetEvents() {
		if ev.GetAction() != sandrunv1.AuditAction_AUDIT_ACTION_INVITATION_REVOKED {
			continue
		}
		id := ev.GetTargetId()
		actor, ok := want[id]
		if !ok || ev.GetTargetKind() != sandrunv1.AuditTargetKind_AUDIT_TARGET_KIND_INVITATION || ev.GetActorUserId() != actor {
			t.Fatalf("unexpected invitation revocation audit event: %v", ev)
		}
		delete(want, id)
	}
	if len(want) != 0 {
		t.Fatalf("missing invitation revocation audit events: %v", want)
	}
}
