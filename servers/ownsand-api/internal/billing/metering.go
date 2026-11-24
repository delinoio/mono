// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/protobuf/types/known/timestamppb"

	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	"github.com/delinoio/mono/protos/delino/accounts/v1/accountsv1connect"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/db"
)

// MaxReportBatch is the largest ReportUsage request.
const MaxReportBatch = 500

// shutdownFlushTimeout bounds the final counter flush of a graceful
// shutdown.
const shutdownFlushTimeout = 10 * time.Second

// ErrReportingDisabled is returned by Report when no delino-accounts client
// is configured.
var ErrReportingDisabled = errors.New("billing: usage reporting is not configured")

// StatusSink receives the billing statuses that each accepted ReportUsage
// response returns (one entry per linked organization of the request). The
// billing status cache implements it.
type StatusSink interface {
	ApplyReportStatuses(ctx context.Context, statuses []*accountsv1.OrganizationBillingStatus)
}

// NoStatusSink drops the statuses.
type NoStatusSink struct{}

// ApplyReportStatuses does nothing.
func (NoStatusSink) ApplyReportStatuses(context.Context, []*accountsv1.OrganizationBillingStatus) {}

// Timing holds the metering intervals of the contract; tests shorten them.
type Timing struct {
	// Flush is the interval of the counter writes of every instance.
	Flush time.Duration
	// Tick is the interval at which the lease holder samples the R2 storage
	// of a new window, seals ended windows and every instance refreshes the
	// outbox metrics.
	Tick time.Duration
	// Report is the interval of the reporter when nothing is due earlier and
	// this instance holds the lease. An instance without the lease checks it
	// every Tick instead.
	Report time.Duration
	// BackoffMin and BackoffMax bound the exponential retry delay of a
	// failed report.
	BackoffMin time.Duration
	BackoffMax time.Duration
}

// DefaultTiming returns the recorded intervals: counter writes every 30
// seconds, a lease holder tick every 10 seconds, reports every 5 minutes and
// after each seal, and retries from 1 second up to 5 minutes.
func DefaultTiming() Timing {
	return Timing{Flush: 30 * time.Second, Tick: 10 * time.Second, Report: 5 * time.Minute, BackoffMin: time.Second, BackoffMax: 5 * time.Minute}
}

// Options configures Metering.
type Options struct {
	DB     *db.DB
	Logger *slog.Logger
	// Now is the clock of windows, seals and retries; nil selects time.Now.
	Now func() time.Time
	// Held reports whether this instance holds the instance lease. Only the
	// lease holder samples, seals and reports; nil means always.
	Held func() bool
	// Client reports usage to delino-accounts. When it is nil, usage is
	// still metered and sealed into the outbox, and the reporter stays idle
	// until a later process has a client.
	Client accountsv1connect.UsageServiceClient
	// Statuses receives the statuses of accepted reports; nil drops them.
	Statuses StatusSink
	// Metrics provides the meter of the metering metrics; nil records none.
	Metrics metric.MeterProvider
	// Timing overrides DefaultTiming.
	Timing *Timing
}

// Metering meters usage on this instance and, on the lease holder, seals
// windows and reports them. Every step is idempotent and safe across
// instances: the seal state row and the storage sample rows serialize them
// in Postgres, and the report idempotency key makes a repeated report
// harmless.
type Metering struct {
	db       *db.DB
	logger   *slog.Logger
	now      func() time.Time
	held     func() bool
	client   accountsv1connect.UsageServiceClient
	statuses StatusSink
	timing   Timing
	metrics  *metrics
	rec      *Recorder
	// newTimer creates the idle timer of the reporter. It is always the real
	// timer outside tests, which replace it to control when the timer fires.
	newTimer func(time.Duration) reportTimer

	flushMu  sync.Mutex
	reportMu sync.Mutex
	kick     chan struct{}
	// sampled is the start (Unix seconds) of the last window whose storage
	// sample this instance confirmed.
	sampled atomic.Int64
}

// New returns a Metering for opts.
func New(opts Options) *Metering {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Held == nil {
		opts.Held = func() bool { return true }
	}
	if opts.Statuses == nil {
		opts.Statuses = NoStatusSink{}
	}
	timing := DefaultTiming()
	if opts.Timing != nil {
		timing = *opts.Timing
	}
	return &Metering{
		db: opts.DB, logger: opts.Logger, now: opts.Now, held: opts.Held, client: opts.Client, statuses: opts.Statuses,
		timing: timing, metrics: newMetrics(opts.Metrics), rec: NewRecorder(opts.Logger), kick: make(chan struct{}, 1),
		newTimer: newRealReportTimer,
	}
}

// reportTimer is the part of time.Timer that the reporter uses. It exists so
// that a test can decide when the idle timer fires; production code always
// uses the real timer.
type reportTimer interface {
	C() <-chan time.Time
	Stop() bool
	Reset(d time.Duration) bool
}

type realReportTimer struct{ *time.Timer }

func newRealReportTimer(d time.Duration) reportTimer { return realReportTimer{time.NewTimer(d)} }

func (t realReportTimer) C() <-chan time.Time { return t.Timer.C }

// Add records quantity units of meter for the organization at the given
// time. It only updates memory; Flush writes the counts to Postgres.
func (m *Metering) Add(orgID string, meter ownsandv1.BillingMeter, at time.Time, quantity int64) {
	m.rec.Add(orgID, meter, at, quantity)
}

// Pending returns the number of in-memory counters that wait for a flush.
func (m *Metering) Pending() int { return m.rec.Pending() }

// ReportingEnabled reports whether a delino-accounts client is configured.
func (m *Metering) ReportingEnabled() bool { return m.client != nil }

// Run runs the metering workers until ctx ends and then writes the
// remaining counts once more.
func (m *Metering) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); m.runFlush(ctx) }()
	go func() { defer wg.Done(); m.runTick(ctx) }()
	go func() { defer wg.Done(); m.runReport(ctx) }()
	wg.Wait()
	fctx, cancel := context.WithTimeout(context.Background(), shutdownFlushTimeout)
	defer cancel()
	_ = m.Flush(fctx)
}

func (m *Metering) runFlush(ctx context.Context) {
	t := time.NewTicker(m.timing.Flush)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		_ = m.Flush(ctx)
	}
}

func (m *Metering) runTick(ctx context.Context) {
	t := time.NewTicker(m.timing.Tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if m.held() {
			if window := WindowStart(m.now()); m.sampled.Load() != window.Unix() {
				_, _ = m.SampleStorage(ctx)
			}
			if n, err := m.Seal(ctx); err == nil && n > 0 {
				m.WakeReporter()
			}
		}
		_ = m.RefreshMetrics(ctx)
	}
}

// WakeReporter queues a report pass without waiting for the reporter.
// The reporter checks the lease before it sends a batch.
func (m *Metering) WakeReporter() {
	if !m.ReportingEnabled() {
		return
	}
	select {
	case m.kick <- struct{}{}:
	default:
	}
}

func (m *Metering) runReport(ctx context.Context) {
	if m.client == nil {
		m.logger.Warn("billing_usage_reporter_disabled",
			slog.String("detail", "LOGTO_TOKEN_ENDPOINT, ACCOUNTS_M2M_CLIENT_ID and ACCOUNTS_M2M_CLIENT_SECRET are unset; metered usage stays in the outbox until a process with them reports it"))
		return
	}
	timer := m.newTimer(m.timing.Tick)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C():
		case <-m.kick:
		}
		// A lease acquisition or seal can interrupt the wait. An instance
		// without the lease checks again after one Tick. The holder waits
		// the longer Report interval or until the next row is due.
		wait := m.timing.Tick
		if m.held() {
			wait = m.timing.Report
			_ = m.Report(ctx)
			if due, ok, err := m.nextDue(ctx); err == nil && ok {
				if d := due.Sub(m.now()); d < wait {
					wait = max(d, m.timing.BackoffMin)
				}
			}
		}
		if !timer.Stop() {
			select {
			case <-timer.C():
			default:
			}
		}
		timer.Reset(wait)
	}
}

// FlushReceiptRetention is how long the receipt of a committed flush batch
// stays in Postgres before the janitor deletes it. A batch that a flush
// retries is safe while the retry runs within this time of the batch's
// first attempt; the retry of an older batch whose first attempt was
// committed but not acknowledged could count it twice (Metering logs
// `billing_usage_flush_batch_stale` for such a retry). The bound is far
// above the 10-second lifetime of the final flush and the 30-second flush
// interval, and a process that cannot reach Postgres for this long is
// restarted by the deployment long before.
const FlushReceiptRetention = 7 * 24 * time.Hour

// Flush adds the in-memory counts of this instance to the shared counter
// rows. A count of a window that was sealed meanwhile moves to the oldest
// open window. The counts go in batches with a receipt ID: after a failure,
// including a commit whose outcome is unknown because the connection broke,
// the next flush retries the same batch under the same ID, and Postgres
// applies it at most once. Counts that arrive meanwhile form the next batch.
func (m *Metering) Flush(ctx context.Context) error { return m.flush(ctx, false) }

// FlushFinal writes the counts that are still in memory and closes the
// recorder in the same step: a count that an RPC adds afterwards is logged
// as `billing_usage_dropped_after_final_flush` and never written. Call it
// once, as the last write before the process exits.
func (m *Metering) FlushFinal(ctx context.Context) error { return m.flush(ctx, true) }

func (m *Metering) flush(ctx context.Context, final bool) error {
	m.flushMu.Lock()
	defer m.flushMu.Unlock()
	if final {
		// Seal the live counters and close the recorder in one step, so a
		// failed final flush keeps its batch for the retry of the caller.
		if err := m.rec.sealBatch(true, m.now()); err != nil {
			m.logger.Error("billing_usage_flush_failed", slog.Any("error", err))
			return err
		}
	}
	sealedLive := final
	for {
		b := m.rec.firstPending()
		if b == nil {
			if sealedLive {
				return nil
			}
			// Batches that failed earlier are written first; the live counters
			// become the next batch.
			if err := m.rec.sealBatch(false, m.now()); err != nil {
				m.logger.Error("billing_usage_flush_failed", slog.Any("error", err))
				return err
			}
			sealedLive = true
			continue
		}
		if err := m.applyBatch(ctx, b); err != nil {
			return err
		}
		m.rec.ack(b)
	}
}

// applyBatch writes one batch in one transaction that first records the
// batch ID as a receipt. A receipt that exists already shows that an earlier
// attempt committed this batch, so the batch adds nothing now: this keeps
// the retry of a lost commit response from counting the batch twice, and
// keeps it from adding to a window that was sealed after the first attempt.
func (m *Metering) applyBatch(ctx context.Context, b *flushBatch) error {
	if age := m.now().Sub(b.created); age > FlushReceiptRetention {
		// The janitor may have deleted the receipt of an earlier, unacknowledged
		// attempt of this batch, so this write is no longer protected.
		m.logger.Warn("billing_usage_flush_batch_stale", slog.String("batch_id", b.id), slog.Duration("age", age), slog.Duration("retention", FlushReceiptRetention))
	}
	var moved []counterKey
	var sealedBefore time.Time
	duplicate := false
	err := m.db.InTx(ctx, func(tx pgx.Tx) error {
		var err error
		if sealedBefore, err = lockSealStateShared(ctx, tx); err != nil {
			return err
		}
		fresh, err := insertFlushReceipt(ctx, tx, b.id, m.now())
		if err != nil {
			return err
		}
		if !fresh {
			duplicate = true
			return nil
		}
		moved, err = addCounters(ctx, tx, sealedBefore, b.counts)
		return err
	})
	if err != nil {
		add(m.metrics.flushFailures)
		m.logger.Warn("billing_usage_flush_failed", slog.String("batch_id", b.id), slog.Int("counters", len(b.counts)), slog.Any("error", err))
		return err
	}
	if duplicate {
		m.logger.Warn("billing_usage_flush_batch_already_applied", slog.String("batch_id", b.id), slog.Int("counters", len(b.counts)))
		return nil
	}
	for _, k := range moved {
		m.logger.Warn("billing_usage_late_count_moved", slog.String("organization_id", k.org), slog.String("meter", MeterID(k.meter)),
			slog.Time("window_start", k.windowStart()), slog.Time("moved_to_window_start", sealedBefore), slog.Int64("quantity", b.counts[k]))
	}
	m.logger.Debug("billing_usage_flushed", slog.String("batch_id", b.id), slog.Int("counters", len(b.counts)))
	return nil
}

// SampleStorage samples the R2 storage of exported snapshots for the window
// that contains now, once per window across all instances, and reports
// whether this call took the sample.
func (m *Metering) SampleStorage(ctx context.Context) (bool, error) {
	now := m.now()
	window := WindowStart(now)
	var counts map[counterKey]int64
	var moved []counterKey
	sampled := false
	err := m.db.InTx(ctx, func(tx pgx.Tx) error {
		sealedBefore, err := lockSealStateShared(ctx, tx)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, "INSERT INTO usage_storage_samples (window_start, sample_time) VALUES ($1, $2) ON CONFLICT (window_start) DO NOTHING", window, now)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		sampled = true
		if counts, err = storageCounts(ctx, tx, window); err != nil {
			return err
		}
		moved, err = addCounters(ctx, tx, sealedBefore, counts)
		return err
	})
	if err != nil {
		m.logger.Warn("billing_storage_sample_failed", slog.Time("window_start", window), slog.Any("error", err))
		return false, err
	}
	m.sampled.Store(window.Unix())
	if !sampled {
		return false, nil
	}
	for k, v := range counts {
		m.logger.Debug("billing_storage_sampled", slog.String("organization_id", k.org), slog.String("meter", MeterID(k.meter)),
			slog.Time("window_start", k.windowStart()), slog.Int64("quantity", v))
	}
	for _, k := range moved {
		m.logger.Warn("billing_usage_late_count_moved", slog.String("organization_id", k.org), slog.String("meter", MeterID(k.meter)),
			slog.Time("window_start", k.windowStart()), slog.String("cause", "the window was already sealed"))
	}
	m.logger.Info("billing_storage_sample_taken", slog.Time("window_start", window), slog.Int("organizations", len(counts)))
	return true, nil
}

// Seal seals every window that ended at least one minute ago: it moves the
// counters of those windows to the usage outbox with their amounts and
// returns the number of outbox rows it wrote. A window with a quantity of 0
// produces no row.
func (m *Metering) Seal(ctx context.Context) (int, error) {
	now := m.now()
	cutoff := sealCutoff(now)
	var sealed []outboxRow
	var conflicts map[counterKey]bool
	var unknown []counterKey
	err := m.db.InTx(ctx, func(tx pgx.Tx) error {
		sealed, conflicts, unknown = nil, nil, nil
		var sealedBefore time.Time
		if err := tx.QueryRow(ctx, "SELECT sealed_before FROM usage_seal_state WHERE singleton FOR UPDATE").Scan(&sealedBefore); err != nil {
			return err
		}
		if !cutoff.After(sealedBefore) {
			return nil
		}
		rows, err := tx.Query(ctx, "DELETE FROM usage_counters WHERE window_start < $1 RETURNING organization_id::text, meter, window_start, quantity", cutoff)
		if err != nil {
			return err
		}
		keep := map[counterKey]int64{}
		for rows.Next() {
			var r outboxRow
			var meter int16
			if err := rows.Scan(&r.org, &meter, &r.windowStart, &r.quantity); err != nil {
				rows.Close()
				return err
			}
			r.meter, r.windowStart = ownsandv1.BillingMeter(meter), r.windowStart.UTC()
			if r.quantity <= 0 {
				continue
			}
			amount, err := Amount(r.meter, r.quantity)
			if err != nil {
				// A meter that this release does not know (for example a row
				// written by a newer release before a rollback) stays in the
				// counters until a release that knows it seals it.
				k := counterKey{org: r.org, meter: r.meter, window: r.windowStart.Unix()}
				keep[k] = r.quantity
				unknown = append(unknown, k)
				continue
			}
			r.amount = amount
			sealed = append(sealed, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if conflicts, err = insertOutbox(ctx, tx, sealed, now); err != nil {
			return err
		}
		// A row that is already in the outbox keeps its amount; the count
		// moves to the oldest open window instead of being lost.
		late := map[counterKey]int64{}
		for _, r := range sealed {
			if k := (counterKey{org: r.org, meter: r.meter, window: r.windowStart.Unix()}); conflicts[k] {
				late[k] = r.quantity
			}
		}
		if _, err := addCounters(ctx, tx, cutoff, late); err != nil {
			return err
		}
		if len(keep) > 0 {
			// Re-insert below the cutoff on purpose: addCounters would move
			// these counts, which no amount can be computed for, to the open
			// window.
			if _, err := addCounters(ctx, tx, time.Unix(0, 0), keep); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, "UPDATE usage_seal_state SET sealed_before = $1 WHERE singleton", cutoff)
		return err
	})
	if err != nil {
		m.logger.Warn("billing_usage_seal_failed", slog.Time("sealed_before", cutoff), slog.Any("error", err))
		return 0, err
	}
	n := 0
	for _, r := range sealed {
		k := counterKey{org: r.org, meter: r.meter, window: r.windowStart.Unix()}
		if conflicts[k] {
			m.logger.Error("billing_usage_seal_conflict", slog.String("organization_id", r.org), slog.String("meter", MeterID(r.meter)),
				slog.Time("window_start", r.windowStart), slog.Int64("quantity", r.quantity), slog.Time("moved_to_window_start", cutoff))
			continue
		}
		n++
		add(m.metrics.sealedRows, attribute.String("meter", MeterID(r.meter)))
		m.logger.Info("billing_usage_sealed", slog.String("organization_id", r.org), slog.String("meter", MeterID(r.meter)),
			slog.Time("window_start", r.windowStart), slog.Int64("quantity", r.quantity), slog.Int64("amount_usd_micros", r.amount))
	}
	for _, k := range unknown {
		m.logger.Error("billing_usage_unknown_meter", slog.String("organization_id", k.org), slog.Int("meter", int(k.meter)), slog.Time("window_start", k.windowStart()))
	}
	return n, nil
}

// Report sends the due unreported outbox rows of the OwnSand meters to
// delino-accounts in batches of at most MaxReportBatch until none is due, a
// batch fails or this instance no longer holds the lease. A failed batch is
// retried with exponential back-off. The lease is checked immediately before
// each batch RPC: a former holder ends the pass without an error and sends no
// later batch, and it leaves the unsent rows due with their attempts and
// next attempt time unchanged for the new holder. A batch RPC that has
// already started finishes, and its acknowledgement or retry handling
// applies.
func (m *Metering) Report(ctx context.Context) error {
	if m.client == nil {
		return ErrReportingDisabled
	}
	m.reportMu.Lock()
	defer m.reportMu.Unlock()
	m.logUnsupportedOutbox(ctx)
	for {
		rows, err := dueRows(ctx, m.db.Pool, m.now(), MaxReportBatch)
		if err != nil {
			if ctx.Err() == nil {
				m.logger.Warn("billing_usage_report_load_failed", slog.Any("error", err))
			}
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		if !m.held() {
			m.logger.Info("billing_usage_report_lease_lost", slog.Int("skipped_due_rows", len(rows)))
			return nil
		}
		if err := m.reportBatch(ctx, rows); err != nil {
			return err
		}
		if len(rows) < MaxReportBatch {
			return nil
		}
	}
}

func (m *Metering) reportBatch(ctx context.Context, rows []outboxRow) error {
	req := &accountsv1.ReportUsageRequest{Reports: make([]*accountsv1.UsageReport, len(rows))}
	for i, r := range rows {
		req.Reports[i] = &accountsv1.UsageReport{
			Service: ServiceID, OrganizationId: r.org, Meter: AccountsMeter(r.meter),
			WindowStart: timestamppb.New(r.windowStart), Quantity: r.quantity, AmountUsdMicros: r.amount,
		}
	}
	resp, err := m.client.ReportUsage(ctx, connect.NewRequest(req))
	if err == nil {
		err = checkResults(req.Reports, resp.Msg.GetResults())
	}
	if err != nil {
		if ctx.Err() != nil {
			return err
		}
		m.recordFailure(ctx, rows, err)
		return err
	}
	now := m.now()
	orgs, meters, windows := rowKeys(rows)
	if _, err := m.db.Pool.Exec(ctx, `UPDATE usage_outbox o SET report_time = $4, attempts = o.attempts + 1
		FROM unnest($1::uuid[], $2::smallint[], $3::timestamptz[]) AS t(org, meter, w)
		WHERE o.organization_id = t.org AND o.meter = t.meter AND o.window_start = t.w AND o.report_time IS NULL`,
		orgs, meters, windows, now); err != nil {
		// accounts-api accepted the reports; the next attempt is a duplicate
		// that deducts nothing again.
		m.logger.Warn("billing_usage_report_mark_failed", slog.Int("reports", len(rows)), slog.Any("error", err))
		return err
	}
	for i, r := range rows {
		res := resp.Msg.GetResults()[i]
		result := resultCharged
		switch {
		case res.GetDuplicate():
			result = resultDuplicate
		case res.GetUnattributed():
			result = resultUnattributed
		}
		add(m.metrics.reports, attribute.String("result", result))
		m.logger.Info("billing_usage_reported", slog.String("organization_id", r.org), slog.String("meter", MeterID(r.meter)),
			slog.Time("window_start", r.windowStart), slog.Int64("quantity", r.quantity), slog.Int64("amount_usd_micros", r.amount),
			slog.String("result", result), slog.String("billing_account_id", res.GetBillingAccountId()), slog.Int("attempt", r.attempts+1))
	}
	m.statuses.ApplyReportStatuses(ctx, resp.Msg.GetStatuses())
	_ = m.RefreshMetrics(ctx)
	return nil
}

// checkResults checks that accounts-api returned one result per report, in
// request order, with the echoed key.
func checkResults(reports []*accountsv1.UsageReport, results []*accountsv1.UsageReportResult) error {
	if len(results) != len(reports) {
		return fmt.Errorf("accounts-api returned %d results for %d reports", len(results), len(reports))
	}
	for i, rep := range reports {
		res := results[i]
		if res.GetService() != rep.GetService() || res.GetOrganizationId() != rep.GetOrganizationId() || res.GetMeter() != rep.GetMeter() ||
			!res.GetWindowStart().AsTime().Equal(rep.GetWindowStart().AsTime()) {
			return fmt.Errorf("accounts-api result %d does not match its report", i)
		}
	}
	return nil
}

// backoff returns the retry delay after the attempt-th failure (1-based):
// BackoffMin doubled for each earlier failure, at most BackoffMax.
func (m *Metering) backoff(attempt int) time.Duration {
	d := m.timing.BackoffMin
	for i := 1; i < attempt && d < m.timing.BackoffMax; i++ {
		d *= 2
	}
	return min(d, m.timing.BackoffMax)
}

func (m *Metering) recordFailure(ctx context.Context, rows []outboxRow, cause error) {
	code := connect.CodeOf(cause).String()
	add(m.metrics.reportFailures, attribute.String("code", code))
	now := m.now()
	orgs, meters, windows := rowKeys(rows)
	next := make([]time.Time, len(rows))
	for i, r := range rows {
		next[i] = now.Add(m.backoff(r.attempts + 1))
	}
	if _, err := m.db.Pool.Exec(ctx, `UPDATE usage_outbox o SET attempts = o.attempts + 1, next_attempt_time = t.next
		FROM unnest($1::uuid[], $2::smallint[], $3::timestamptz[], $4::timestamptz[]) AS t(org, meter, w, next)
		WHERE o.organization_id = t.org AND o.meter = t.meter AND o.window_start = t.w AND o.report_time IS NULL`,
		orgs, meters, windows, next); err != nil {
		m.logger.Warn("billing_usage_report_retry_store_failed", slog.Int("reports", len(rows)), slog.Any("error", err))
	}
	for i, r := range rows {
		m.logger.Warn("billing_usage_report_failed", slog.String("organization_id", r.org), slog.String("meter", MeterID(r.meter)),
			slog.Time("window_start", r.windowStart), slog.Int("attempt", r.attempts+1), slog.String("code", code),
			slog.String("error", cause.Error()), slog.Time("next_attempt_time", next[i]))
	}
}

// nextDue returns the earliest next attempt time of an unreported row of a
// supported meter (a row of another meter is never reported, so it must not
// keep the reporter waking up).
func (m *Metering) nextDue(ctx context.Context) (time.Time, bool, error) {
	var due *time.Time
	if err := m.db.Pool.QueryRow(ctx,
		"SELECT min(next_attempt_time) FROM usage_outbox WHERE report_time IS NULL AND meter = ANY($1::smallint[])", supportedMeters()).Scan(&due); err != nil {
		return time.Time{}, false, err
	}
	if due == nil {
		return time.Time{}, false, nil
	}
	return *due, true, nil
}

// OutboxStats returns the number of unreported outbox rows of a supported
// meter and the start of the oldest such window. Rows of another meter value
// are counted by unsupportedOutbox instead, so they do not raise the report
// lag that no report can lower.
func (m *Metering) OutboxStats(ctx context.Context) (int64, *time.Time, error) {
	var depth int64
	var oldest *time.Time
	err := m.db.Pool.QueryRow(ctx, "SELECT count(*), min(window_start) FROM usage_outbox WHERE report_time IS NULL AND meter = ANY($1::smallint[])",
		supportedMeters()).Scan(&depth, &oldest)
	return depth, oldest, err
}

// unsupportedOutbox returns the number of unreported outbox rows whose meter
// is not an OwnSand meter, the start of the oldest such window and the
// distinct meter values.
func (m *Metering) unsupportedOutbox(ctx context.Context) (int64, *time.Time, []int16, error) {
	var count int64
	var oldest *time.Time
	var meters []int16
	err := m.db.Pool.QueryRow(ctx, `SELECT count(*), min(window_start), COALESCE(array_agg(DISTINCT meter), '{}'::smallint[])
		FROM usage_outbox WHERE report_time IS NULL AND NOT (meter = ANY($1::smallint[]))`, supportedMeters()).Scan(&count, &oldest, &meters)
	return count, oldest, meters, err
}

// logUnsupportedOutbox logs the unreported rows that the reporter skips
// because their meter is not an OwnSand meter. The rows stay in the outbox
// unchanged, so a later version that supports the meter can report them.
// Report calls it once per pass, which rate-limits the record.
func (m *Metering) logUnsupportedOutbox(ctx context.Context) {
	count, oldest, meters, err := m.unsupportedOutbox(ctx)
	if err != nil || count == 0 {
		return
	}
	attrs := []any{slog.Int64("rows", count), slog.Any("meters", meters)}
	if oldest != nil {
		attrs = append(attrs, slog.Time("oldest_window_start", oldest.UTC()))
	}
	m.logger.Error("billing_usage_unknown_meter_unsent", attrs...)
}

// RefreshMetrics updates the outbox depth and report lag gauges.
func (m *Metering) RefreshMetrics(ctx context.Context) error {
	depth, oldest, err := m.OutboxStats(ctx)
	if err != nil {
		return err
	}
	m.metrics.outboxDepth.Record(ctx, depth)
	lag := 0.0
	if oldest != nil {
		if d := m.now().Sub(oldest.Add(WindowLength)); d > 0 {
			lag = d.Seconds()
		}
	}
	m.metrics.reportLag.Record(ctx, lag)
	unsupported, _, _, err := m.unsupportedOutbox(ctx)
	if err != nil {
		return err
	}
	m.metrics.unsupportedDepth.Record(ctx, unsupported)
	return nil
}
