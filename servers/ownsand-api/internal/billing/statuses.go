// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	"github.com/delinoio/mono/protos/delino/accounts/v1/accountsv1connect"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/db"
)

// staleLogInterval bounds the billing_status_stale warnings to one per
// organization and interval.
const staleLogInterval = 10 * time.Minute

// unlinkCallTimeout bounds one UnlinkOrganization call of a pending unlink.
const unlinkCallTimeout = 10 * time.Second

// StatusTiming holds the intervals of the status worker; tests change them.
type StatusTiming struct {
	// Poll is the interval of the ListBillingStatusChanges poll (1 minute).
	Poll time.Duration
	// Tick is the interval at which the lease holder records time-based
	// suspensions and retries pending unlinks, and every instance refreshes
	// the phase metrics (10 seconds).
	Tick time.Duration
	// UnlinkBackoffMin and UnlinkBackoffMax bound the retry delay of a
	// pending unlink (1 second to 5 minutes).
	UnlinkBackoffMin time.Duration
	UnlinkBackoffMax time.Duration
}

// DefaultStatusTiming returns the recorded intervals.
func DefaultStatusTiming() StatusTiming {
	return StatusTiming{Poll: PollInterval, Tick: 10 * time.Second, UnlinkBackoffMin: time.Second, UnlinkBackoffMax: 5 * time.Minute}
}

// StatusOptions configures Statuses.
type StatusOptions struct {
	DB     *db.DB
	Logger *slog.Logger
	// Now is the clock of the status rules; nil selects time.Now.
	Now func() time.Time
	// Held reports whether this instance holds the instance lease; nil
	// means always.
	Held func() bool
	// Client is the delino-accounts UsageService client. Without it, the
	// poll and the pending unlinks stay idle, and the RPCs that need
	// delino-accounts fail with unavailable.
	Client accountsv1connect.UsageServiceClient
	// Metrics provides the meter of the status metrics; nil records none.
	Metrics metric.MeterProvider
	// Timing overrides DefaultStatusTiming.
	Timing *StatusTiming
}

// Statuses is the billing status cache of the control plane
// (docs/servers-ownsand-api-foundation.md#billing-status-cache). It applies
// the statuses of usage report responses (it is the StatusSink of
// Metering), polls changed statuses on the lease holder, records time-based
// suspensions and retries the unlinks that delino-accounts has not
// confirmed yet. Gated RPCs read only the cache.
type Statuses struct {
	db      *db.DB
	logger  *slog.Logger
	now     func() time.Time
	held    func() bool
	client  accountsv1connect.UsageServiceClient
	timing  StatusTiming
	metrics *statusMetrics

	pollMu sync.Mutex
	kick   chan struct{}

	hookMu   sync.Mutex
	onChange []func()

	staleMu     sync.Mutex
	staleLogged map[string]time.Time
}

// NewStatuses returns the status cache for opts.
func NewStatuses(opts StatusOptions) *Statuses {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Held == nil {
		opts.Held = func() bool { return true }
	}
	timing := DefaultStatusTiming()
	if opts.Timing != nil {
		timing = *opts.Timing
	}
	return &Statuses{
		db: opts.DB, logger: opts.Logger, now: opts.Now, held: opts.Held, client: opts.Client, timing: timing,
		metrics: newStatusMetrics(opts.Metrics), kick: make(chan struct{}, 1), staleLogged: map[string]time.Time{},
	}
}

// Client returns the delino-accounts client, or nil when none is configured.
func (s *Statuses) Client() accountsv1connect.UsageServiceClient { return s.client }

// OnChange registers fn to run after a status change committed, for example
// to start the suspension worker at once.
func (s *Statuses) OnChange(fn func()) {
	s.hookMu.Lock()
	s.onChange = append(s.onChange, fn)
	s.hookMu.Unlock()
}

// Committed logs changes that committed and runs the change hooks.
func (s *Statuses) Committed(ctx context.Context, changes ...Change) {
	changed := false
	for _, c := range changes {
		if c.OrganizationID == "" || !c.Changed() {
			continue
		}
		changed = true
		LogChange(ctx, s.logger, c)
	}
	if !changed {
		return
	}
	s.hookMu.Lock()
	hooks := append([]func(){}, s.onChange...)
	s.hookMu.Unlock()
	for _, fn := range hooks {
		fn()
	}
}

// Load returns the cached status of an existing organization. An
// organization without a row (none is expected) is treated as unlinked and
// blocked from now.
func (s *Statuses) Load(ctx context.Context, q db.Querier, orgID string) (Status, error) {
	st, ok, err := LoadStatus(ctx, q, orgID, false)
	if err != nil {
		return Status{}, err
	}
	if !ok {
		now := s.now()
		s.logger.ErrorContext(ctx, "billing_status_missing", slog.String("organization_id", orgID))
		return unlinkedStatus(orgID, now, 0, now), nil
	}
	return st, nil
}

// Enforced returns the cached status of the organization and the phase that
// the RPC gates apply now. It logs billing_status_stale (at most once per
// organization every 10 minutes) when the gates fail open.
func (s *Statuses) Enforced(ctx context.Context, q db.Querier, orgID string) (Status, ownsandv1.BillingPhase, error) {
	st, err := s.Load(ctx, q, orgID)
	if err != nil {
		return Status{}, 0, err
	}
	now := s.now()
	if st.Stale(now) {
		s.warnStale(ctx, st, now)
	}
	return st, st.Enforced(now), nil
}

func (s *Statuses) warnStale(ctx context.Context, st Status, now time.Time) {
	s.staleMu.Lock()
	last, ok := s.staleLogged[st.OrganizationID]
	if ok && now.Sub(last) < staleLogInterval && !now.Before(last) {
		s.staleMu.Unlock()
		return
	}
	s.staleLogged[st.OrganizationID] = now
	if len(s.staleLogged) > 10000 {
		for id, t := range s.staleLogged {
			if now.Sub(t) >= staleLogInterval {
				delete(s.staleLogged, id)
			}
		}
	}
	s.staleMu.Unlock()
	attrs := []slog.Attr{slog.String("organization_id", st.OrganizationID), slog.String("billing_account_id", st.BillingAccountID),
		slog.String("cached_phase", PhaseName(st.Confirmed(now)))}
	if st.SyncTime != nil {
		attrs = append(attrs, slog.Time("status_sync_time", *st.SyncTime))
	}
	s.logger.LogAttrs(ctx, slog.LevelWarn, "billing_status_stale", attrs...)
}

// ApplyReportStatuses applies the statuses of an accepted ReportUsage
// response.
func (s *Statuses) ApplyReportStatuses(ctx context.Context, statuses []*accountsv1.OrganizationBillingStatus) {
	for _, r := range statuses {
		if err := s.applyRemote(ctx, r, SourceReport); err != nil {
			s.logger.WarnContext(ctx, "billing_status_apply_failed", slog.String("organization_id", r.GetOrganizationId()),
				slog.String("source", SourceReport), slog.Any("error", err))
		}
	}
}

func (s *Statuses) applyRemote(ctx context.Context, r *accountsv1.OrganizationBillingStatus, source string) error {
	var change Change
	err := s.db.InTx(ctx, func(tx pgx.Tx) error {
		var err error
		change, _, err = ApplyRemoteTx(ctx, tx, r, source, s.now())
		return err
	})
	if err != nil {
		return err
	}
	s.Committed(ctx, change)
	return nil
}

// Poll reads the statuses that changed after the stored cursor, applies
// them, advances the cursor and then refreshes the sync time of every linked
// organization, because a successful poll proves that the cached statuses
// are current.
func (s *Statuses) Poll(ctx context.Context) error {
	if s.client == nil {
		return ErrReportingDisabled
	}
	s.pollMu.Lock()
	defer s.pollMu.Unlock()
	start := s.now()
	var after int64
	if err := s.db.Pool.QueryRow(ctx, "SELECT after_sequence FROM billing_status_cursor WHERE singleton").Scan(&after); err != nil {
		s.logger.WarnContext(ctx, "billing_status_poll_failed", slog.String("code", "internal"), slog.Any("error", err))
		return err
	}
	applied := 0
	for {
		resp, err := s.client.ListBillingStatusChanges(ctx, connect.NewRequest(&accountsv1.ListBillingStatusChangesRequest{
			Service: ServiceID, AfterSequence: after, PageSize: StatusPageSize,
		}))
		if err != nil {
			if ctx.Err() != nil {
				return err
			}
			code := connect.CodeOf(err).String()
			add(s.metrics.pollFailures, attribute.String("code", code))
			s.logger.WarnContext(ctx, "billing_status_poll_failed", slog.String("code", code), slog.Int64("after_sequence", after), slog.Any("error", err))
			return err
		}
		for _, r := range resp.Msg.GetStatuses() {
			if err := s.applyRemote(ctx, r, SourcePoll); err != nil {
				s.logger.WarnContext(ctx, "billing_status_apply_failed", slog.String("organization_id", r.GetOrganizationId()),
					slog.String("source", SourcePoll), slog.Any("error", err))
				return err
			}
			applied++
		}
		next := resp.Msg.GetNextSequence()
		if next > after {
			if _, err := s.db.Pool.Exec(ctx, "UPDATE billing_status_cursor SET after_sequence = $1 WHERE singleton AND after_sequence < $1", next); err != nil {
				return err
			}
		}
		full := len(resp.Msg.GetStatuses()) >= StatusPageSize
		if !full || next <= after {
			break
		}
		after = next
	}
	if _, err := s.db.Pool.Exec(ctx, `UPDATE organization_billing SET status_sync_time = $1
		WHERE billing_account_id IS NOT NULL AND (status_sync_time IS NULL OR status_sync_time < $1)`, start); err != nil {
		return err
	}
	if _, err := s.db.Pool.Exec(ctx, "UPDATE billing_status_cursor SET poll_time = $1 WHERE singleton", start); err != nil {
		return err
	}
	s.logger.DebugContext(ctx, "billing_status_polled", slog.Int("statuses", applied), slog.Int64("after_sequence", after))
	return nil
}

// Advance records the suspensions that the passing of time caused: a
// BLOCKED organization whose block started 3 days ago becomes SUSPENDED.
// The gates and the suspension worker compute the same phase from the block
// start, so Advance only makes the stored phase, the logs and the metrics
// follow.
func (s *Statuses) Advance(ctx context.Context) error {
	now := s.now()
	var changes []Change
	err := s.db.InTx(ctx, func(tx pgx.Tx) error {
		changes = nil
		rows, err := tx.Query(ctx, statusSelect+" WHERE phase = $1 AND blocked_since <= $2 FOR UPDATE SKIP LOCKED",
			int16(PhaseBlocked), now.Add(-SuspendAfter))
		if err != nil {
			return err
		}
		var due []Status
		for rows.Next() {
			st, err := scanStatus(rows)
			if err != nil {
				rows.Close()
				return err
			}
			due = append(due, st)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, st := range due {
			next := st
			next.Phase = PhaseSuspended
			if err := writeStatus(ctx, tx, next, now); err != nil {
				return err
			}
			changes = append(changes, Change{OrganizationID: st.OrganizationID, BillingAccountID: st.BillingAccountID, From: PhaseBlocked, To: PhaseSuspended,
				Linked: st.Linked(), BlockedSince: st.BlockedSince, Source: SourceTime})
		}
		return nil
	})
	if err != nil {
		s.logger.WarnContext(ctx, "billing_status_advance_failed", slog.Any("error", err))
		return err
	}
	s.Committed(ctx, changes...)
	return nil
}

// UnlinkPending sends UnlinkOrganization for the pending unlink of the
// organization, if it has one that is due, and deletes it after
// delino-accounts accepted it. A pending unlink is a link that
// TransferOwnership or DeleteOrganization removed locally or a link attempt
// whose outcome is unknown. It takes the organization row lock in the mode
// that LinkBillingAccount takes, so a pending unlink never removes a link
// that the new owner made after it. A link attempt whose call can still be
// running is left alone.
//
// The due check reads the row after the lock and compares it with a clock
// reading that it takes after the lock, because the wait for the lock can be
// long. A caller that selected the row as due before the lock sends nothing
// when a transaction that held the lock moved the deadline into the future
// or removed the row, and it sends the call when the deadline passed while
// it waited.
func (s *Statuses) UnlinkPending(ctx context.Context, orgID string) error {
	if s.client == nil {
		return ErrReportingDisabled
	}
	return s.db.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT 1 FROM organizations WHERE organization_id = $1 FOR NO KEY UPDATE", orgID); err != nil {
			return err
		}
		now := s.now()
		var due bool
		err := tx.QueryRow(ctx, "SELECT next_attempt_time <= $2 FROM billing_pending_unlinks WHERE organization_id = $1", orgID, now).Scan(&due)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !due) {
			return nil
		}
		if err != nil {
			return err
		}
		err = s.UnlinkPendingTx(ctx, tx, orgID, now)
		if errors.Is(err, ErrUnlinkFailed) || errors.Is(err, ErrLinkInProgress) {
			// The failed attempt is recorded in the transaction; commit it.
			return nil
		}
		return err
	})
}

// ConfirmUnlinkedTx makes sure that delino-accounts has no link for the
// organization when the organization has no local link: it sends the pending
// unlink of the organization, or, when there is none, an UnlinkOrganization
// call that is idempotent on delino-accounts. A cached link or a link
// attempt record is not a precondition, because delino-accounts can hold a
// link that left no local trace. It records a pending unlink without a
// billing account first and lets UnlinkPendingTx send and delete it, so it
// has the errors, the locking rules and the ordering of UnlinkPendingTx
// (ErrLinkInProgress while a link attempt can still be running). tx must
// hold the organization row lock. When it returns an error, the caller rolls
// tx back, which removes the row that it recorded.
func (s *Statuses) ConfirmUnlinkedTx(ctx context.Context, tx pgx.Tx, orgID string, now time.Time) error {
	if s.client == nil {
		return ErrReportingDisabled
	}
	if _, err := tx.Exec(ctx, `INSERT INTO billing_pending_unlinks (organization_id, create_time, attempts, next_attempt_time)
		VALUES ($1, $2, 0, $2) ON CONFLICT (organization_id) DO NOTHING`, orgID, now); err != nil {
		return err
	}
	return s.UnlinkPendingTx(ctx, tx, orgID, now)
}

// ErrUnlinkFailed marks a failed UnlinkOrganization call whose retry is
// recorded.
var ErrUnlinkFailed = errors.New("billing: UnlinkOrganization failed")

// UnlinkPendingTx sends the pending unlink of the organization in tx, which
// must hold the organization row lock (FOR NO KEY UPDATE or stronger) when
// the organization exists. It deletes the pending row after delino-accounts
// accepted the unlink. When the call fails, it records the next attempt
// time in tx (the time of the failure plus the back-off of the attempt) and
// returns an error that wraps the Connect error and ErrUnlinkFailed. It
// sends nothing, leaves the row intact and returns ErrLinkInProgress while
// the row is a link attempt whose LinkOrganization call can still be running
// (link_call_deadline) or a pending unlink that a takeover of such an
// attempt guards until the call can no longer run (unlink_not_before). Every
// caller, the retry of the lease holder and the synchronous link and unlink
// RPCs alike, therefore honours the same guard.
func (s *Statuses) UnlinkPendingTx(ctx context.Context, tx pgx.Tx, orgID string, now time.Time) error {
	var account string
	var attempts int
	var running, guard *time.Time
	err := tx.QueryRow(ctx, "SELECT COALESCE(billing_account_id::text, ''), attempts, link_call_deadline, unlink_not_before FROM billing_pending_unlinks WHERE organization_id = $1 FOR UPDATE", orgID).Scan(&account, &attempts, &running, &guard)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if (running != nil && running.After(now)) || (guard != nil && guard.After(now)) {
		return ErrLinkInProgress
	}
	if s.client == nil {
		return ErrReportingDisabled
	}
	cctx, cancel := context.WithTimeout(ctx, unlinkCallTimeout)
	_, cerr := s.client.UnlinkOrganization(cctx, connect.NewRequest(&accountsv1.UnlinkOrganizationRequest{Service: ServiceID, OrganizationId: orgID}))
	cancel()
	if cerr != nil {
		// The back-off starts when the call failed, not at the caller's
		// earlier reading of the clock: the wait for the organization lock
		// and the call itself must not consume the delay.
		next := s.now().Add(s.unlinkBackoff(attempts + 1))
		if _, err := tx.Exec(ctx, "UPDATE billing_pending_unlinks SET attempts = attempts + 1, next_attempt_time = $2 WHERE organization_id = $1", orgID, next); err != nil {
			return err
		}
		s.logger.WarnContext(ctx, "billing_unlink_failed", slog.String("organization_id", orgID), slog.String("billing_account_id", account),
			slog.Int("attempt", attempts+1), slog.String("code", connect.CodeOf(cerr).String()), slog.String("error", cerr.Error()), slog.Time("next_attempt_time", next))
		return errors.Join(ErrUnlinkFailed, cerr)
	}
	if _, err := tx.Exec(ctx, "DELETE FROM billing_pending_unlinks WHERE organization_id = $1", orgID); err != nil {
		return err
	}
	s.logger.InfoContext(ctx, "billing_unlink_confirmed", slog.String("organization_id", orgID), slog.String("billing_account_id", account), slog.Int("attempt", attempts+1))
	return nil
}

// unlinkBackoff returns the retry delay after the attempt-th failure.
func (s *Statuses) unlinkBackoff(attempt int) time.Duration {
	d := s.timing.UnlinkBackoffMin
	for i := 1; i < attempt && d < s.timing.UnlinkBackoffMax; i++ {
		d *= 2
	}
	return min(d, s.timing.UnlinkBackoffMax)
}

// RetryPendingUnlinks sends every pending unlink that is due, oldest
// next_attempt_time first, one at a time. It stops at the next row when
// this instance no longer holds the instance lease, so a batch that a lease
// handover overtook does not keep sending: the new holder retries the
// remaining rows, which stay due.
func (s *Statuses) RetryPendingUnlinks(ctx context.Context) error {
	if s.client == nil {
		return ErrReportingDisabled
	}
	rows, err := s.db.Pool.Query(ctx, "SELECT organization_id::text FROM billing_pending_unlinks WHERE next_attempt_time <= $1 ORDER BY next_attempt_time LIMIT 100", s.now())
	if err != nil {
		return err
	}
	var orgs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		orgs = append(orgs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, orgID := range orgs {
		if !s.held() {
			return nil
		}
		if err := s.UnlinkPending(ctx, orgID); err != nil && ctx.Err() != nil {
			return err
		}
	}
	return nil
}

// RefreshMetrics updates the number of organizations in each confirmed
// phase and the number of pending unlinks.
func (s *Statuses) RefreshMetrics(ctx context.Context) error {
	rows, err := s.db.Pool.Query(ctx, `SELECT CASE WHEN phase = $1 AND blocked_since <= $2 THEN $3 ELSE phase END AS p, count(*)
		FROM organization_billing GROUP BY p`, int16(PhaseBlocked), s.now().Add(-SuspendAfter), int16(PhaseSuspended))
	if err != nil {
		return err
	}
	counts := map[ownsandv1.BillingPhase]int64{}
	for rows.Next() {
		var phase int16
		var n int64
		if err := rows.Scan(&phase, &n); err != nil {
			rows.Close()
			return err
		}
		counts[ownsandv1.BillingPhase(phase)] += n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, p := range []ownsandv1.BillingPhase{PhaseAllowed, PhaseBlocked, PhaseSuspended} {
		s.metrics.organizations.Record(ctx, counts[p], metric.WithAttributes(attribute.String("phase", PhaseName(p))))
	}
	var pending int64
	if err := s.db.Pool.QueryRow(ctx, "SELECT count(*) FROM billing_pending_unlinks").Scan(&pending); err != nil {
		return err
	}
	s.metrics.pendingUnlinks.Record(ctx, pending)
	return nil
}

// Run runs the status worker until ctx ends: on the lease holder it polls
// every Poll interval (measured between poll starts) and, every Tick,
// records time-based suspensions and starts a retry of the due pending
// unlinks unless the previous retry is still running; every instance
// refreshes the metrics. A slow UnlinkOrganization call delays none of
// these steps. Run returns after the running retry ended.
func (s *Statuses) Run(ctx context.Context) {
	if s.client == nil {
		s.logger.Warn("billing_status_sync_disabled",
			slog.String("detail", "LOGTO_TOKEN_ENDPOINT, ACCOUNTS_M2M_CLIENT_ID and ACCOUNTS_M2M_CLIENT_SECRET are unset; billing statuses are not polled and billing links cannot change"))
	}
	t := time.NewTicker(s.timing.Tick)
	defer t.Stop()
	// A retry batch can take as long as ten seconds for each due row, so it
	// runs in its own goroutine, one batch at a time: the poll, the
	// time-based suspensions and the metrics keep their schedule. Run waits
	// for the running batch before it returns; the batch ends with ctx.
	var batch sync.WaitGroup
	defer batch.Wait()
	var retrying atomic.Bool
	var lastPoll time.Time
	for {
		if s.held() {
			if s.client != nil {
				// The poll interval runs from the start of the previous poll,
				// not from its end, and a step may run up to half a tick
				// early: the step is sampled only at a tick, and a step that
				// runs a few milliseconds before the interval has elapsed
				// would otherwise wait for the next tick, which stretches
				// every interval by one Tick.
				if lastPoll.IsZero() || time.Since(lastPoll) >= s.timing.Poll-s.timing.Tick/2 {
					// A failed poll is retried at the next poll interval, not
					// at every tick; Poll logs the failure.
					lastPoll = time.Now()
					_ = s.Poll(ctx)
				}
				// Due pending unlinks are retried at every tick, not only at
				// a poll: the back-off bounds (1 second to 5 minutes) are
				// shorter than the poll interval, and an attempt that is not
				// due yet is skipped by the query. A tick that finds the
				// previous batch still running starts none: the rows that it
				// has not reached stay due and are selected first by the
				// next batch.
				if retrying.CompareAndSwap(false, true) {
					batch.Add(1)
					go func() {
						defer batch.Done()
						defer retrying.Store(false)
						_ = s.RetryPendingUnlinks(ctx)
					}()
				}
			}
			_ = s.Advance(ctx)
		}
		_ = s.RefreshMetrics(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.kick:
			lastPoll = time.Time{}
		}
	}
}

// KickPoll makes the worker poll at its next step, for example after a
// lease handover.
func (s *Statuses) KickPoll() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// statusMetrics are the OpenTelemetry metrics of the status cache. Their
// labels never contain organization, user or billing account IDs.
type statusMetrics struct {
	organizations  metric.Int64Gauge
	pollFailures   metric.Int64Counter
	pendingUnlinks metric.Int64Gauge
}

func newStatusMetrics(provider metric.MeterProvider) *statusMetrics {
	m := meter(provider)
	return &statusMetrics{
		organizations:  int64Gauge(m, "ownsand_billing_organizations", "Organizations in each confirmed billing phase (allowed, blocked or suspended)."),
		pollFailures:   int64Counter(m, "ownsand_billing_status_poll_failures", "Failed ListBillingStatusChanges requests, by Connect code."),
		pendingUnlinks: int64Gauge(m, "ownsand_billing_pending_unlinks", "Billing links that were removed locally, or link attempts with an unknown outcome, that delino-accounts has not confirmed yet."),
	}
}
