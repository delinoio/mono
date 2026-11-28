// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"maps"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	"github.com/delinoio/mono/servers/accounts-api/internal/api"
	"github.com/delinoio/mono/servers/accounts-api/internal/testenv"
)

func TestMembershipAuditTimestampsAfterLockWait(t *testing.T) {
	t.Parallel()

	updateRole := func(owner person, accountID, targetID string, role accountsv1.BillingAccountRole) error {
		_, err := owner.Client.UpdateMemberRole(ctx(), connect.NewRequest(&accountsv1.UpdateMemberRoleRequest{
			BillingAccountId: accountID, UserId: targetID, Role: role,
		}))
		return err
	}
	cases := []struct {
		name       string
		mutate     func(owner person, accountID, targetID string) error
		kind       api.AuditKind
		details    map[string]string
		ownerRole  api.Role
		targetRole api.Role
	}{
		{
			name: "role change",
			mutate: func(owner person, accountID, targetID string) error {
				return updateRole(owner, accountID, targetID, roleAdmin)
			},
			kind:       api.AuditMemberRoleChanged,
			details:    map[string]string{"old_role": string(api.RoleViewer), "new_role": string(api.RoleAdmin)},
			ownerRole:  api.RoleOwner,
			targetRole: api.RoleAdmin,
		},
		{
			name: "removal",
			mutate: func(owner person, accountID, targetID string) error {
				_, err := owner.Client.RemoveMember(ctx(), connect.NewRequest(&accountsv1.RemoveMemberRequest{
					BillingAccountId: accountID, UserId: targetID,
				}))
				return err
			},
			kind:      api.AuditMemberRemoved,
			details:   map[string]string{"role": string(api.RoleViewer)},
			ownerRole: api.RoleOwner,
		},
		{
			name: "ownership transfer",
			mutate: func(owner person, accountID, targetID string) error {
				_, err := owner.Client.TransferOwnership(ctx(), connect.NewRequest(&accountsv1.TransferOwnershipRequest{
					BillingAccountId: accountID, UserId: targetID,
				}))
				return err
			},
			kind:       api.AuditOwnershipTransferred,
			details:    map[string]string{"new_owner_previous_role": string(api.RoleViewer)},
			ownerRole:  api.RoleAdmin,
			targetRole: api.RoleOwner,
		},
		{
			name: "same role",
			mutate: func(owner person, accountID, targetID string) error {
				return updateRole(owner, accountID, targetID, roleViewer)
			},
			ownerRole:  api.RoleOwner,
			targetRole: api.RoleViewer,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := testenv.New(t)
			owner := newPerson(env, "owner")
			target := newPerson(env, "viewer")
			accountID := createAccount(t, owner, "Audit clock").GetBillingAccountId()
			invID := invite(t, owner, accountID, target.Email, roleViewer).GetInvitationId()
			before := readBusinessState(t, env, accountID)

			var acceptedAt time.Time
			err := raceMembershipChange(t, env, accountID, func(tx pgx.Tx) {
				// Match AcceptInvitation's writes while the RPC waits for this transaction.
				env.Clock.Advance(time.Second)
				acceptedAt = env.Clock.Now()
				execTx(t, tx, `INSERT INTO billing_account_members (billing_account_id, user_id, role, create_time)
					VALUES ($1, $2, $3, $4)`, accountID, target.ID, api.RoleViewer, acceptedAt)
				execTx(t, tx, `UPDATE billing_account_invitations SET accept_time = $2, accepted_user_id = $3
					WHERE invitation_id = $1`, invID, acceptedAt, target.ID)
				execTx(t, tx, `INSERT INTO billing_audit_events
					(audit_event_id, billing_account_id, kind, actor_user_id, target_user_id, invitation_id, details, create_time)
					VALUES (gen_random_uuid(), $1, $2, $3, $3, $4, $5, $6)`,
					accountID, api.AuditInvitationAccepted, target.ID, invID, map[string]string{"role": string(api.RoleViewer)}, acceptedAt)
			}, func() error {
				return tc.mutate(owner, accountID, target.ID)
			})
			wantCode(t, err, 0)

			wantRoles := owner.ID + "=" + string(tc.ownerRole)
			if tc.targetRole != "" {
				wantRoles += "," + target.ID + "=" + string(tc.targetRole)
			}
			wantEvents := 1
			if tc.kind != "" {
				wantEvents++
			}
			after := readBusinessState(t, env, accountID)
			if after.Roles != wantRoles || after.AuditEvents-before.AuditEvents != wantEvents {
				t.Fatalf("roles = %q, new audit rows = %d, want %q and %d", after.Roles, after.AuditEvents-before.AuditEvents, wantRoles, wantEvents)
			}

			type event struct {
				Kind         api.AuditKind
				Actor        string
				InvitationID string
				Details      map[string]string
				Time         time.Time
			}
			rows, err := env.DB.Pool.Query(ctx(), `SELECT kind, actor_user_id, COALESCE(invitation_id::text, ''), details, create_time
				FROM billing_audit_events WHERE billing_account_id = $1 AND target_user_id = $2
				ORDER BY kind`, accountID, target.ID)
			if err != nil {
				t.Fatal(err)
			}
			events, err := pgx.CollectRows(rows, pgx.RowToStructByPos[event])
			if err != nil {
				t.Fatal(err)
			}
			if len(events) != wantEvents {
				t.Fatalf("target audit rows = %+v, want %d rows", events, wantEvents)
			}
			acceptance := events[0]
			if acceptance.Kind != api.AuditInvitationAccepted || acceptance.Actor != target.ID ||
				acceptance.InvitationID != invID || !maps.Equal(acceptance.Details, map[string]string{"role": string(api.RoleViewer)}) ||
				!acceptance.Time.Equal(acceptedAt) {
				t.Fatalf("unexpected invitation acceptance audit row: %+v", acceptance)
			}
			if tc.kind == "" {
				return
			}
			mutation := events[1]
			if mutation.Kind != tc.kind || mutation.Actor != owner.ID || mutation.InvitationID != "" || !maps.Equal(mutation.Details, tc.details) {
				t.Fatalf("mutation audit row = %+v, want kind %s, actor %s, no invitation, and details %v", mutation, tc.kind, owner.ID, tc.details)
			}
			if !mutation.Time.After(acceptance.Time) {
				t.Fatalf("mutation audit time %s is not after acceptance audit time %s", mutation.Time.UTC(), acceptance.Time.UTC())
			}
			t.Logf("acceptance: %s, %s: %s", acceptance.Time.UTC(), mutation.Kind, mutation.Time.UTC())
		})
	}
}
