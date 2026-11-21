// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/delinoio/mono/go/uuidv7"
	"github.com/delinoio/mono/servers/accounts-api/internal/email"
	"github.com/delinoio/mono/servers/accounts-api/internal/paddle"
	"github.com/delinoio/mono/servers/accounts-api/internal/paddle/paddletest"
)

// link links an OwnSand organization to the billing account now, as
// LinkOrganization does, and returns the organization ID.
func (f *fixture) link(accountID, name string) string {
	f.t.Helper()
	org, err := uuidv7.NewString()
	if err != nil {
		f.t.Fatal(err)
	}
	linkID, _ := uuidv7.NewString()
	f.tx(func(tx pgx.Tx) error {
		if err := LockAccount(ctx(), tx, accountID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx(), `INSERT INTO organization_links (organization_link_id, service, organization_id,
				billing_account_id, organization_name, link_time) VALUES ($1, 'ownsand', $2, $3, $4, $5)`,
			linkID, org, accountID, name, f.s.timestamp()); err != nil {
			return err
		}
		return f.s.SyncStatuses(ctx(), tx, accountID)
	})
	return org
}

func (f *fixture) status(org string) OrganizationStatus {
	f.t.Helper()
	st, ok, err := CurrentStatus(ctx(), f.db.Pool, "ownsand", org)
	if err != nil || !ok {
		f.t.Fatalf("CurrentStatus(%s) = %v, %v", org, ok, err)
	}
	return st
}

// credit adds credit with a verified one-time charge webhook.
func (f *fixture) credit(accountID string, cents int64) {
	f.t.Helper()
	if code := f.webhook(paddle.EventTransactionCompleted, paddletest.Transaction(paddletest.TransactionData{
		ID: f.paddle.NewID("txn"), Origin: paddle.OriginSubscriptionCharge, BillingAccountID: accountID, SubtotalCents: cents,
	})); code != http.StatusOK {
		f.t.Fatalf("credit webhook status %d", code)
	}
}

func (f *fixture) wantStatus(org string, status LinkStatus, reason StatusReason) OrganizationStatus {
	f.t.Helper()
	st := f.status(org)
	if st.Status != status || st.Reason != reason {
		f.t.Fatalf("status of %s = %s/%s, want %s/%s", org, st.Status, st.Reason, status, reason)
	}
	if (status == StatusAllowed) != (st.BlockedSince == nil && st.SuspendTime == nil) {
		f.t.Fatalf("status %s with blocked_since %v and suspend_time %v", status, st.BlockedSince, st.SuspendTime)
	}
	if st.BlockedSince != nil && !st.SuspendTime.Equal(st.BlockedSince.Add(SuspendAfter)) {
		f.t.Fatalf("suspend_time %v is not blocked_since %v plus 3 days", st.SuspendTime, st.BlockedSince)
	}
	return st
}

// orgEmails returns the organization status emails as `kind>address`.
func (f *fixture) orgEmails() []string {
	var out []string
	for _, m := range f.email.Messages() {
		if m.Kind == email.KindOrganizationBlocked || m.Kind == email.KindOrganizationSuspended {
			out = append(out, string(m.Kind)+">"+m.To)
		}
	}
	return out
}

func TestStatusStateMachine(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	f.activate(acct)
	f.credit(acct, 1000)
	org := f.link(acct, "Acme <Sandbox>")
	allowed := f.wantStatus(org, StatusAllowed, ReasonNone)
	if allowed.Sequence <= 0 {
		t.Fatalf("sequence %d", allowed.Sequence)
	}

	// Usage that takes the balance to zero blocks the organization at once.
	f.clock.Advance(time.Hour)
	blockedAt := f.s.timestamp()
	f.deduct(acct, 10*usd)
	blocked := f.wantStatus(org, StatusBlocked, ReasonNonPositiveBalance)
	if !blocked.BlockedSince.Equal(blockedAt) || blocked.Sequence <= allowed.Sequence {
		t.Fatalf("blocked = %+v, want blocked_since %v and a higher sequence", blocked, blockedAt)
	}
	// More usage changes nothing: the block continues.
	f.deduct(acct, usd)
	if st := f.status(org); st.Sequence != blocked.Sequence {
		t.Fatalf("sequence changed without a status change: %d -> %d", blocked.Sequence, st.Sequence)
	}

	// The suspension follows exactly 3 days after the block started.
	f.clock.Set(blockedAt.Add(SuspendAfter - time.Microsecond))
	f.s.RunOnce(ctx())
	f.wantStatus(org, StatusBlocked, ReasonNonPositiveBalance)
	f.clock.Set(blockedAt.Add(SuspendAfter))
	f.s.RunOnce(ctx())
	suspended := f.wantStatus(org, StatusSuspended, ReasonNonPositiveBalance)
	if !suspended.BlockedSince.Equal(blockedAt) || !suspended.SuspendTime.Equal(blockedAt.Add(SuspendAfter)) ||
		suspended.Sequence <= blocked.Sequence {
		t.Fatalf("suspended = %+v", suspended)
	}
	f.s.RunOnce(ctx())
	if st := f.status(org); st.Sequence != suspended.Sequence {
		t.Fatalf("a second worker run changed the sequence: %d -> %d", suspended.Sequence, st.Sequence)
	}

	// The owner and the admin got each email once; the viewer got none.
	kinds := strings.Join(f.orgEmails(), " ")
	n := accountSeq.Load()
	want := fmt.Sprintf("organization_blocked>admin-%d@example.com organization_blocked>owner-%d@example.com "+
		"organization_suspended>admin-%d@example.com organization_suspended>owner-%d@example.com", n, n, n, n)
	if kinds != want {
		t.Fatalf("emails = %s, want %s", kinds, want)
	}
	var msgs []email.Message
	for _, m := range f.email.Messages() {
		if m.Kind == email.KindOrganizationBlocked {
			msgs = append(msgs, m)
		}
	}
	if !strings.Contains(msgs[0].Text, `"Acme <Sandbox>"`) || !strings.Contains(msgs[0].Text, blockedAt.Add(SuspendAfter).Format("January 2, 2006 15:04 UTC")) ||
		!strings.Contains(msgs[0].HTML, "Acme &lt;Sandbox&gt;") {
		t.Fatalf("blocked email does not name the organization and the suspension time:\n%s\n%s", msgs[0].Text, msgs[0].HTML)
	}

	// Credit while the subscription is ACTIVE resumes the organization.
	f.credit(acct, 2500)
	resumed := f.wantStatus(org, StatusAllowed, ReasonNone)
	if resumed.Sequence <= suspended.Sequence {
		t.Fatalf("resumed sequence %d", resumed.Sequence)
	}
	var since *time.Time
	if err := f.db.Pool.QueryRow(ctx(), `SELECT nonpayment_since FROM billing_accounts WHERE billing_account_id = $1`, acct).Scan(&since); err != nil || since != nil {
		t.Fatalf("nonpayment_since = %v, %v", since, err)
	}
}

func TestStatusSubscriptionAndReasons(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	// Without a subscription the organization is blocked from the link on,
	// with NO_ACTIVE_SUBSCRIPTION, also with a positive balance.
	f.credit(acct, 1000)
	f.clock.Advance(time.Hour)
	linkedAt := f.s.timestamp()
	org := f.link(acct, "")
	st := f.wantStatus(org, StatusBlocked, ReasonNoActiveSubscription)
	if !st.BlockedSince.Equal(linkedAt) {
		t.Fatalf("blocked_since %v, want the link time %v", st.BlockedSince, linkedAt)
	}

	// The subscription payment makes it ALLOWED.
	f.clock.Advance(time.Hour)
	txnID := f.checkout(acct)
	subID := f.paddle.NewID("sub")
	f.webhook(paddle.EventTransactionCompleted, paddletest.Transaction(paddletest.TransactionData{
		ID: txnID, SubscriptionID: subID, Origin: paddle.OriginWeb, PriceID: priceID, SubtotalCents: 1000,
	}))
	f.wantStatus(org, StatusAllowed, ReasonNone)

	// A canceled subscription blocks it; with a balance of zero as well the
	// reason stays NO_ACTIVE_SUBSCRIPTION.
	f.clock.Advance(time.Hour)
	canceledAt := f.s.timestamp()
	f.webhook("subscription.canceled", map[string]any{"id": subID, "status": "canceled", "updated_at": f.clock.Now()})
	st = f.wantStatus(org, StatusBlocked, ReasonNoActiveSubscription)
	if !st.BlockedSince.Equal(canceledAt) {
		t.Fatalf("blocked_since %v, want %v", st.BlockedSince, canceledAt)
	}
	f.clock.Advance(time.Hour)
	f.deduct(acct, 30*usd)
	after := f.wantStatus(org, StatusBlocked, ReasonNoActiveSubscription)
	if after.Sequence != st.Sequence {
		t.Fatal("the balance changed the status although the subscription reason wins")
	}

	// A second organization linked later is blocked from its own link time,
	// while the first keeps the start of the non-payment.
	f.clock.Advance(24 * time.Hour)
	laterAt := f.s.timestamp()
	org2 := f.link(acct, "Second")
	st2 := f.wantStatus(org2, StatusBlocked, ReasonNoActiveSubscription)
	if !st2.BlockedSince.Equal(laterAt) || !f.status(org).BlockedSince.Equal(canceledAt) {
		t.Fatalf("blocked_since %v and %v", st2.BlockedSince, f.status(org).BlockedSince)
	}
	// Each status has its own sequence.
	if st2.Sequence <= after.Sequence {
		t.Fatalf("sequences %d, %d", after.Sequence, st2.Sequence)
	}

	// An active subscription with a debt keeps the block, now with
	// NON_POSITIVE_BALANCE, and the block start is unchanged.
	f.activate(acct)
	if err := f.s.SyncAccount(ctx(), acct); err != nil {
		t.Fatal(err)
	}
	st = f.wantStatus(org, StatusBlocked, ReasonNonPositiveBalance)
	if !st.BlockedSince.Equal(canceledAt) {
		t.Fatalf("blocked_since %v moved", st.BlockedSince)
	}
}

func TestStatusEmailsAreDeduplicated(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	org := f.link(acct, "Org")
	f.wantStatus(org, StatusBlocked, ReasonNoActiveSubscription)
	for range 3 {
		if err := f.s.SyncAccount(ctx(), acct); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := f.db.Pool.QueryRow(ctx(), `SELECT count(*) FROM billing_notifications WHERE billing_account_id = $1
		AND kind = $2`, acct, string(NotifyOrganizationBlocked)).Scan(&n); err != nil || n != 1 {
		t.Fatalf("blocked notifications = %d, %v", n, err)
	}
	if err := f.s.DeliverNotifications(ctx()); err != nil {
		t.Fatal(err)
	}
	for _, m := range f.email.Messages() {
		if m.Kind != email.KindOrganizationBlocked || !strings.Contains(m.Text, "no active subscription") {
			t.Fatalf("unexpected email %s: %s", m.Kind, m.Text)
		}
	}
	if len(f.email.Messages()) != 2 {
		t.Fatalf("%d emails, want one to the owner and one to the admin", len(f.email.Messages()))
	}
}

func TestStatusIgnoresUnlinkedOrganizations(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	f.activate(acct)
	f.credit(acct, 1000)
	org := f.link(acct, "")
	if _, err := f.db.Pool.Exec(ctx(), `UPDATE organization_links SET unlink_time = link_time WHERE organization_id = $1`, org); err != nil {
		t.Fatal(err)
	}
	f.deduct(acct, 20*usd)
	if _, ok, err := CurrentStatus(ctx(), f.db.Pool, "ownsand", org); ok || err != nil {
		t.Fatalf("an unlinked organization has a status: %v, %v", ok, err)
	}
	var status string
	if err := f.db.Pool.QueryRow(ctx(), `SELECT billing_status FROM organization_links WHERE organization_id = $1`, org).Scan(&status); err != nil || status != "allowed" {
		t.Fatalf("the unlinked row changed: %s, %v", status, err)
	}
}
