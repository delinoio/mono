// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/servers/sandrun-api/internal/testenv"
)

func TestAcceptInvitationChecksExpiryAfterLockWait(t *testing.T) {
	for _, tc := range []struct {
		name   string
		offset time.Duration
		accept bool
	}{
		{name: "before expiry", offset: -time.Second, accept: true},
		{name: "at expiry"},
		{name: "after expiry", offset: time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := testenv.New(t)
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()

			owner := env.User("alice")
			org, err := owner.Organization.CreateOrganization(ctx, connect.NewRequest(&sandrunv1.CreateOrganizationRequest{Name: "org"}))
			if err != nil {
				t.Fatal(err)
			}
			orgID := org.Msg.GetOrganization().GetOrganizationId()
			const role = sandrunv1.Role_ROLE_ADMIN
			created, err := owner.Member.CreateInvitation(ctx, connect.NewRequest(&sandrunv1.CreateInvitationRequest{
				OrganizationId: orgID, Role: role,
			}))
			if err != nil {
				t.Fatal(err)
			}
			inv := created.Msg.GetInvitation()
			release := holdTx(t, env, "SELECT 1 FROM invitations WHERE invitation_id = $1 FOR UPDATE", inv.GetInvitationId())
			const user = "carol"
			client := env.User(user).Member
			accept := func() (*connect.Response[sandrunv1.AcceptInvitationResponse], error) {
				return client.AcceptInvitation(ctx, connect.NewRequest(&sandrunv1.AcceptInvitationRequest{
					InvitationSecret: created.Msg.GetInvitationSecret(),
				}))
			}
			type result struct {
				response *connect.Response[sandrunv1.AcceptInvitationResponse]
				err      error
			}
			done := make(chan result, 1)
			go func() {
				resp, err := accept()
				done <- result{response: resp, err: err}
			}()

			waitForLockWaiter(t, env)
			checkTime := inv.GetExpireTime().AsTime().Add(tc.offset)
			env.Clock.Advance(checkTime.Sub(env.Clock.Now()))
			release()
			got := <-done

			wantWrites := 0
			if tc.accept {
				if got.err != nil {
					t.Fatal(got.err)
				}
				member := got.response.Msg.GetMember()
				if member.GetRole() != role || !member.GetJoinTime().AsTime().Equal(checkTime) {
					t.Fatalf("returned role and join time = %v, %v, want %v, %v", member.GetRole(), member.GetJoinTime(), role, checkTime)
				}
				_, err := accept()
				wantCode(t, "reused invitation", err, connect.CodeNotFound)
				wantWrites = 1
			} else {
				wantCode(t, "invitation expired during lock wait", got.err, connect.CodeNotFound)
			}

			var acceptTime, joinTime *time.Time
			var acceptedBy *string
			var storedRole *int16
			var memberships int
			err = env.DB.Pool.QueryRow(ctx, `SELECT i.accept_time, i.accepted_by_user_id, m.join_time, m.role,
				(SELECT count(*) FROM memberships WHERE organization_id = i.organization_id AND user_id = $2)
				FROM invitations i LEFT JOIN memberships m ON m.organization_id = i.organization_id AND m.user_id = $2
				WHERE i.invitation_id = $1`, inv.GetInvitationId(), user).Scan(&acceptTime, &acceptedBy, &joinTime, &storedRole, &memberships)
			if err != nil {
				t.Fatal(err)
			}
			if memberships != wantWrites {
				t.Errorf("membership count = %d, want %d", memberships, wantWrites)
			}
			if tc.accept {
				if acceptTime == nil || !acceptTime.Equal(checkTime) || acceptedBy == nil || *acceptedBy != user ||
					joinTime == nil || !joinTime.Equal(checkTime) || storedRole == nil || sandrunv1.Role(*storedRole) != role {
					t.Error("stored acceptance or membership did not match the user, role, and post-lock time")
				}
			} else if acceptTime != nil || acceptedBy != nil {
				t.Error("expired invitation was consumed")
			}
			if n := countAction(auditActions(t, env, orgID), sandrunv1.AuditAction_AUDIT_ACTION_MEMBER_JOINED); n != wantWrites {
				t.Errorf("join audit events = %d, want %d", n, wantWrites)
			}
		})
	}
}

func TestAcceptInvitationRejectsOrganizationDeletedDuringLockWait(t *testing.T) {
	env := testenv.New(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	org := env.NewOrg("alice")
	created, err := env.User(org.Owner).Member.CreateInvitation(ctx, connect.NewRequest(&sandrunv1.CreateInvitationRequest{
		OrganizationId: org.ID, Role: sandrunv1.Role_ROLE_MEMBER,
	}))
	if err != nil {
		t.Fatal(err)
	}
	// The acceptance sees the invitation until the deletion commits, but
	// it must wait for the organization row lock before it can proceed.
	commit := holdTx(t, env, "DELETE FROM organizations WHERE organization_id = $1", org.ID)
	done := make(chan error, 1)
	go func() {
		_, err := env.User("carol").Member.AcceptInvitation(ctx, connect.NewRequest(&sandrunv1.AcceptInvitationRequest{
			InvitationSecret: created.Msg.GetInvitationSecret(),
		}))
		done <- err
	}()
	waitForLockWaiter(t, env)
	commit()
	wantCode(t, "deleted organization invitation", <-done, connect.CodeNotFound)
}
