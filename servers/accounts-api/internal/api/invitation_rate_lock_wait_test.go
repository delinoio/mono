// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	"github.com/delinoio/mono/servers/accounts-api/internal/api"
	"github.com/delinoio/mono/servers/accounts-api/internal/testenv"
)

func TestCreateInvitationUsesTimeAfterRateLimitLocks(t *testing.T) {
	for _, limit := range []struct {
		name   string
		prefix string
		count  int
	}{
		{"inviter", "invitation_rate_inviter:", rateInviterLimit},
		{"recipient", "invitation_rate_email:", rateEmailLimit},
	} {
		t.Run(limit.name, func(t *testing.T) {
			for _, scenario := range []struct {
				name     string
				rollover bool
			}{
				{"rollover", true},
				{"still full", false},
			} {
				t.Run(scenario.name, func(t *testing.T) {
					env := testenv.New(t)
					owner := newPerson(env, "owner")
					accountID := createAccount(t, owner, "Team").GetBillingAccountId()
					const addr = "guest@example.com"

					seedOwner, lockKey := owner, owner.ID
					seedAddr := func(i int) string {
						return fmt.Sprintf("guest-%d@example.com", i)
					}
					if limit.name == "recipient" {
						seedOwner, lockKey = newPerson(env, "seed-owner"), addr
						seedAddr = func(int) string { return addr }
					}
					seedAccountID := createAccount(t, seedOwner, "Seed").GetBillingAccountId()
					oldest := invite(t, seedOwner, seedAccountID, seedAddr(0), roleViewer)
					revokeInvitation(t, seedOwner, seedAccountID, oldest.GetInvitationId())
					env.Clock.Advance(time.Minute)
					for i := 1; i < limit.count; i++ {
						inviteAndRevoke(t, seedOwner, seedAccountID, seedAddr(i))
					}

					// Advance to two minutes before the oldest invitation ages out.
					// The fake clock advances one millisecond on each read.
					boundary := oldest.GetCreateTime().AsTime().Add(24 * time.Hour)
					beforeWait := boundary.Add(-2 * time.Minute)
					env.Clock.Advance(beforeWait.Sub(env.Clock.Now().Add(time.Millisecond)))

					var created *accountsv1.BillingAccountInvitation
					err := raceLockWait(t, env, limit.name+" advisory lock", func(tx pgx.Tx) {
						execTx(t, tx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, limit.prefix+lockKey)
					}, func(pgx.Tx) {
						if scenario.rollover {
							env.Clock.Advance(2 * time.Minute)
						}
					}, func() error {
						var err error
						created, err = createInvitation(owner, accountID, addr)
						return err
					})
					if !scenario.rollover {
						wantRateLimited(t, err)
						assertInvitationCounts(t, env, accountID, limit.count, 0, limit.count)
						return
					}

					wantCode(t, err, 0)
					assertInvitationCounts(t, env, accountID, limit.count+1, 1, limit.count+1)
					assertInvitationCreationTimes(t, env, created, boundary)
				})
			}
		})
	}
}

func TestCreateInvitationKeepsPendingDuplicateErrorAtRecipientLimit(t *testing.T) {
	env := testenv.New(t)
	owner := newPerson(env, "owner")
	accountID := createAccount(t, owner, "Team").GetBillingAccountId()
	other := newPerson(env, "other")
	otherAccountID := createAccount(t, other, "Other").GetBillingAccountId()
	const addr = "guest@example.com"
	for i := 0; i < rateEmailLimit-1; i++ {
		inviteAndRevoke(t, other, otherAccountID, addr)
	}
	invite(t, owner, accountID, addr, roleViewer)

	_, err := createInvitation(owner, accountID, addr)
	wantCode(t, err, connect.CodeAlreadyExists)
	assertInvitationCounts(t, env, accountID, rateEmailLimit, 1, rateEmailLimit)
}

func TestCreateInvitationReplacesInvitationThatExpiredWhileWaitingForRecipientLock(t *testing.T) {
	env := testenv.New(t)
	owner := newPerson(env, "owner")
	accountID := createAccount(t, owner, "Team").GetBillingAccountId()
	const addr = "guest@example.com"
	old := invite(t, owner, accountID, addr, roleViewer)
	boundary := old.GetExpireTime().AsTime()
	beforeWait := boundary.Add(-time.Minute)
	env.Clock.Advance(beforeWait.Sub(env.Clock.Now().Add(time.Millisecond)))

	var created *accountsv1.BillingAccountInvitation
	err := raceLockWait(t, env, "recipient advisory lock", func(tx pgx.Tx) {
		execTx(t, tx, `SELECT pg_advisory_xact_lock(hashtextextended('invitation_rate_email:' || $1::text, 0))`, addr)
	}, func(pgx.Tx) {
		env.Clock.Advance(time.Minute)
	}, func() error {
		var err error
		created, err = createInvitation(owner, accountID, addr)
		return err
	})
	wantCode(t, err, 0)
	assertInvitationCounts(t, env, accountID, 2, 2, 2)
	assertInvitationCreationTimes(t, env, created, boundary)
}

func assertInvitationCounts(t *testing.T, env *testenv.Env, accountID string, invitations, audits, emails int) {
	t.Helper()
	if got := totalInvitationRows(t, env); got != invitations {
		t.Errorf("invitation rows = %d, want %d", got, invitations)
	}
	if got := auditRowCount(t, env, accountID, api.AuditInvitationCreated); got != audits {
		t.Errorf("creation audit rows = %d, want %d", got, audits)
	}
	if got := len(env.Email.Messages()); got != emails {
		t.Errorf("emails = %d, want %d", got, emails)
	}
}

func assertInvitationCreationTimes(t *testing.T, env *testenv.Env, created *accountsv1.BillingAccountInvitation, wantCreate time.Time) {
	t.Helper()
	wantExpiry := wantCreate.Add(7 * 24 * time.Hour)
	if got := created.GetCreateTime().AsTime(); !got.Equal(wantCreate) {
		t.Errorf("response create_time = %v, want %v", got, wantCreate)
	}
	if got := created.GetExpireTime().AsTime(); !got.Equal(wantExpiry) {
		t.Errorf("response expire_time = %v, want %v", got, wantExpiry)
	}
	var createTime, expireTime, auditTime time.Time
	if err := env.DB.Pool.QueryRow(ctx(), `SELECT i.create_time, i.expire_time, a.create_time
		FROM billing_account_invitations i JOIN billing_audit_events a
		ON a.billing_account_id = i.billing_account_id AND a.invitation_id = i.invitation_id
		WHERE i.invitation_id = $1 AND a.kind = $2`,
		created.GetInvitationId(), string(api.AuditInvitationCreated)).Scan(&createTime, &expireTime, &auditTime); err != nil {
		t.Fatal(err)
	}
	if !createTime.Equal(wantCreate) || !expireTime.Equal(wantExpiry) || !auditTime.Equal(wantCreate) {
		t.Errorf("stored creation, expiry and audit times = (%v, %v, %v), want (%v, %v, %v)",
			createTime, expireTime, auditTime, wantCreate, wantExpiry, wantCreate)
	}
	msgs := env.Email.Messages()
	msg := msgs[len(msgs)-1]
	expiryText := wantExpiry.UTC().Format("January 2, 2006 15:04 UTC")
	if msg.IdempotencyKey != "invitation/"+created.GetInvitationId() || !strings.Contains(msg.Text, expiryText) {
		t.Errorf("want the new invitation's email with expiry %q", expiryText)
	}
}
