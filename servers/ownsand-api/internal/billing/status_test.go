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
	"github.com/delinoio/mono/protos/delino/accounts/v1/accountsv1connect"
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

// pollRecorder wraps a UsageService client and records the start time of
// every ListBillingStatusChanges call, after which it takes delay to answer.
type pollRecorder struct {
	accountsv1connect.UsageServiceClient
	delay time.Duration

	mu     sync.Mutex
	starts []time.Time
}

func (p *pollRecorder) ListBillingStatusChanges(ctx context.Context, req *connect.Request[accountsv1.ListBillingStatusChangesRequest]) (*connect.Response[accountsv1.ListBillingStatusChangesResponse], error) {
	p.mu.Lock()
	p.starts = append(p.starts, time.Now())
	p.mu.Unlock()
	time.Sleep(p.delay)
	return p.UsageServiceClient.ListBillingStatusChanges(ctx, req)
}

func (p *pollRecorder) startTimes() []time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]time.Time(nil), p.starts...)
}

// runStatuses runs the status worker of a lease holder with the timing and
// the client until the returned stop function is called.
func (f *statusFixture) runStatuses(timing billing.StatusTiming, client accountsv1connect.UsageServiceClient, held func() bool) (*billing.Statuses, func()) {
	statuses := billing.NewStatuses(billing.StatusOptions{DB: f.db, Logger: f.logger, Now: f.clock.Now, Client: client, Timing: &timing, Held: held})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { statuses.Run(ctx); close(done) }()
	return statuses, func() { cancel(); <-done }
}

// TestRunPollCadence checks that consecutive polls start one Poll interval
// apart when a poll takes up to one Tick: the interval runs between poll
// starts, and the step that polls is not delayed by one more Tick when it
// runs a few milliseconds before the interval has elapsed. The wall-clock
// bounds are generous: the old behavior (an interval of Poll plus Tick)
// averages 250 ms for the 200 ms interval, the expected one 200 ms.
func TestRunPollCadence(t *testing.T) {
	const (
		poll = 200 * time.Millisecond
		tick = 50 * time.Millisecond
	)
	for _, tc := range []struct {
		name  string
		delay time.Duration
	}{
		{"fast poll", 5 * time.Millisecond},
		// A poll that takes longer than two ticks makes the ticker drop ticks.
		{"slow poll", 120 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newStatusFixture(t, time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC))
			rec := &pollRecorder{UsageServiceClient: f.fake.Client(), delay: tc.delay}
			_, stop := f.runStatuses(billing.StatusTiming{Poll: poll, Tick: tick, UnlinkBackoffMin: time.Hour, UnlinkBackoffMax: 2 * time.Hour}, rec, nil)
			defer stop()
			eventually(t, "ten polls", func() bool { return len(rec.startTimes()) >= 10 })
			starts := rec.startTimes()
			for i := 1; i < len(starts); i++ {
				if gap := starts[i].Sub(starts[i-1]); gap < poll-tick {
					t.Fatalf("poll %d started %v after the previous one, want about %v", i, gap, poll)
				}
			}
			avg := starts[len(starts)-1].Sub(starts[0]) / time.Duration(len(starts)-1)
			if avg < poll-20*time.Millisecond || avg > poll+25*time.Millisecond {
				t.Fatalf("average poll interval = %v over %d polls, want about %v (not %v)", avg, len(starts), poll, poll+tick)
			}
		})
	}
}

// TestRunPollTriggers checks the cases that the poll cadence keeps: the first
// step polls, a failed poll is retried at the next interval and not at every
// tick, KickPoll makes the next step poll, and an instance without the lease
// never polls.
func TestRunPollTriggers(t *testing.T) {
	f := newStatusFixture(t, time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC))
	var mu sync.Mutex
	isHeld := false
	held := func() bool { mu.Lock(); defer mu.Unlock(); return isHeld }
	setHeld := func(v bool) { mu.Lock(); isHeld = v; mu.Unlock() }
	polls := func() int { return f.fake.Calls(accountstest.MethodListBillingStatusChanges) }

	// The poll interval is far longer than the test: only the first step and
	// a kick poll.
	statuses, stop := f.runStatuses(billing.StatusTiming{Poll: time.Hour, Tick: 20 * time.Millisecond, UnlinkBackoffMin: time.Hour, UnlinkBackoffMax: 2 * time.Hour}, f.fake.Client(), held)
	defer stop()
	time.Sleep(200 * time.Millisecond)
	if n := polls(); n != 0 {
		t.Fatalf("an instance without the lease polled %d times", n)
	}
	setHeld(true)
	eventually(t, "the poll of the first held step", func() bool { return polls() == 1 })
	time.Sleep(200 * time.Millisecond)
	if n := polls(); n != 1 {
		t.Fatalf("polls = %d, want 1 within the poll interval", n)
	}
	statuses.KickPoll()
	eventually(t, "the poll after a kick", func() bool { return polls() == 2 })
	time.Sleep(200 * time.Millisecond)
	if n := polls(); n != 2 {
		t.Fatalf("polls = %d, want 2 after the kick", n)
	}
}

// TestRunFailedPollRetriesAtNextInterval checks that a failed poll is retried
// at the next poll interval, not at every tick.
func TestRunFailedPollRetriesAtNextInterval(t *testing.T) {
	f := newStatusFixture(t, time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC))
	f.fake.Fail(accountstest.MethodListBillingStatusChanges, connect.NewError(connect.CodeUnavailable, errors.New("down")))
	_, stop := f.runStatuses(billing.StatusTiming{Poll: 300 * time.Millisecond, Tick: 20 * time.Millisecond, UnlinkBackoffMin: time.Hour, UnlinkBackoffMax: 2 * time.Hour}, f.fake.Client(), nil)
	defer stop()
	time.Sleep(time.Second)
	// One poll at the first step and one every 300 ms: 4 within a second. A
	// retry at every tick would make about 50.
	if n := f.fake.Calls(accountstest.MethodListBillingStatusChanges); n < 3 || n > 6 {
		t.Fatalf("polls = %d within 1s of a 300 ms poll interval, want about 4", n)
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
