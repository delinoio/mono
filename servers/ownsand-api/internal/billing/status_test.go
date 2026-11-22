// SPDX-License-Identifier: FSL-1.1-ALv2

package billing_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/attribute"

	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/accounts/accountstest"
	"github.com/delinoio/mono/servers/ownsand-api/internal/billing"
)

const (
	accountA = "0190f3a4-0000-7000-8000-0000000000a1"
	accountB = "0190f3a4-0000-7000-8000-0000000000b1"
	owner    = "user-owner"
)

// TestStatusPhases checks the phase rules of the cache without a database:
// the 3-day suspension of a block, the 1-hour fail open of a linked
// organization and the reported fields.
func TestStatusPhases(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	unlinked := billing.Status{OrganizationID: org1, Phase: billing.PhaseBlocked, BlockedSince: &t0}
	if got := unlinked.Enforced(t0.Add(72*time.Hour - time.Second)); got != billing.PhaseBlocked {
		t.Fatalf("before 3 days: %v", got)
	}
	if got := unlinked.Enforced(t0.Add(72 * time.Hour)); got != billing.PhaseSuspended {
		t.Fatalf("after 3 days: %v", got)
	}
	// An organization without a link never fails open.
	if unlinked.Stale(t0.Add(48 * time.Hour)) {
		t.Fatal("an unlinked status is stale")
	}
	b := unlinked.Proto(t0)
	if b.GetPhase() != billing.PhaseBlocked || b.GetReason() != ownsandv1.BillingReason_BILLING_REASON_ACCOUNT_REQUIRED || b.GetLinked() ||
		!b.GetBlockedSince().AsTime().Equal(t0) || !b.GetSuspendTime().AsTime().Equal(t0.Add(72*time.Hour)) {
		t.Fatalf("unlinked proto = %v", b)
	}
	if got := unlinked.Proto(t0.Add(73 * time.Hour)).GetReason(); got != ownsandv1.BillingReason_BILLING_REASON_SUSPENDED {
		t.Fatalf("suspended reason = %v", got)
	}

	sync := t0
	linked := billing.Status{OrganizationID: org1, BillingAccountID: accountA, Phase: billing.PhaseBlocked, BlockedSince: &t0, SyncTime: &sync}
	if b := linked.Proto(t0.Add(time.Hour)); b.GetPhase() != billing.PhaseBlocked || b.GetReason() != ownsandv1.BillingReason_BILLING_REASON_BLOCKED || !b.GetLinked() {
		t.Fatalf("linked proto = %v", b)
	}
	// More than 1 hour after the last sync, the gates fail open, and the
	// status reports ALLOWED without times; the confirmed phase stays.
	late := t0.Add(time.Hour + time.Second)
	if !linked.Stale(late) || linked.Enforced(late) != billing.PhaseAllowed || linked.Confirmed(late) != billing.PhaseBlocked {
		t.Fatalf("stale: stale=%v enforced=%v confirmed=%v", linked.Stale(late), linked.Enforced(late), linked.Confirmed(late))
	}
	if b := linked.Proto(late); b.GetPhase() != billing.PhaseAllowed || b.GetReason() != ownsandv1.BillingReason_BILLING_REASON_UNSPECIFIED ||
		b.GetBlockedSince() != nil || b.GetSuspendTime() != nil {
		t.Fatalf("fail-open proto = %v", b)
	}
	allowed := billing.Status{OrganizationID: org1, BillingAccountID: accountA, Phase: billing.PhaseAllowed, SyncTime: &sync}
	if b := allowed.Proto(t0); b.GetPhase() != billing.PhaseAllowed || b.GetBlockedSince() != nil || b.GetReason() != ownsandv1.BillingReason_BILLING_REASON_UNSPECIFIED {
		t.Fatalf("allowed proto = %v", b)
	}
}

// statusFixture is a database with organizations, a fake delino-accounts
// and a status cache on a fake clock.
type statusFixture struct {
	*fixture
	fake     *accountstest.Server
	statuses *billing.Statuses
	registry *testMeter
}

func newStatusFixture(t *testing.T, start time.Time) *statusFixture {
	f := &statusFixture{fixture: newFixture(t, start)}
	f.fake = accountstest.New(t, f.clock.Now)
	f.fake.AddAccount(accountA, "Account A", map[string]accountsv1.BillingAccountRole{owner: accountsv1.BillingAccountRole_BILLING_ACCOUNT_ROLE_OWNER})
	f.fake.AddAccount(accountB, "Account B", map[string]accountsv1.BillingAccountRole{owner: accountsv1.BillingAccountRole_BILLING_ACCOUNT_ROLE_ADMIN})
	f.registry = newTestMeter(t)
	f.statuses = billing.NewStatuses(billing.StatusOptions{DB: f.db, Logger: f.logger, Now: f.clock.Now, Client: f.fake.Client(), Metrics: f.registry})
	return f
}

// addOrg creates an organization and its status row, as CreateOrganization
// does, at the current fake time.
func (f *statusFixture) addOrg(t *testing.T, org string) {
	t.Helper()
	ctx := context.Background()
	now := f.clock.Now()
	err := f.db.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "INSERT INTO organizations (organization_id, name, create_time) VALUES ($1, 'org', $2)", org, now); err != nil {
			return err
		}
		return billing.InsertStatus(ctx, tx, org, now)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// link links the organization like LinkBillingAccount does.
func (f *statusFixture) link(t *testing.T, org, account string) billing.Status {
	t.Helper()
	ctx := context.Background()
	resp, err := f.fake.Client().LinkOrganization(ctx, connect.NewRequest(&accountsv1.LinkOrganizationRequest{
		Service: billing.ServiceID, OrganizationId: org, BillingAccountId: account, UserId: owner, OrganizationName: "org",
	}))
	if err != nil {
		t.Fatal(err)
	}
	var st billing.Status
	var change billing.Change
	if err := f.db.InTx(ctx, func(tx pgx.Tx) error {
		var err error
		st, change, err = billing.LinkTx(ctx, tx, org, account, resp.Msg.GetStatus(), f.clock.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f.statuses.Committed(ctx, change)
	return st
}

func (f *statusFixture) load(t *testing.T, org string) billing.Status {
	t.Helper()
	st, ok, err := billing.LoadStatus(context.Background(), f.db.Pool, org, false)
	if err != nil || !ok {
		t.Fatalf("load status: %v %v", ok, err)
	}
	return st
}

// TestStatusPollAndLinks follows one organization through the status
// sources: a new organization is blocked, a link to an account in good
// standing allows it, polled changes block and suspend it, and an unlink
// keeps the block start of the non-payment.
func TestStatusPollAndLinks(t *testing.T) {
	t0 := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	f := newStatusFixture(t, t0)
	ctx := context.Background()
	f.addOrg(t, org1)
	st := f.load(t, org1)
	if st.Linked() || st.Confirmed(t0) != billing.PhaseBlocked || !st.BlockedSince.Equal(t0) {
		t.Fatalf("new organization = %+v", st)
	}

	f.clock.Advance(time.Hour)
	st = f.link(t, org1, accountA)
	if st.BillingAccountID != accountA || st.Phase != billing.PhaseAllowed || st.BlockedSince != nil || st.SyncTime == nil {
		t.Fatalf("after link = %+v", st)
	}
	if !strings.Contains(f.logText(), `"msg":"billing_status_changed"`) || !strings.Contains(f.logText(), `"source":"link"`) {
		t.Fatalf("link not logged: %s", f.logText())
	}

	// The account runs out of credit; the poll applies the change.
	blockStart := f.clock.Now()
	f.fake.SetAccountStatus(accountA, accountsv1.BillingStatus_BILLING_STATUS_BLOCKED, blockStart)
	if err := f.statuses.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	st = f.load(t, org1)
	if st.Phase != billing.PhaseBlocked || !st.BlockedSince.Equal(blockStart) || st.Sequence != f.fake.Status(org1).GetSequence() {
		t.Fatalf("after poll = %+v", st)
	}
	var cursor int64
	if err := f.db.Pool.QueryRow(ctx, "SELECT after_sequence FROM billing_status_cursor").Scan(&cursor); err != nil || cursor != st.Sequence {
		t.Fatalf("cursor = %d, %v", cursor, err)
	}

	// A poll without changes still refreshes the sync time, so the status
	// does not become stale.
	f.clock.Advance(50 * time.Minute)
	if err := f.statuses.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(50 * time.Minute)
	if st = f.load(t, org1); st.Stale(f.clock.Now()) {
		t.Fatalf("stale after an empty poll: %+v", st)
	}
	// Without a poll for more than an hour, it is stale.
	f.clock.Advance(11 * time.Minute)
	if st = f.load(t, org1); !st.Stale(f.clock.Now()) {
		t.Fatalf("not stale: %+v", st)
	}

	// A status of another billing account (one that arrives after a change
	// of the link) and an older sequence are ignored.
	for _, r := range []*accountsv1.OrganizationBillingStatus{
		{Service: billing.ServiceID, OrganizationId: org1, BillingAccountId: accountB, Status: accountsv1.BillingStatus_BILLING_STATUS_ALLOWED, Sequence: st.Sequence + 10},
		{Service: billing.ServiceID, OrganizationId: org1, BillingAccountId: accountA, Status: accountsv1.BillingStatus_BILLING_STATUS_ALLOWED, Sequence: st.Sequence - 1},
	} {
		f.statuses.ApplyReportStatuses(ctx, []*accountsv1.OrganizationBillingStatus{r})
		if got := f.load(t, org1); got.Phase != billing.PhaseBlocked {
			t.Fatalf("status %v was applied: %+v", r, got)
		}
	}

	// accounts-api suspends the organization 3 days after the block start.
	f.clock.Set(blockStart.Add(72 * time.Hour))
	f.fake.SetAccountStatus(accountA, accountsv1.BillingStatus_BILLING_STATUS_SUSPENDED, blockStart)
	if err := f.statuses.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if st = f.load(t, org1); st.Phase != billing.PhaseSuspended || !st.BlockedSince.Equal(blockStart) {
		t.Fatalf("suspended = %+v", st)
	}

	// An unlink in non-payment keeps the block start.
	var former string
	if err := f.db.InTx(ctx, func(tx pgx.Tx) error {
		var err error
		st, former, _, err = billing.UnlinkTx(ctx, tx, org1, billing.SourceUnlink, f.clock.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if former != accountA || st.Linked() || !st.BlockedSince.Equal(blockStart) || st.Confirmed(f.clock.Now()) != billing.PhaseSuspended ||
		st.Proto(f.clock.Now()).GetReason() != ownsandv1.BillingReason_BILLING_REASON_SUSPENDED {
		t.Fatalf("unlinked = %+v (former %q)", st, former)
	}
	// A late status of the former link is ignored.
	f.statuses.ApplyReportStatuses(ctx, []*accountsv1.OrganizationBillingStatus{{Service: billing.ServiceID, OrganizationId: org1, BillingAccountId: accountA,
		Status: accountsv1.BillingStatus_BILLING_STATUS_ALLOWED, Sequence: st.Sequence + 100}})
	if got := f.load(t, org1); got.Linked() || got.Phase != billing.PhaseSuspended {
		t.Fatalf("late status applied: %+v", got)
	}
}

// TestLinkKeepsEarlierBlockStart checks that linking an organization in
// non-payment to a billing account in non-payment keeps the earlier block
// start, and that an unlink of an allowed organization starts a new block.
func TestLinkKeepsEarlierBlockStart(t *testing.T) {
	t0 := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	f := newStatusFixture(t, t0)
	ctx := context.Background()
	f.addOrg(t, org1)
	f.fake.SetAccountStatus(accountB, accountsv1.BillingStatus_BILLING_STATUS_BLOCKED, t0.Add(-24*time.Hour))
	f.clock.Advance(2 * 24 * time.Hour)
	// accounts-api reports the link time as the block start; the
	// organization was blocked since t0.
	st := f.link(t, org1, accountB)
	if !st.BlockedSince.Equal(t0) || st.Phase != billing.PhaseBlocked {
		t.Fatalf("linked in non-payment = %+v", st)
	}
	f.clock.Set(t0.Add(72 * time.Hour))
	if got := st.Confirmed(f.clock.Now()); got != billing.PhaseSuspended {
		t.Fatalf("3 days after the first block start: %v", got)
	}
	// Advance records the suspension that time caused.
	if err := f.statuses.Advance(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.load(t, org1); got.Phase != billing.PhaseSuspended {
		t.Fatalf("stored phase = %v", got.Phase)
	}
	if !strings.Contains(f.logText(), `"source":"time"`) {
		t.Fatalf("time-based suspension not logged")
	}

	// Recovery and a new unlink: the block starts at the unlink.
	f.fake.SetAccountStatus(accountB, accountsv1.BillingStatus_BILLING_STATUS_ALLOWED, time.Time{})
	if err := f.statuses.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.load(t, org1); got.Phase != billing.PhaseAllowed {
		t.Fatalf("recovered = %+v", got)
	}
	f.clock.Advance(time.Hour)
	unlinkAt := f.clock.Now()
	var st2 billing.Status
	if err := f.db.InTx(ctx, func(tx pgx.Tx) error {
		var err error
		st2, _, _, err = billing.UnlinkTx(ctx, tx, org1, billing.SourceTransfer, unlinkAt)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !st2.BlockedSince.Equal(unlinkAt) || st2.Confirmed(unlinkAt) != billing.PhaseBlocked {
		t.Fatalf("unlinked from ALLOWED = %+v", st2)
	}
}

// removeLink removes the link of the organization like removeBillingLinkTx
// does for TransferOwnership and DeleteOrganization, and commits the change
// through Committed. It returns the former billing account ID.
func (f *statusFixture) removeLink(t *testing.T, org, source string) string {
	t.Helper()
	ctx := context.Background()
	var former string
	var change billing.Change
	if err := f.db.InTx(ctx, func(tx pgx.Tx) error {
		var err error
		_, former, change, err = billing.UnlinkTx(ctx, tx, org, source, f.clock.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f.statuses.Committed(ctx, change)
	return former
}

// TestLinkRemovalIsLogged checks that every removal of a link writes one
// billing_status_changed record and runs the change hooks once, also when
// the phase stays BLOCKED or SUSPENDED, and that an organization without a
// link logs nothing and runs no hook.
func TestLinkRemovalIsLogged(t *testing.T) {
	cases := []struct {
		name    string
		source  string
		status  accountsv1.BillingStatus
		phase   string
		allowed bool
	}{
		{name: "blocked transfer", source: billing.SourceTransfer, status: accountsv1.BillingStatus_BILLING_STATUS_BLOCKED, phase: "blocked"},
		{name: "blocked delete", source: billing.SourceDelete, status: accountsv1.BillingStatus_BILLING_STATUS_BLOCKED, phase: "blocked"},
		{name: "suspended transfer", source: billing.SourceTransfer, status: accountsv1.BillingStatus_BILLING_STATUS_SUSPENDED, phase: "suspended"},
		{name: "suspended delete", source: billing.SourceDelete, status: accountsv1.BillingStatus_BILLING_STATUS_SUSPENDED, phase: "suspended"},
		{name: "allowed transfer", source: billing.SourceTransfer, status: accountsv1.BillingStatus_BILLING_STATUS_ALLOWED, phase: "allowed", allowed: true},
		{name: "allowed delete", source: billing.SourceDelete, status: accountsv1.BillingStatus_BILLING_STATUS_ALLOWED, phase: "allowed", allowed: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t0 := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
			f := newStatusFixture(t, t0)
			f.addOrg(t, org1)
			// A linked organization keeps the earlier block start of its
			// non-payment, so it is SUSPENDED when the link comes 3 days after
			// the creation of the organization.
			f.clock.Advance(time.Hour)
			if tc.status == accountsv1.BillingStatus_BILLING_STATUS_SUSPENDED {
				f.clock.Advance(3 * 24 * time.Hour)
			}
			if tc.status != accountsv1.BillingStatus_BILLING_STATUS_ALLOWED {
				f.fake.SetAccountStatus(accountA, tc.status, f.clock.Now())
			}
			f.link(t, org1, accountA)
			if got := f.load(t, org1).Confirmed(f.clock.Now()); billing.PhaseName(got) != tc.phase {
				t.Fatalf("linked phase = %v, want %s", got, tc.phase)
			}

			// Count the records and hooks of the removal only.
			var hooks int
			f.statuses.OnChange(func() { hooks++ })
			logsBefore := f.logText()
			f.clock.Advance(time.Minute)
			if former := f.removeLink(t, org1, tc.source); former != accountA {
				t.Fatalf("former billing account = %q", former)
			}
			logs := strings.TrimPrefix(f.logText(), logsBefore)
			if got := strings.Count(logs, `"msg":"billing_status_changed"`); got != 1 {
				t.Fatalf("%d billing_status_changed records, want 1: %s", got, logs)
			}
			for _, want := range []string{
				`"source":"` + tc.source + `"`, `"organization_id":"` + org1 + `"`, `"billing_account_id":"` + accountA + `"`,
				`"from_phase":"` + tc.phase + `"`, `"linked":false`,
			} {
				if !strings.Contains(logs, want) {
					t.Fatalf("record lacks %s: %s", want, logs)
				}
			}
			// An ALLOWED organization becomes BLOCKED; the others keep their phase.
			wantTo := tc.phase
			if tc.allowed {
				wantTo = "blocked"
			}
			if !strings.Contains(logs, `"to_phase":"`+wantTo+`"`) {
				t.Fatalf("record lacks to_phase %s: %s", wantTo, logs)
			}
			if hooks != 1 {
				t.Fatalf("change hooks ran %d times, want 1", hooks)
			}

			// A second removal finds no link: no record and no hook.
			f.clock.Advance(time.Minute)
			logsBefore = f.logText()
			if former := f.removeLink(t, org1, tc.source); former != "" {
				t.Fatalf("former billing account after removal = %q", former)
			}
			if got := f.logText(); got != logsBefore || hooks != 1 {
				t.Fatalf("removal without a link logged %q, hooks = %d", strings.TrimPrefix(got, logsBefore), hooks)
			}
		})
	}
}

// TestPendingUnlinkRetries checks that a pending unlink is retried with
// back-off until delino-accounts accepts it.
func TestPendingUnlinkRetries(t *testing.T) {
	t0 := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	f := newStatusFixture(t, t0)
	ctx := context.Background()
	f.addOrg(t, org1)
	f.link(t, org1, accountA)
	if err := f.db.InTx(ctx, func(tx pgx.Tx) error {
		_, former, _, err := billing.UnlinkTx(ctx, tx, org1, billing.SourceTransfer, t0)
		if err != nil {
			return err
		}
		return billing.AddPendingUnlinkTx(ctx, tx, org1, former, t0)
	}); err != nil {
		t.Fatal(err)
	}
	pending := func() (attempts int, next time.Time, ok bool) {
		err := f.db.Pool.QueryRow(ctx, "SELECT attempts, next_attempt_time FROM billing_pending_unlinks WHERE organization_id = $1", org1).Scan(&attempts, &next)
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, time.Time{}, false
		}
		if err != nil {
			t.Fatal(err)
		}
		return attempts, next, true
	}
	f.fake.Fail(accountstest.MethodUnlinkOrganization, connect.NewError(connect.CodeUnavailable, errors.New("down")))
	if err := f.statuses.RetryPendingUnlinks(ctx); err != nil {
		t.Fatal(err)
	}
	if a, next, ok := pending(); !ok || a != 1 || !next.Equal(t0.Add(time.Second)) {
		t.Fatalf("after one failure: %d %v %v", a, next, ok)
	}
	f.clock.Advance(time.Second)
	_ = f.statuses.RetryPendingUnlinks(ctx)
	if a, next, ok := pending(); !ok || a != 2 || !next.Equal(t0.Add(3*time.Second)) {
		t.Fatalf("after two failures: %d %v %v", a, next, ok)
	}
	// Not due yet: no call.
	calls := f.fake.Calls(accountstest.MethodUnlinkOrganization)
	_ = f.statuses.RetryPendingUnlinks(ctx)
	if f.fake.Calls(accountstest.MethodUnlinkOrganization) != calls {
		t.Fatal("a pending unlink was retried before its next attempt time")
	}
	if _, _, linked := f.fake.Link(org1); !linked {
		t.Fatal("the link is gone before an unlink succeeded")
	}
	f.fake.Fail(accountstest.MethodUnlinkOrganization, nil)
	f.clock.Advance(2 * time.Second)
	if err := f.statuses.RetryPendingUnlinks(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := pending(); ok {
		t.Fatal("the pending unlink stayed after a success")
	}
	if _, _, linked := f.fake.Link(org1); linked {
		t.Fatal("delino-accounts still has the link")
	}
	if !strings.Contains(f.logText(), `"msg":"billing_unlink_confirmed"`) {
		t.Fatal("billing_unlink_confirmed not logged")
	}
}

// TestRunRetriesPendingUnlinksEveryTick checks that the status worker of the
// lease holder retries a due pending unlink at every tick, not only at a
// poll, and that an instance without the lease never retries.
func TestRunRetriesPendingUnlinksEveryTick(t *testing.T) {
	t0 := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	f := newStatusFixture(t, t0)
	ctx := context.Background()
	f.addOrg(t, org1)
	f.link(t, org1, accountA)
	// The poll interval and the first back-off are far longer than the test,
	// so only the tick can cause a retry.
	timing := billing.StatusTiming{Poll: time.Hour, Tick: 20 * time.Millisecond, UnlinkBackoffMin: time.Hour, UnlinkBackoffMax: 2 * time.Hour}
	var mu sync.Mutex
	isHeld := false
	statuses := billing.NewStatuses(billing.StatusOptions{DB: f.db, Logger: f.logger, Now: f.clock.Now, Client: f.fake.Client(), Timing: &timing,
		Held: func() bool { mu.Lock(); defer mu.Unlock(); return isHeld }})
	setHeld := func(v bool) { mu.Lock(); isHeld = v; mu.Unlock() }
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { statuses.Run(runCtx); close(done) }()
	defer func() { cancel(); <-done }()

	if err := f.db.InTx(ctx, func(tx pgx.Tx) error {
		_, former, _, err := billing.UnlinkTx(ctx, tx, org1, billing.SourceTransfer, t0)
		if err != nil {
			return err
		}
		return billing.AddPendingUnlinkTx(ctx, tx, org1, former, t0)
	}); err != nil {
		t.Fatal(err)
	}
	f.fake.Fail(accountstest.MethodUnlinkOrganization, connect.NewError(connect.CodeUnavailable, errors.New("down")))
	calls := func() int { return f.fake.Calls(accountstest.MethodUnlinkOrganization) }

	// Without the lease, a due row stays untouched.
	time.Sleep(300 * time.Millisecond)
	if n := calls(); n != 0 {
		t.Fatalf("an instance without the lease retried a pending unlink %d times", n)
	}

	// With the lease, the next tick retries it although the poll interval has
	// not elapsed since the poll of the first step.
	setHeld(true)
	eventually(t, "the first retry", func() bool { return calls() == 1 })
	polls := f.fake.Calls(accountstest.MethodListBillingStatusChanges)
	if polls != 1 {
		t.Fatalf("polls = %d, want the single poll of the first step", polls)
	}

	// The failed attempt moved the row an hour ahead; later ticks skip it, and
	// no further poll runs within the poll interval.
	time.Sleep(300 * time.Millisecond)
	if n := calls(); n != 1 {
		t.Fatalf("a pending unlink that was not due was retried: %d calls", n)
	}
	if n := f.fake.Calls(accountstest.MethodListBillingStatusChanges); n != polls {
		t.Fatalf("polls = %d, want %d: the poll interval has not elapsed", n, polls)
	}

	// Once the back-off has elapsed, the next tick sends the unlink.
	f.fake.Fail(accountstest.MethodUnlinkOrganization, nil)
	f.clock.Advance(time.Hour + time.Second)
	eventually(t, "the confirmed unlink", func() bool {
		var n int
		if err := f.db.Pool.QueryRow(ctx, "SELECT count(*) FROM billing_pending_unlinks WHERE organization_id = $1", org1).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 0
	})
	if _, _, linked := f.fake.Link(org1); linked {
		t.Fatal("delino-accounts still has the link")
	}
}

// TestStatusMetrics checks the organizations-per-phase gauge.
func TestStatusMetrics(t *testing.T) {
	t0 := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	f := newStatusFixture(t, t0)
	ctx := context.Background()
	f.addOrg(t, org1)
	f.addOrg(t, org2)
	f.link(t, org2, accountA)
	if err := f.statuses.RefreshMetrics(ctx); err != nil {
		t.Fatal(err)
	}
	expect := func(phase string, want float64) {
		t.Helper()
		got, ok := f.registry.gauge(t, "ownsand_billing_organizations", attribute.String("phase", phase))
		if !ok {
			t.Fatalf("no %s sample", phase)
		}
		if got != want {
			t.Fatalf("%s = %v, want %v", phase, got, want)
		}
	}
	expect("allowed", 1)
	expect("blocked", 1)
	expect("suspended", 0)
	// The unlinked organization counts as suspended 3 days after its
	// creation, before Advance stores the phase.
	f.clock.Advance(72 * time.Hour)
	if err := f.statuses.RefreshMetrics(ctx); err != nil {
		t.Fatal(err)
	}
	expect("blocked", 0)
	expect("suspended", 1)
	if _, ok := f.registry.gauge(t, "ownsand_billing_pending_unlinks"); !ok {
		t.Fatal("ownsand_billing_pending_unlinks is not recorded")
	}
}
