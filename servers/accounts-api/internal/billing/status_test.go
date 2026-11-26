// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
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

// dueLinks links n organizations to n distinct billing accounts without an
// active subscription, so every link is BLOCKED at once, and returns the
// billing accounts and organizations in link order.
func (f *fixture) dueLinks(n int) (accounts, orgs []string) {
	f.t.Helper()
	for i := 0; i < n; i++ {
		acct := f.account()
		accounts = append(accounts, acct)
		orgs = append(orgs, f.link(acct, fmt.Sprintf("Org %d", i)))
	}
	return accounts, orgs
}

func (f *fixture) countStatus(status LinkStatus) int {
	f.t.Helper()
	return f.count(`SELECT count(*) FROM organization_links WHERE unlink_time IS NULL AND billing_status = $1`, string(status))
}

// captureLogs replaces the logger of the service and returns the buffer of
// its JSON records.
func (f *fixture) captureLogs() *bytes.Buffer {
	var buf bytes.Buffer
	f.s.logger = slog.New(slog.NewJSONHandler(&buf, nil))
	return &buf
}

func TestListStatusChangesSuspendsMoreThanOneBatchOfAccounts(t *testing.T) {
	// 101 accounts take two batches, 100 exactly one full batch and a second
	// empty query, and 250 three batches.
	for _, n := range []int{100, suspendBatchSize + 1, 250} {
		t.Run(fmt.Sprintf("%d accounts", n), func(t *testing.T) {
			f := newFixture(t)
			_, orgs := f.dueLinks(n)
			if got := f.countStatus(StatusBlocked); got != n {
				t.Fatalf("%d blocked links before the suspension time, want %d", got, n)
			}
			logs := f.captureLogs()
			f.clock.Advance(SuspendAfter)
			statuses, err := f.s.ListStatusChanges(ctx(), "ownsand", 0, 1000)
			if err != nil {
				t.Fatal(err)
			}
			if len(statuses) != n {
				t.Fatalf("ListStatusChanges returned %d statuses, want %d", len(statuses), n)
			}
			for _, st := range statuses {
				if st.Status != StatusSuspended {
					t.Fatalf("%s is %s after its suspension time %v", st.OrganizationID, st.Status, st.SuspendTime)
				}
			}
			if got := f.countStatus(StatusBlocked); got != 0 {
				t.Fatalf("%d links stay blocked", got)
			}
			for _, org := range orgs {
				f.wantStatus(org, StatusSuspended, ReasonNoActiveSubscription)
			}
			if want := fmt.Sprintf(`"accounts":%d`, n); !strings.Contains(logs.String(), `"msg":"billing_suspend_due"`) || !strings.Contains(logs.String(), want) {
				t.Fatalf("log does not record %d processed accounts:\n%s", n, logs)
			}
		})
	}
}

func TestSuspendDueFiltersStillSelectOnlyMatchingLinks(t *testing.T) {
	const (
		sandRunLinks = 5
		ownsandLinks = suspendBatchSize + 3
	)
	f := newFixture(t)
	accounts, orgs := f.dueLinks(ownsandLinks + sandRunLinks)

	// SuspendDue walks the accounts in ID order, so the five accounts with
	// the highest IDs get the SandRun links. The remaining ownsand links are
	// then more than one batch even after the account filter picked some.
	// The IDs are UUID v7 values, so creation order does not guarantee ID
	// order: sort the pairs explicitly.
	type pair struct{ account, org string }
	pairs := make([]pair, len(accounts))
	for i := range accounts {
		pairs[i] = pair{accounts[i], orgs[i]}
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].account < pairs[j].account })
	ownsand, sandrun := pairs[:ownsandLinks], pairs[ownsandLinks:]
	sandRunOrgs := make([]string, len(sandrun))
	for i, p := range sandrun {
		sandRunOrgs[i] = p.org
	}
	if _, err := f.db.Pool.Exec(ctx(), `UPDATE organization_links SET service = 'sandrun' WHERE organization_id = ANY($1)`, sandRunOrgs); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(SuspendAfter)

	// A billing account filter touches only the listed accounts: the two
	// lowest ownsand accounts and the lowest SandRun account.
	picked := []pair{ownsand[0], ownsand[1], sandrun[0]}
	pickedAccounts := make([]string, len(picked))
	for i, p := range picked {
		pickedAccounts[i] = p.account
	}
	if err := f.s.SuspendDue(ctx(), SuspendFilter{BillingAccountIDs: pickedAccounts}); err != nil {
		t.Fatal(err)
	}
	if got := f.countStatus(StatusSuspended); got != len(picked) {
		t.Fatalf("%d suspended links after the account filter, want %d", got, len(picked))
	}
	// The status helpers read the ownsand service; the SandRun link is checked
	// by the counts below.
	f.wantStatus(ownsand[0].org, StatusSuspended, ReasonNoActiveSubscription)
	f.wantStatus(ownsand[1].org, StatusSuspended, ReasonNoActiveSubscription)
	if got, want := f.count(`SELECT count(*) FROM organization_links WHERE service = 'ownsand' AND billing_status = 'suspended'`), 2; got != want {
		t.Fatalf("%d suspended ownsand links after the account filter, want %d", got, want)
	}
	if got, want := f.count(`SELECT count(*) FROM organization_links WHERE service = 'sandrun' AND billing_status = 'suspended'`), 1; got != want {
		t.Fatalf("%d suspended SandRun links after the account filter, want %d", got, want)
	}
	f.wantStatus(ownsand[2].org, StatusBlocked, ReasonNoActiveSubscription)

	// A service filter suspends the 101 remaining ownsand links, more than
	// one batch of suspendBatchSize accounts, and leaves the other SandRun
	// links blocked.
	logs := f.captureLogs()
	if err := f.s.SuspendDue(ctx(), SuspendFilter{Service: "ownsand"}); err != nil {
		t.Fatal(err)
	}
	if got, want := f.count(`SELECT count(*) FROM organization_links WHERE service = 'ownsand' AND billing_status = 'suspended'`), ownsandLinks; got != want {
		t.Fatalf("%d suspended ownsand links, want %d", got, want)
	}
	if got, want := f.count(`SELECT count(*) FROM organization_links WHERE service = 'ownsand' AND billing_status = 'blocked'`), 0; got != want {
		t.Fatalf("%d blocked ownsand links, want %d", got, want)
	}
	if got, want := f.count(`SELECT count(*) FROM organization_links WHERE service = 'sandrun' AND billing_status = 'suspended'`), 1; got != want {
		t.Fatalf("%d suspended SandRun links, want %d (only the one picked by the account filter)", got, want)
	}
	if got, want := f.count(`SELECT count(*) FROM organization_links WHERE service = 'sandrun' AND billing_status = 'blocked'`), sandRunLinks-1; got != want {
		t.Fatalf("%d blocked SandRun links, want %d", got, want)
	}
	if got := f.countStatus(StatusBlocked); got != sandRunLinks-1 {
		t.Fatalf("%d blocked links, want %d", got, sandRunLinks-1)
	}
	// The 101 accounts take a full batch and a second one with one account.
	for _, want := range []string{`"msg":"billing_suspend_due"`, fmt.Sprintf(`"accounts":%d`, ownsandLinks-2), `"batches":2`} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("log does not contain %s:\n%s", want, logs)
		}
	}
}

func TestSuspendDueWithoutDueLinksChangesNothing(t *testing.T) {
	f := newFixture(t)
	f.dueLinks(3)
	// The links are blocked but not due yet.
	logs := f.captureLogs()
	before := f.count(`SELECT COALESCE(sum(last_sequence), 0)::bigint FROM billing_status_sequences`)
	f.clock.Advance(SuspendAfter - time.Microsecond)
	if err := f.s.SuspendDue(ctx(), SuspendFilter{}); err != nil {
		t.Fatal(err)
	}
	if after := f.count(`SELECT COALESCE(sum(last_sequence), 0)::bigint FROM billing_status_sequences`); after != before {
		t.Fatalf("status sequences changed from %d to %d without a due link", before, after)
	}
	if logs.Len() != 0 {
		t.Fatalf("SuspendDue logged without a due link:\n%s", logs)
	}
	if got := f.countStatus(StatusBlocked); got != 3 {
		t.Fatalf("%d blocked links, want 3", got)
	}
}
