// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"

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
	// Metrics registers the status metrics; nil registers none.
	Metrics prometheus.Registerer
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
			s.metrics.pollFailures.WithLabelValues(code).Inc()
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
// organization, if it has one, and deletes it after delino-accounts
// accepted it. It takes the organization row lock in the mode that
// LinkBillingAccount takes, so a pending unlink never removes a link that
// the new owner made after it.
func (s *Statuses) UnlinkPending(ctx context.Context, orgID string) error {
	if s.client == nil {
		return ErrReportingDisabled
	}
	now := s.now()
	return s.db.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT 1 FROM organizations WHERE organization_id = $1 FOR NO KEY UPDATE", orgID); err != nil {
			return err
		}
		err := s.UnlinkPendingTx(ctx, tx, orgID, now)
		if errors.Is(err, ErrUnlinkFailed) {
			// The attempt is recorded in the transaction; commit it.
			return nil
		}
		return err
	})
}

// ErrUnlinkFailed marks a failed UnlinkOrganization call whose retry is
// recorded.
var ErrUnlinkFailed = errors.New("billing: UnlinkOrganization failed")

// UnlinkPendingTx sends the pending unlink of the organization in tx, which
// must hold the organization row lock (FOR NO KEY UPDATE or stronger) when
// the organization exists. It deletes the pending row after delino-accounts
// accepted the unlink. When the call fails, it records the next attempt
// time in tx and returns an error that wraps the Connect error.
func (s *Statuses) UnlinkPendingTx(ctx context.Context, tx pgx.Tx, orgID string, now time.Time) error {
	var account string
	var attempts int
	err := tx.QueryRow(ctx, "SELECT billing_account_id::text, attempts FROM billing_pending_unlinks WHERE organization_id = $1 FOR UPDATE", orgID).Scan(&account, &attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if s.client == nil {
		return ErrReportingDisabled
	}
	cctx, cancel := context.WithTimeout(ctx, unlinkCallTimeout)
	_, cerr := s.client.UnlinkOrganization(cctx, connect.NewRequest(&accountsv1.UnlinkOrganizationRequest{Service: ServiceID, OrganizationId: orgID}))
	cancel()
	if cerr != nil {
		next := now.Add(s.unlinkBackoff(attempts + 1))
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

// RetryPendingUnlinks sends every pending unlink that is due.
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
	counts := map[ownsandv1.BillingPhase]float64{}
	for rows.Next() {
		var phase int16
		var n int64
		if err := rows.Scan(&phase, &n); err != nil {
			rows.Close()
			return err
		}
		counts[ownsandv1.BillingPhase(phase)] += float64(n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, p := range []ownsandv1.BillingPhase{PhaseAllowed, PhaseBlocked, PhaseSuspended} {
		s.metrics.organizations.WithLabelValues(PhaseName(p)).Set(counts[p])
	}
	var pending int64
	if err := s.db.Pool.QueryRow(ctx, "SELECT count(*) FROM billing_pending_unlinks").Scan(&pending); err != nil {
		return err
	}
	s.metrics.pendingUnlinks.Set(float64(pending))
	return nil
}

// Run runs the status worker until ctx ends: on the lease holder it polls
// every Poll interval and, every Tick, records time-based suspensions and
// retries due pending unlinks; every instance refreshes the metrics.
func (s *Statuses) Run(ctx context.Context) {
	if s.client == nil {
		s.logger.Warn("billing_status_sync_disabled",
			slog.String("detail", "LOGTO_TOKEN_ENDPOINT, ACCOUNTS_M2M_CLIENT_ID and ACCOUNTS_M2M_CLIENT_SECRET are unset; billing statuses are not polled and billing links cannot change"))
	}
	t := time.NewTicker(s.timing.Tick)
	defer t.Stop()
	var lastPoll time.Time
	for {
		if s.held() {
			if s.client != nil {
				if time.Since(lastPoll) >= s.timing.Poll {
					// A failed poll is retried at the next poll interval, not
					// at every tick; Poll logs the failure.
					_ = s.Poll(ctx)
					lastPoll = time.Now()
				}
				// Due pending unlinks are retried at every tick, not only at
				// a poll: the back-off bounds (1 second to 5 minutes) are
				// shorter than the poll interval, and an attempt that is not
				// due yet is skipped by the query.
				_ = s.RetryPendingUnlinks(ctx)
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

// statusMetrics are the Prometheus metrics of the status cache. Their
// labels never contain organization, user or billing account IDs.
type statusMetrics struct {
	organizations  *prometheus.GaugeVec
	pollFailures   *prometheus.CounterVec
	pendingUnlinks prometheus.Gauge
}

func newStatusMetrics(reg prometheus.Registerer) *statusMetrics {
	m := &statusMetrics{
		organizations: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "ownsand_billing_organizations",
			Help: "Organizations in each confirmed billing phase (allowed, blocked or suspended).",
		}, []string{"phase"}),
		pollFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ownsand_billing_status_poll_failures_total",
			Help: "Failed ListBillingStatusChanges requests, by Connect code.",
		}, []string{"code"}),
		pendingUnlinks: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "ownsand_billing_pending_unlinks",
			Help: "Billing links that were removed locally and that delino-accounts has not confirmed yet.",
		}),
	}
	if reg != nil {
		reg.MustRegister(m.organizations, m.pollFailures, m.pendingUnlinks)
	}
	return m
}
