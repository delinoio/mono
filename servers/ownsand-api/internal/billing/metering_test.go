// SPDX-License-Identifier: FSL-1.1-ALv2

package billing_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	"github.com/delinoio/mono/protos/delino/accounts/v1/accountsv1connect"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/billing"
	"github.com/delinoio/mono/servers/ownsand-api/internal/db"
	"github.com/delinoio/mono/servers/ownsand-api/internal/db/dbtest"
)

const (
	org1   = "0190f3a4-0000-7000-8000-000000000001"
	org2   = "0190f3a4-0000-7000-8000-000000000002"
	gib    = int64(1) << 30
	relay  = ownsandv1.BillingMeter_BILLING_METER_RELAY_EGRESS
	r2     = ownsandv1.BillingMeter_BILLING_METER_R2_STORAGE
	classA = ownsandv1.BillingMeter_BILLING_METER_CLASS_A_RPC
	classB = ownsandv1.BillingMeter_BILLING_METER_CLASS_B_RPC
)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clock) Set(t time.Time) {
	c.mu.Lock()
	c.now = t
	c.mu.Unlock()
}
func (c *clock) Advance(d time.Duration) { c.Set(c.Now().Add(d)) }

type reportKey struct {
	org    string
	meter  accountsv1.Meter
	window time.Time
}

// fakeAccounts is a fake accounts-api UsageService. It fails while failing
// is set, answers a repeated key as a duplicate, and fails the test when a
// key comes back with another quantity or amount.
type fakeAccounts struct {
	accountsv1connect.UnimplementedUsageServiceHandler
	t *testing.T

	mu           sync.Mutex
	failing      error
	requests     int
	reports      map[reportKey]*accountsv1.UsageReport
	order        []reportKey
	unattributed map[string]bool
	statuses     []*accountsv1.OrganizationBillingStatus
	badResults   bool
}

func (f *fakeAccounts) ReportUsage(_ context.Context, req *connect.Request[accountsv1.ReportUsageRequest]) (*connect.Response[accountsv1.ReportUsageResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	if f.failing != nil {
		return nil, f.failing
	}
	if req.Header().Get("Authorization") != "Bearer test-token" {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("missing token"))
	}
	n := len(req.Msg.GetReports())
	if n < 1 || n > billing.MaxReportBatch {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("%d reports", n))
	}
	resp := &accountsv1.ReportUsageResponse{Statuses: f.statuses}
	for _, r := range req.Msg.GetReports() {
		ws := r.GetWindowStart().AsTime()
		if r.GetService() != accountsv1.ServiceId_SERVICE_ID_OWNSAND || ws.Unix()%300 != 0 || ws.Nanosecond() != 0 || r.GetQuantity() <= 0 || r.GetAmountUsdMicros() < 0 {
			f.t.Errorf("invalid report %v", r)
		}
		k := reportKey{r.GetOrganizationId(), r.GetMeter(), ws}
		res := &accountsv1.UsageReportResult{Service: r.GetService(), OrganizationId: r.GetOrganizationId(), Meter: r.GetMeter(), WindowStart: r.GetWindowStart()}
		if prev, ok := f.reports[k]; ok {
			if prev.GetQuantity() != r.GetQuantity() || prev.GetAmountUsdMicros() != r.GetAmountUsdMicros() {
				f.t.Errorf("window %v reported again with another amount: %v then %v", k, prev, r)
			}
			res.Duplicate = true
		} else {
			f.reports[k] = r
			f.order = append(f.order, k)
		}
		if f.unattributed[r.GetOrganizationId()] {
			res.Unattributed = true
		} else {
			res.BillingAccountId = "0190f3a4-0000-7000-8000-0000000000ba"
		}
		if f.badResults {
			res.OrganizationId = "other"
		}
		resp.Results = append(resp.Results, res)
	}
	return connect.NewResponse(resp), nil
}

func (f *fakeAccounts) report(org string, meter accountsv1.Meter, window time.Time) *accountsv1.UsageReport {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reports[reportKey{org, meter, window}]
}

func (f *fakeAccounts) count() (requests, reports int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests, len(f.reports)
}

func (f *fakeAccounts) setFailing(err error) {
	f.mu.Lock()
	f.failing = err
	f.mu.Unlock()
}

type tokenHeader struct{}

func (tokenHeader) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		req.Header().Set("Authorization", "Bearer test-token")
		return next(ctx, req)
	}
}
func (tokenHeader) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}
func (tokenHeader) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

func newFakeAccounts(t *testing.T) (*fakeAccounts, accountsv1connect.UsageServiceClient) {
	f := &fakeAccounts{t: t, reports: map[reportKey]*accountsv1.UsageReport{}, unattributed: map[string]bool{}}
	mux := http.NewServeMux()
	mux.Handle(accountsv1connect.NewUsageServiceHandler(f))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return f, accountsv1connect.NewUsageServiceClient(srv.Client(), srv.URL, connect.WithInterceptors(tokenHeader{}))
}

type statusRecorder struct {
	mu   sync.Mutex
	seen []*accountsv1.OrganizationBillingStatus
}

func (s *statusRecorder) ApplyReportStatuses(_ context.Context, st []*accountsv1.OrganizationBillingStatus) {
	s.mu.Lock()
	s.seen = append(s.seen, st...)
	s.mu.Unlock()
}

type fixture struct {
	db     *db.DB
	clock  *clock
	logs   *strings.Builder
	logMu  *sync.Mutex
	logger *slog.Logger
}

type lockedWriter struct {
	mu *sync.Mutex
	b  *strings.Builder
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func newFixture(t *testing.T, start time.Time) *fixture {
	f := &fixture{db: dbtest.New(t), clock: &clock{now: start}, logs: &strings.Builder{}, logMu: &sync.Mutex{}}
	f.logger = slog.New(slog.NewJSONHandler(lockedWriter{f.logMu, f.logs}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return f
}

func (f *fixture) logText() string { f.logMu.Lock(); defer f.logMu.Unlock(); return f.logs.String() }

func (f *fixture) metering(client accountsv1connect.UsageServiceClient, opts ...func(*billing.Options)) *billing.Metering {
	o := billing.Options{DB: f.db, Logger: f.logger, Now: f.clock.Now, Client: client}
	for _, fn := range opts {
		fn(&o)
	}
	return billing.New(o)
}

func (f *fixture) counter(t *testing.T, org string, meter ownsandv1.BillingMeter, window time.Time) int64 {
	t.Helper()
	var q int64
	err := f.db.Pool.QueryRow(context.Background(), "SELECT COALESCE(sum(quantity), 0) FROM usage_counters WHERE organization_id = $1 AND meter = $2 AND window_start = $3",
		org, int16(meter), window).Scan(&q)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

type outboxState struct {
	quantity, amount int64
	attempts         int
	next             time.Time
	reported         bool
}

func (f *fixture) outbox(t *testing.T, org string, meter ownsandv1.BillingMeter, window time.Time) (outboxState, bool) {
	t.Helper()
	var s outboxState
	var reported *time.Time
	err := f.db.Pool.QueryRow(context.Background(), `SELECT quantity, amount_usd_micros, attempts, next_attempt_time, report_time FROM usage_outbox
		WHERE organization_id = $1 AND meter = $2 AND window_start = $3`, org, int16(meter), window).Scan(&s.quantity, &s.amount, &s.attempts, &s.next, &reported)
	if err != nil {
		return s, false
	}
	s.reported = reported != nil
	return s, true
}

var t0 = time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)

func TestFlushSealAndReport(t *testing.T) {
	f := newFixture(t, t0.Add(time.Minute))
	accounts, client := newFakeAccounts(t)
	accounts.statuses = []*accountsv1.OrganizationBillingStatus{{Service: accountsv1.ServiceId_SERVICE_ID_OWNSAND, OrganizationId: org1, Status: accountsv1.BillingStatus_BILLING_STATUS_ALLOWED, Sequence: 7}}
	accounts.unattributed[org2] = true
	sink := &statusRecorder{}
	reg := newTestMeter(t)
	m := f.metering(client, func(o *billing.Options) { o.Statuses = sink; o.Metrics = reg })
	ctx := context.Background()

	m.Add(org1, classA, t0.Add(time.Minute), 2)
	m.Add(org1, classA, t0.Add(4*time.Minute+59*time.Second), 1)
	m.Add(org1, classB, t0.Add(2*time.Minute), 101)
	m.Add(org2, relay, t0.Add(3*time.Minute), gib+1)
	m.Add(org1, classA, t0.Add(5*time.Minute), 1) // next window
	if err := m.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if m.Pending() != 0 || f.counter(t, org1, classA, t0) != 3 || f.counter(t, org1, classB, t0) != 101 || f.counter(t, org2, relay, t0) != gib+1 ||
		f.counter(t, org1, classA, t0.Add(5*time.Minute)) != 1 {
		t.Fatal("counters were not written")
	}
	// A second instance adds to the same rows.
	other := f.metering(nil)
	other.Add(org1, classA, t0.Add(30*time.Second), 4)
	if err := other.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.counter(t, org1, classA, t0); got != 7 {
		t.Fatalf("shared counter = %d", got)
	}

	// Nothing is sealed until one minute after the window end.
	f.clock.Set(t0.Add(5*time.Minute + 59*time.Second))
	if n, err := m.Seal(ctx); err != nil || n != 0 {
		t.Fatalf("early seal: %d %v", n, err)
	}
	f.clock.Set(t0.Add(6 * time.Minute))
	if n, err := m.Seal(ctx); err != nil || n != 3 {
		t.Fatalf("seal: %d %v", n, err)
	}
	if n, err := m.Seal(ctx); err != nil || n != 0 {
		t.Fatalf("second seal: %d %v", n, err)
	}
	// Amounts: ceil(7 × 4.5) = 32, ceil(101 × 0.36) = 37, ceil((2^30 + 1) × 90,000 / 2^30) = 90,001.
	for _, c := range []struct {
		org    string
		meter  ownsandv1.BillingMeter
		q, amt int64
	}{{org1, classA, 7, 32}, {org1, classB, 101, 37}, {org2, relay, gib + 1, 90_001}} {
		s, ok := f.outbox(t, c.org, c.meter, t0)
		if !ok || s.quantity != c.q || s.amount != c.amt || s.reported || s.attempts != 0 {
			t.Fatalf("outbox %s %v = %+v %v", c.org, c.meter, s, ok)
		}
	}
	if f.counter(t, org1, classA, t0) != 0 || f.counter(t, org1, classA, t0.Add(5*time.Minute)) != 1 {
		t.Fatal("the seal did not move exactly the ended window")
	}
	// A count of the sealed window that arrives late moves to the oldest open
	// window; the sealed row keeps its amount.
	m.Add(org1, classA, t0.Add(4*time.Minute), 2)
	if err := m.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.counter(t, org1, classA, t0.Add(5*time.Minute)); got != 3 {
		t.Fatalf("late count: open window = %d", got)
	}
	if s, _ := f.outbox(t, org1, classA, t0); s.quantity != 7 {
		t.Fatalf("sealed row changed: %+v", s)
	}
	if !strings.Contains(f.logText(), `"msg":"billing_usage_late_count_moved"`) {
		t.Fatal("missing late count log")
	}
	if err := m.RefreshMetrics(ctx); err != nil {
		t.Fatal(err)
	}
	if got := gauge(t, reg, "ownsand_billing_usage_outbox_depth"); got != 3 {
		t.Fatalf("outbox depth = %v", got)
	}
	if got := gauge(t, reg, "ownsand_billing_usage_report_lag"); got != 60 {
		t.Fatalf("report lag = %v", got)
	}

	if err := m.Report(ctx); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		org    string
		meter  accountsv1.Meter
		q, amt int64
	}{{org1, accountsv1.Meter_METER_OWNSAND_CLASS_A_RPC, 7, 32}, {org1, accountsv1.Meter_METER_OWNSAND_CLASS_B_RPC, 101, 37}, {org2, accountsv1.Meter_METER_OWNSAND_RELAY_EGRESS, gib + 1, 90_001}} {
		r := accounts.report(c.org, c.meter, t0)
		if r == nil || r.GetQuantity() != c.q || r.GetAmountUsdMicros() != c.amt {
			t.Fatalf("report %s %v = %v", c.org, c.meter, r)
		}
	}
	for _, meter := range []ownsandv1.BillingMeter{classA, classB} {
		if s, _ := f.outbox(t, org1, meter, t0); !s.reported || s.attempts != 1 {
			t.Fatalf("outbox row not marked reported: %+v", s)
		}
	}
	// The unattributed report of org2 counts as accepted.
	if s, _ := f.outbox(t, org2, relay, t0); !s.reported {
		t.Fatalf("unattributed row not marked reported: %+v", s)
	}
	if len(sink.seen) != 1 || sink.seen[0].GetSequence() != 7 {
		t.Fatalf("statuses = %v", sink.seen)
	}
	if got := gauge(t, reg, "ownsand_billing_usage_outbox_depth"); got != 0 {
		t.Fatalf("outbox depth after report = %v", got)
	}
	logs := f.logText()
	for _, want := range []string{`"msg":"billing_usage_reported"`, `"organization_id":"` + org1 + `"`, `"meter":"ownsand.class_a_rpc"`, `"window_start":"2026-11-21T10:00:00Z"`, `"result":"unattributed"`} {
		if !strings.Contains(logs, want) {
			t.Fatalf("logs miss %s", want)
		}
	}
	// A report pass with nothing due sends nothing.
	before, _ := accounts.count()
	if err := m.Report(ctx); err != nil {
		t.Fatal(err)
	}
	if after, _ := accounts.count(); after != before {
		t.Fatal("a pass without due rows sent a request")
	}
}

func gauge(t *testing.T, reg *testMeter, name string) float64 {
	t.Helper()
	v, ok := reg.gauge(t, name)
	if !ok {
		t.Fatalf("metric %s not recorded", name)
	}
	return v
}

// TestReportSurvivesOutageAndRestart covers the outbox across an accounts-api
// outage with exponential back-off and across a process restart: no window
// is lost and none is reported with another amount.
func TestReportSurvivesOutageAndRestart(t *testing.T) {
	f := newFixture(t, t0.Add(2*time.Minute))
	accounts, client := newFakeAccounts(t)
	ctx := context.Background()
	m := f.metering(client)
	m.Add(org1, classB, t0.Add(time.Minute), 1000)
	// A graceful shutdown of the first process writes its counts.
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { m.Run(runCtx); close(done) }()
	stop()
	<-done
	if got := f.counter(t, org1, classB, t0); got != 1000 {
		t.Fatalf("counts after the shutdown flush = %d", got)
	}

	// The second process seals the window while accounts-api is down.
	accounts.setFailing(connect.NewError(connect.CodeUnavailable, errors.New("accounts-api is down")))
	m2 := f.metering(client)
	f.clock.Set(t0.Add(6 * time.Minute))
	if n, err := m2.Seal(ctx); err != nil || n != 1 {
		t.Fatalf("seal: %d %v", n, err)
	}
	wantNext := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}
	for i, d := range wantNext {
		if err := m2.Report(ctx); connect.CodeOf(err) != connect.CodeUnavailable {
			t.Fatalf("report %d during the outage: %v", i, err)
		}
		s, _ := f.outbox(t, org1, classB, t0)
		if s.reported || s.attempts != i+1 || !s.next.Equal(f.clock.Now().Add(d)) {
			t.Fatalf("after failure %d: %+v (now %s)", i+1, s, f.clock.Now())
		}
		// Not due yet: nothing is sent.
		requests, _ := accounts.count()
		if err := m2.Report(ctx); err != nil {
			t.Fatal(err)
		}
		if again, _ := accounts.count(); again != requests {
			t.Fatal("a row was retried before its back-off ended")
		}
		f.clock.Advance(d)
	}
	if !strings.Contains(f.logText(), `"msg":"billing_usage_report_failed"`) {
		t.Fatal("missing report failure log")
	}

	// A third process reports after accounts-api recovered.
	accounts.setFailing(nil)
	m3 := f.metering(client)
	if err := m3.Report(ctx); err != nil {
		t.Fatal(err)
	}
	r := accounts.report(org1, accountsv1.Meter_METER_OWNSAND_CLASS_B_RPC, t0)
	if r == nil || r.GetQuantity() != 1000 || r.GetAmountUsdMicros() != 360 {
		t.Fatalf("report = %v", r)
	}
	if s, _ := f.outbox(t, org1, classB, t0); !s.reported || s.attempts != 5 {
		t.Fatalf("after recovery: %+v", s)
	}
	// A retry of an accepted window (for example after a lost response) is a
	// duplicate with the same amount, which counts as accepted.
	if _, err := f.db.Pool.Exec(ctx, "UPDATE usage_outbox SET report_time = NULL"); err != nil {
		t.Fatal(err)
	}
	if err := m3.Report(ctx); err != nil {
		t.Fatal(err)
	}
	if s, _ := f.outbox(t, org1, classB, t0); !s.reported {
		t.Fatalf("duplicate not accepted: %+v", s)
	}
}

func TestBackoffCapsAtFiveMinutes(t *testing.T) {
	f := newFixture(t, t0.Add(10*time.Minute))
	accounts, client := newFakeAccounts(t)
	accounts.setFailing(connect.NewError(connect.CodeInternal, errors.New("boom")))
	m := f.metering(client)
	ctx := context.Background()
	if _, err := f.db.Pool.Exec(ctx, `INSERT INTO usage_outbox (organization_id, meter, window_start, quantity, amount_usd_micros, seal_time, attempts, next_attempt_time)
		VALUES ($1, $2, $3, 1, 5, $4, 20, $4)`, org1, int16(classA), t0, f.clock.Now()); err != nil {
		t.Fatal(err)
	}
	if err := m.Report(ctx); err == nil {
		t.Fatal("report succeeded")
	}
	if s, _ := f.outbox(t, org1, classA, t0); s.attempts != 21 || !s.next.Equal(f.clock.Now().Add(5*time.Minute)) {
		t.Fatalf("capped back-off: %+v", s)
	}
}

func TestReportBatchesAndRejectsMismatchedResults(t *testing.T) {
	f := newFixture(t, t0.Add(24*time.Hour))
	accounts, client := newFakeAccounts(t)
	m := f.metering(client)
	ctx := context.Background()
	// 501 sealed rows, one per window of one organization.
	if _, err := f.db.Pool.Exec(ctx, `INSERT INTO usage_outbox (organization_id, meter, window_start, quantity, amount_usd_micros, seal_time, next_attempt_time)
		SELECT $1, $2, $3::timestamptz + make_interval(mins => 5 * i), 1, 1, $4, $4 FROM generate_series(0, 500) AS i`, org2, int16(classB), t0.Add(-48*time.Hour), f.clock.Now()); err != nil {
		t.Fatal(err)
	}
	accounts.badResults = true
	if err := m.Report(ctx); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatched results: %v", err)
	}
	accounts.badResults = false
	f.clock.Advance(time.Second)
	if err := m.Report(ctx); err != nil {
		t.Fatal(err)
	}
	requests, reports := accounts.count()
	if requests != 3 || reports != 501 {
		t.Fatalf("requests = %d, reports = %d", requests, reports)
	}
	depth, _, err := m.OutboxStats(ctx)
	if err != nil || depth != 0 {
		t.Fatalf("depth = %d %v", depth, err)
	}
}

func TestReportingDisabledKeepsTheOutbox(t *testing.T) {
	f := newFixture(t, t0.Add(time.Minute))
	m := f.metering(nil)
	ctx := context.Background()
	m.Add(org1, classA, t0, 1)
	if err := m.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	f.clock.Set(t0.Add(6 * time.Minute))
	if n, err := m.Seal(ctx); err != nil || n != 1 {
		t.Fatalf("seal: %d %v", n, err)
	}
	if err := m.Report(ctx); !errors.Is(err, billing.ErrReportingDisabled) {
		t.Fatalf("report: %v", err)
	}
	if s, ok := f.outbox(t, org1, classA, t0); !ok || s.reported || s.amount != 5 {
		t.Fatalf("outbox = %+v %v", s, ok)
	}
	// Run logs the idle reporter once.
	ctx2, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { m.Run(ctx2); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(f.logText(), "billing_usage_reporter_disabled") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if strings.Count(f.logText(), "billing_usage_reporter_disabled") != 1 {
		t.Fatal("the idle reporter was not logged exactly once")
	}
}

func TestFlushFailureKeepsCounts(t *testing.T) {
	f := newFixture(t, t0)
	m := f.metering(nil)
	m.Add(org1, classA, t0, 3)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.Flush(canceled); err == nil {
		t.Fatal("flush with a canceled context succeeded")
	}
	if m.Pending() != 1 {
		t.Fatalf("pending = %d", m.Pending())
	}
	if err := m.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.counter(t, org1, classA, t0); got != 3 {
		t.Fatalf("counter = %d", got)
	}
}

func TestConcurrentFlushes(t *testing.T) {
	f := newFixture(t, t0)
	ctx := context.Background()
	instances := make([]*billing.Metering, 4)
	for i := range instances {
		instances[i] = f.metering(nil)
	}
	var wg sync.WaitGroup
	for _, m := range instances {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				for _, org := range []string{org2, org1} {
					m.Add(org, classA, t0, 1)
					m.Add(org, classB, t0, 2)
				}
				if err := m.Flush(ctx); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	if a, b := f.counter(t, org1, classA, t0), f.counter(t, org2, classB, t0); a != 80 || b != 160 {
		t.Fatalf("counters = %d %d", a, b)
	}
}

func TestSealConflictKeepsTheSealedAmount(t *testing.T) {
	f := newFixture(t, t0.Add(11*time.Minute))
	m := f.metering(nil)
	ctx := context.Background()
	// A row of the window is in the outbox already, and a counter of the same
	// window exists below the seal bound (as an older release could leave).
	if _, err := f.db.Pool.Exec(ctx, `INSERT INTO usage_outbox (organization_id, meter, window_start, quantity, amount_usd_micros, seal_time, next_attempt_time)
		VALUES ($1, $2, $3, 10, 45, $3, $3)`, org1, int16(classA), t0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Pool.Exec(ctx, "INSERT INTO usage_counters (organization_id, meter, window_start, quantity) VALUES ($1, $2, $3, 4)", org1, int16(classA), t0); err != nil {
		t.Fatal(err)
	}
	if n, err := m.Seal(ctx); err != nil || n != 0 {
		t.Fatalf("seal: %d %v", n, err)
	}
	if s, _ := f.outbox(t, org1, classA, t0); s.quantity != 10 || s.amount != 45 {
		t.Fatalf("sealed row changed: %+v", s)
	}
	// The count moved to the oldest open window: [10:05, 10:10) was sealed by
	// this pass too, so it is 10:10.
	if got := f.counter(t, org1, classA, t0.Add(10*time.Minute)); got != 4 {
		t.Fatalf("moved count = %d", got)
	}
}

// insertExport adds an organization with an exported snapshot of parts.
func insertSnapshot(t *testing.T, d *db.DB, org, snapshot string, exportState ownsandv1.SnapshotExportState, parts ...int64) {
	t.Helper()
	ctx := context.Background()
	if _, err := d.Pool.Exec(ctx, "INSERT INTO organizations (organization_id, name, create_time) VALUES ($1, 'org', now()) ON CONFLICT DO NOTHING", org); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(ctx, `INSERT INTO snapshots (snapshot_id, organization_id, source_sandbox_id, state, export_state, guest_os, host_os, arch,
		vcpu_count, memory_mib, disk_gib, network_mode, created_by_kind, created_by_id, create_time)
		VALUES ($1, $2, $1, 1, $3, 1, 1, 1, 1, 512, 8, 1, 1, 'u', now())`, snapshot, org, int16(exportState)); err != nil {
		t.Fatal(err)
	}
	for i, size := range parts {
		if _, err := d.Pool.Exec(ctx, "INSERT INTO snapshot_parts (snapshot_id, part_number, size_bytes, sha256) VALUES ($1, $2, $3, '\\x00')", snapshot, i+1, size); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSampleStorage(t *testing.T) {
	f := newFixture(t, t0.Add(30*time.Second))
	ctx := context.Background()
	insertSnapshot(t, f.db, org1, "0190f3a4-0000-7000-8000-0000000000a1", ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTED, gib, gib/2)
	insertSnapshot(t, f.db, org1, "0190f3a4-0000-7000-8000-0000000000a2", ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTED, gib/2)
	// Parts of an export that is still running or failed are not stored usage.
	insertSnapshot(t, f.db, org1, "0190f3a4-0000-7000-8000-0000000000a3", ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTING, gib)
	insertSnapshot(t, f.db, org2, "0190f3a4-0000-7000-8000-0000000000a4", ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORT_FAILED, gib)
	m := f.metering(nil)
	other := f.metering(nil)
	if ok, err := m.SampleStorage(ctx); err != nil || !ok {
		t.Fatalf("sample: %v %v", ok, err)
	}
	// Each window is sampled once across instances.
	f.clock.Advance(4 * time.Minute)
	if ok, err := other.SampleStorage(ctx); err != nil || ok {
		t.Fatalf("second sample of the window: %v %v", ok, err)
	}
	if got := f.counter(t, org1, r2, t0); got != 2*gib*300 {
		t.Fatalf("org1 byte-seconds = %d", got)
	}
	if got := f.counter(t, org2, r2, t0); got != 0 {
		t.Fatalf("org2 byte-seconds = %d", got)
	}
	f.clock.Advance(time.Minute)
	if ok, err := other.SampleStorage(ctx); err != nil || !ok {
		t.Fatalf("sample of the next window: %v %v", ok, err)
	}
	f.clock.Set(t0.Add(11 * time.Minute))
	if n, err := m.Seal(ctx); err != nil || n != 2 {
		t.Fatalf("seal: %d %v", n, err)
	}
	// 2 GiB for one window: ceil(2 × 300 × 30,000 / (3,600 × 730)) = 7.
	for _, w := range []time.Time{t0, t0.Add(5 * time.Minute)} {
		if s, ok := f.outbox(t, org1, r2, w); !ok || s.quantity != 2*gib*300 || s.amount != 7 {
			t.Fatalf("storage outbox %s = %+v %v", w, s, ok)
		}
	}
}

func TestRunSealsAndReportsOnTheLeaseHolderOnly(t *testing.T) {
	f := newFixture(t, t0.Add(time.Minute))
	accounts, client := newFakeAccounts(t)
	timing := billing.Timing{Flush: 10 * time.Millisecond, Tick: 10 * time.Millisecond, Report: time.Hour, BackoffMin: 10 * time.Millisecond, BackoffMax: time.Second}
	var held sync.Mutex
	isHeld := false
	m := f.metering(client, func(o *billing.Options) {
		o.Timing = &timing
		o.Held = func() bool { held.Lock(); defer held.Unlock(); return isHeld }
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	m.Add(org1, classA, t0.Add(time.Minute), 2)
	f.clock.Set(t0.Add(7 * time.Minute))
	eventually(t, "counts flushed", func() bool { return f.counter(t, org1, classA, t0) == 2 })
	time.Sleep(50 * time.Millisecond)
	if _, ok := f.outbox(t, org1, classA, t0); ok {
		t.Fatal("an instance without the lease sealed")
	}
	held.Lock()
	isHeld = true
	held.Unlock()
	eventually(t, "window reported", func() bool {
		return accounts.report(org1, accountsv1.Meter_METER_OWNSAND_CLASS_A_RPC, t0) != nil
	})
	if s, _ := f.outbox(t, org1, classA, t0); s.amount != 9 {
		t.Fatalf("amount = %d", s.amount)
	}
}

// A new lease holder reports the due rows that another instance sealed, even
// when it seals no window itself, within about one Tick of the lease handover
// (the idle Report interval is an hour here, so only the lease check wakes
// the reporter).
func TestRunReportsDueRowsAfterLeaseHandover(t *testing.T) {
	f := newFixture(t, t0.Add(time.Minute))
	accounts, client := newFakeAccounts(t)
	ctx := context.Background()

	// A previous holder sealed the window and never reported it.
	previous := f.metering(nil)
	previous.Add(org1, classA, t0.Add(time.Minute), 2)
	if err := previous.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	f.clock.Set(t0.Add(7 * time.Minute))
	if n, err := previous.Seal(ctx); err != nil || n != 1 {
		t.Fatalf("seal: %d %v", n, err)
	}

	timing := billing.Timing{Flush: 10 * time.Millisecond, Tick: 10 * time.Millisecond, Report: time.Hour, BackoffMin: 10 * time.Millisecond, BackoffMax: time.Second}
	var held atomic.Bool
	m := f.metering(client, func(o *billing.Options) {
		o.Timing = &timing
		o.Held = held.Load
	})
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { m.Run(runCtx); close(done) }()
	defer func() { cancel(); <-done }()

	// While the instance is not the holder, several Ticks pass without a report.
	time.Sleep(100 * time.Millisecond)
	if _, reports := accounts.count(); reports != 0 {
		t.Fatalf("an instance without the lease reported %d rows", reports)
	}
	held.Store(true)
	eventually(t, "due row reported after the handover", func() bool {
		return accounts.report(org1, accountsv1.Meter_METER_OWNSAND_CLASS_A_RPC, t0) != nil
	})

	// While this instance holds the lease it keeps the idle Report interval:
	// a due row that appears without a kick waits for it.
	// Wait until the first pass ended (it marks the row reported and then looks
	// for the next due row) so that the new row cannot be part of it.
	eventually(t, "first pass finished", func() bool {
		s, ok := f.outbox(t, org1, classA, t0)
		return ok && s.reported
	})
	time.Sleep(100 * time.Millisecond)
	_, before := accounts.count()
	if _, err := f.db.Pool.Exec(ctx, `INSERT INTO usage_outbox (organization_id, meter, window_start, quantity, amount_usd_micros, seal_time, next_attempt_time)
		SELECT organization_id, $1, window_start, quantity, amount_usd_micros, seal_time, seal_time FROM usage_outbox WHERE organization_id = $2 AND meter = $3 AND window_start = $4`,
		int16(classB), org1, int16(classA), t0); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if _, after := accounts.count(); after != before {
		t.Fatalf("the holder reported %d rows before its idle interval ended", after-before)
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
