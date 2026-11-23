// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	"github.com/delinoio/mono/servers/accounts-api/internal/api"
	"github.com/delinoio/mono/servers/accounts-api/internal/testenv"
)

func revokeCall(p person, accountID, invitationID string) func() error {
	return func() error {
		_, err := p.Client.RevokeInvitation(ctx(), connect.NewRequest(&accountsv1.RevokeInvitationRequest{
			BillingAccountId: accountID, InvitationId: invitationID,
		}))
		return err
	}
}

// An invitation that reaches its expiry while the revocation waits for the
// billing account lock must be rejected: the pending check uses the time
// after the lock, so no revoke time or audit event is written.
func TestRevokeInvitationExpiresWhileWaitingForAccountLock(t *testing.T) {
	env := testenv.New(t)
	owner := newPerson(env, "owner")
	accountID := createAccount(t, owner, "Team").GetBillingAccountId()
	inv := invite(t, owner, accountID, "revoke-expiry@example.com", roleViewer)

	err := raceMembershipChange(t, env, accountID, func(pgx.Tx) {
		env.Clock.Advance(api.InvitationLifetime + time.Second)
	}, revokeCall(owner, accountID, inv.GetInvitationId()))
	wantCode(t, err, connect.CodeFailedPrecondition)

	var revoked *time.Time
	if err := env.DB.Pool.QueryRow(ctx(), `SELECT revoke_time FROM billing_account_invitations WHERE invitation_id = $1::uuid`,
		inv.GetInvitationId()).Scan(&revoked); err != nil {
		t.Fatal(err)
	}
	if revoked != nil {
		t.Fatalf("revoke_time = %v after a rejected revoke, want NULL", *revoked)
	}
	if n := auditRowCount(t, env, accountID, api.AuditInvitationRevoked); n != 0 {
		t.Fatalf("%d invitation_revoked audit rows after a rejected revoke, want 0", n)
	}
}

// A revocation that waits for the lock and still finds the invitation
// pending stores, audits and returns one time taken after the wait.
func TestRevokeInvitationAfterLockWaitUsesTimeAfterWait(t *testing.T) {
	env := testenv.New(t)
	owner := newPerson(env, "owner")
	accountID := createAccount(t, owner, "Team").GetBillingAccountId()
	inv := invite(t, owner, accountID, "revoke-pending@example.com", roleViewer)
	waited := inv.GetCreateTime().AsTime().Add(time.Hour)

	var resp *connect.Response[accountsv1.RevokeInvitationResponse]
	err := raceMembershipChange(t, env, accountID, func(pgx.Tx) {
		env.Clock.Advance(time.Hour)
	}, func() error {
		var err error
		resp, err = owner.Client.RevokeInvitation(ctx(), connect.NewRequest(&accountsv1.RevokeInvitationRequest{
			BillingAccountId: accountID, InvitationId: inv.GetInvitationId(),
		}))
		return err
	})
	wantCode(t, err, 0)
	if got := resp.Msg.GetInvitation().GetState(); got != accountsv1.InvitationState_INVITATION_STATE_REVOKED {
		t.Fatalf("response state = %s, want REVOKED", got)
	}

	var revoked time.Time
	if err := env.DB.Pool.QueryRow(ctx(), `SELECT revoke_time FROM billing_account_invitations WHERE invitation_id = $1::uuid`,
		inv.GetInvitationId()).Scan(&revoked); err != nil {
		t.Fatal(err)
	}
	if revoked.Before(waited) {
		t.Fatalf("revoke_time %v is before the end of the lock wait %v", revoked, waited)
	}
	var auditTime time.Time
	if err := env.DB.Pool.QueryRow(ctx(), `SELECT create_time FROM billing_audit_events
		WHERE billing_account_id = $1 AND kind = $2`, accountID, string(api.AuditInvitationRevoked)).Scan(&auditTime); err != nil {
		t.Fatal(err)
	}
	if !auditTime.Equal(revoked) {
		t.Fatalf("audit time %v differs from revoke_time %v", auditTime, revoked)
	}
}
