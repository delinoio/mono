// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	"github.com/delinoio/mono/servers/accounts-api/internal/api"
	"github.com/delinoio/mono/servers/accounts-api/internal/email"
	"github.com/delinoio/mono/servers/accounts-api/internal/testenv"
)

func invitationEmailCount(env *testenv.Env, addr string) int {
	n := 0
	for _, msg := range env.Email.Messages() {
		if msg.To == addr && msg.Kind == email.KindInvitation {
			n++
		}
	}
	return n
}

// A pending invitation that expires while CreateInvitation waits for the
// billing account lock no longer blocks the new invitation: the pending check,
// the stored times, the audit event, the email and the response all use the
// time after the wait.
func TestCreateInvitationReplacesInvitationThatExpiredWhileWaitingForAccountLock(t *testing.T) {
	env := testenv.New(t)
	owner := newPerson(env, "owner")
	accountID := createAccount(t, owner, "Team").GetBillingAccountId()
	const addr = "create-expiry@example.com"
	old := invite(t, owner, accountID, addr, roleViewer)
	if got := invitationEmailCount(env, addr); got != 1 {
		t.Fatalf("%d invitation emails after the first invitation, want 1", got)
	}

	var resp *connect.Response[accountsv1.CreateInvitationResponse]
	err := raceMembershipChange(t, env, accountID, func(pgx.Tx) {
		env.Clock.Advance(api.InvitationLifetime + time.Second)
	}, func() error {
		var err error
		resp, err = owner.Client.CreateInvitation(ctx(), connect.NewRequest(&accountsv1.CreateInvitationRequest{
			BillingAccountId: accountID, Email: addr, Role: roleViewer,
		}))
		return err
	})
	wantCode(t, err, 0)

	created := resp.Msg.GetInvitation()
	if created.GetInvitationId() == old.GetInvitationId() {
		t.Fatal("the replacement reused the expired invitation ID")
	}
	oldExpiry := old.GetExpireTime().AsTime()
	if !created.GetCreateTime().AsTime().After(oldExpiry) {
		t.Fatalf("create_time %v is not after the expired invitation's expiry %v", created.GetCreateTime().AsTime(), oldExpiry)
	}
	if got, want := created.GetExpireTime().AsTime(), created.GetCreateTime().AsTime().Add(api.InvitationLifetime); !got.Equal(want) {
		t.Fatalf("expire_time = %v, want create_time + lifetime = %v", got, want)
	}

	var createTime, expireTime time.Time
	if err := env.DB.Pool.QueryRow(ctx(), `SELECT create_time, expire_time FROM billing_account_invitations WHERE invitation_id = $1::uuid`,
		created.GetInvitationId()).Scan(&createTime, &expireTime); err != nil {
		t.Fatal(err)
	}
	if !createTime.Equal(created.GetCreateTime().AsTime()) || !expireTime.Equal(created.GetExpireTime().AsTime()) {
		t.Fatalf("stored times (%v, %v) differ from the response (%v, %v)",
			createTime, expireTime, created.GetCreateTime().AsTime(), created.GetExpireTime().AsTime())
	}
	var auditTime time.Time
	if err := env.DB.Pool.QueryRow(ctx(), `SELECT create_time FROM billing_audit_events
		WHERE billing_account_id = $1 AND kind = $2 AND invitation_id = $3::uuid`,
		accountID, string(api.AuditInvitationCreated), created.GetInvitationId()).Scan(&auditTime); err != nil {
		t.Fatal(err)
	}
	if !auditTime.Equal(createTime) {
		t.Fatalf("audit time %v differs from create_time %v", auditTime, createTime)
	}

	if got := invitationEmailCount(env, addr); got != 2 {
		t.Fatalf("%d invitation emails, want 2 (the first and the replacement)", got)
	}
	msgs := env.Email.Messages()
	last := msgs[len(msgs)-1]
	if last.To != addr || last.Kind != email.KindInvitation {
		t.Fatalf("last email is %s to %s, want the replacement invitation", last.Kind, last.To)
	}
	if want := "invitation/" + created.GetInvitationId(); last.IdempotencyKey != want {
		t.Fatalf("idempotency key = %q, want %q", last.IdempotencyKey, want)
	}
	// The email shows the expiry at minute precision, in UTC.
	wantExpiry := expireTime.UTC().Format("January 2, 2006 15:04 UTC")
	oldExpiryText := oldExpiry.UTC().Format("January 2, 2006 15:04 UTC")
	if !strings.Contains(last.Text, wantExpiry) {
		t.Fatalf("the replacement email does not show the new expiry %q", wantExpiry)
	}
	if strings.Contains(last.Text, oldExpiryText) {
		t.Fatalf("the replacement email shows the expired invitation's expiry %q", oldExpiryText)
	}
}

// A pending invitation that is still valid after the lock wait keeps blocking
// the new invitation: one row, one email.
func TestCreateInvitationStillConflictsWhenInvitationPendingAfterAccountLockWait(t *testing.T) {
	env := testenv.New(t)
	owner := newPerson(env, "owner")
	accountID := createAccount(t, owner, "Team").GetBillingAccountId()
	const addr = "create-pending@example.com"
	invite(t, owner, accountID, addr, roleViewer)

	err := raceMembershipChange(t, env, accountID, func(pgx.Tx) {
		env.Clock.Advance(api.InvitationLifetime - time.Hour)
	}, func() error {
		_, err := owner.Client.CreateInvitation(ctx(), connect.NewRequest(&accountsv1.CreateInvitationRequest{
			BillingAccountId: accountID, Email: addr, Role: roleViewer,
		}))
		return err
	})
	wantCode(t, err, connect.CodeAlreadyExists)

	var rows int
	if err := env.DB.Pool.QueryRow(ctx(), `SELECT count(*) FROM billing_account_invitations
		WHERE billing_account_id = $1 AND email_key = $2`, accountID, addr).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("%d invitation rows, want 1", rows)
	}
	if got := invitationEmailCount(env, addr); got != 1 {
		t.Fatalf("%d invitation emails, want 1", got)
	}
	if n := auditRowCount(t, env, accountID, api.AuditInvitationCreated); n != 1 {
		t.Fatalf("%d invitation_created audit rows, want 1", n)
	}
}
