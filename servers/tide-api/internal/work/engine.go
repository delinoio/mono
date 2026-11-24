// SPDX-License-Identifier: FSL-1.1-ALv2

// Package work implements the queue, the dispatcher, the runner lifecycle
// and WorkService. The owning contract is the Queue and dispatch and Runner
// lifecycle sections of docs/servers-tide-api-foundation.md.
//
// The engine runs on the lease holder as three lease components: the
// dispatcher (one pass per organization when its work changes and at least
// every 15 seconds), the runner reconciler (one driver goroutine per runner
// that is not ended) and the gauge collector. RPCs and the hooks of the
// other layers change requests and runners on any instance with
// compare-and-set updates; the lease holder picks the changes up at its next
// pass, or at once when the change happened on the lease holder.
//
// Workload-specific behavior is in adapters (adapter.go). This package
// implements no adapter; the dev session layer registers the DEV_SESSION
// adapter, and the GitHub and vendor layers add theirs.
package work

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/servers/tide-api/internal/api"
	"github.com/delinoio/mono/servers/tide-api/internal/db"
	"github.com/delinoio/mono/servers/tide-api/internal/link"
	"github.com/delinoio/mono/servers/tide-api/internal/metrics"
	"github.com/delinoio/mono/servers/tide-api/internal/profile"
)

// Timing holds the intervals of the engine.
type Timing struct {
	// PassInterval is the longest time between two dispatcher passes of an
	// organization (15 seconds).
	PassInterval time.Duration
	// ReconcileInterval is the interval of the runner reconciler.
	ReconcileInterval time.Duration
	// SandboxPoll is the interval of the sandbox state checks of a driver.
	SandboxPoll time.Duration
	// StartTimeout bounds the wait for a CREATING sandbox to be RUNNING.
	StartTimeout time.Duration
	// GaugeInterval is the interval of the gauge collector.
	GaugeInterval time.Duration
	// CreateTimeout bounds one CreateSandbox call (15 seconds when zero). A
	// call that reaches it is an uncertain result: the runner keeps its key
	// and its driver repeats the call.
	CreateTimeout time.Duration
}

// defaultCreateTimeout is the CreateTimeout of a zero Timing field.
const defaultCreateTimeout = 15 * time.Second

func (t Timing) createTimeout() time.Duration {
	if t.CreateTimeout > 0 {
		return t.CreateTimeout
	}
	return defaultCreateTimeout
}

// DefaultTiming returns the production intervals.
func DefaultTiming() Timing {
	return Timing{
		PassInterval:      15 * time.Second,
		ReconcileInterval: 5 * time.Second,
		SandboxPoll:       10 * time.Second,
		StartTimeout:      30 * time.Minute,
		GaugeInterval:     15 * time.Second,
		CreateTimeout:     defaultCreateTimeout,
	}
}

// MaxStartAttempts is the number of failed runner starts after which a
// request ends with RUNNER_START_FAILED.
const MaxStartAttempts = 3

// Engine is the queue, the dispatcher and the runner lifecycle.
type Engine struct {
	svc      *api.Service
	links    *link.Links
	profiles *profile.Profiles
	m        *metrics.Metrics
	timing   Timing
	adapters map[tidev1.WorkloadKind]Adapter

	kicks chan string

	mu      sync.Mutex
	drivers map[string]chan struct{}
	// initialWarmCreates holds runner IDs while the dispatcher calls CreateSandbox.
	initialWarmCreates sync.Map
	// runCtx and runWG belong to the running reconciler component.
	runCtx context.Context
	runWG  *sync.WaitGroup
	// pendingReleases holds the requests that a clean-up hook ended in a
	// transaction, for the claim releases after the commit.
	pendingReleases sync.Map
}

// New returns an engine. Register adapters before the app runs.
func New(svc *api.Service, links *link.Links, profiles *profile.Profiles, m *metrics.Metrics, timing Timing) *Engine {
	e := &Engine{svc: svc, links: links, profiles: profiles, m: m, timing: timing,
		adapters: map[tidev1.WorkloadKind]Adapter{}, kicks: make(chan string, 1024), drivers: map[string]chan struct{}{}}
	svc.OnWorkChange = e.Kick
	return e
}

// RegisterAdapter registers the adapter of a workload kind.
func (e *Engine) RegisterAdapter(a Adapter) { e.adapters[a.Kind()] = a }

// Links returns the OwnSand links.
func (e *Engine) Links() *link.Links { return e.links }

// Profiles returns the runner profiles.
func (e *Engine) Profiles() *profile.Profiles { return e.profiles }

// Metrics returns the metrics.
func (e *Engine) Metrics() *metrics.Metrics { return e.m }

// Kick asks the dispatcher of this instance for a pass of the organization.
// It never blocks; a full queue drops the kick, and the periodic pass runs.
func (e *Engine) Kick(orgID string) {
	select {
	case e.kicks <- orgID:
	default:
	}
}

// kickRunner wakes the driver of a runner on this instance.
func (e *Engine) kickRunner(runnerID string) {
	e.mu.Lock()
	ch := e.drivers[runnerID]
	e.mu.Unlock()
	if ch != nil {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// label returns the stable short name of an enum value for metric labels
// and logs, for example DEV_SESSION for WORKLOAD_KIND_DEV_SESSION.
func label(v fmt.Stringer, prefix string) string { return strings.TrimPrefix(v.String(), prefix) }

func kindLabel(k tidev1.WorkloadKind) string     { return label(k, "WORKLOAD_KIND_") }
func reqStateLabel(s tidev1.RequestState) string { return label(s, "REQUEST_STATE_") }
func reqReasonLabel(r tidev1.RequestEndReason) string {
	if r == 0 {
		return "NONE"
	}
	return label(r, "REQUEST_END_REASON_")
}
func runStateLabel(s tidev1.RunnerState) string { return label(s, "RUNNER_STATE_") }
func guestLabel(g tidev1.GuestOs) string        { return label(g, "GUEST_OS_") }
func priorityLabel(p tidev1.Priority) string    { return label(p, "PRIORITY_") }

// decorate runs the RequestDecorator of each kind on the API form of
// requests of one organization.
func (e *Engine) decorate(ctx context.Context, orgID string, reqs ...*tidev1.Request) {
	byKind := map[tidev1.WorkloadKind][]*tidev1.Request{}
	for _, r := range reqs {
		byKind[r.GetKind()] = append(byKind[r.GetKind()], r)
	}
	for kind, list := range byKind {
		d, ok := e.adapters[kind].(RequestDecorator)
		if !ok {
			continue
		}
		if err := d.DecorateRequests(ctx, e.svc.DB.Pool, orgID, list); err != nil && ctx.Err() == nil {
			e.svc.Logger.WarnContext(ctx, "request_decorate_failed", slog.String("organization_id", orgID), slog.String("kind", kindLabel(kind)), slog.Any("error", err))
		}
	}
}

// GetRequest loads a request of the organization (not_found otherwise).
func (e *Engine) GetRequest(ctx context.Context, q db.Querier, orgID, id string) (*Request, error) {
	r, err := scanRequest(q.QueryRow(ctx, "SELECT "+requestColumns+" FROM requests WHERE organization_id = $1 AND request_id = $2", orgID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, api.ErrNotFound("request")
	}
	return r, err
}

func (e *Engine) lockRequest(ctx context.Context, tx pgx.Tx, id string) (*Request, error) {
	r, err := scanRequest(tx.QueryRow(ctx, "SELECT "+requestColumns+" FROM requests WHERE request_id = $1 FOR UPDATE", id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, api.ErrNotFound("request")
	}
	return r, err
}

// GetRunner loads a runner of the organization (not_found otherwise).
func (e *Engine) GetRunner(ctx context.Context, q db.Querier, orgID, id string) (*Runner, error) {
	r, err := scanRunner(q.QueryRow(ctx, "SELECT "+runnerColumns+" FROM runners WHERE organization_id = $1 AND runner_id = $2", orgID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, api.ErrNotFound("runner")
	}
	return r, err
}

func (e *Engine) lockRunner(ctx context.Context, tx pgx.Tx, id string) (*Runner, error) {
	r, err := scanRunner(tx.QueryRow(ctx, "SELECT "+runnerColumns+" FROM runners WHERE runner_id = $1 FOR UPDATE", id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, api.ErrNotFound("runner")
	}
	return r, err
}

func (e *Engine) logRequest(ctx context.Context, r *Request, prev tidev1.RequestState) {
	e.svc.Logger.LogAttrs(ctx, slog.LevelInfo, "request_state_changed",
		slog.String("organization_id", r.OrganizationID), slog.String("request_id", r.ID), slog.String("runner_profile_id", r.ProfileID),
		slog.String("runner_id", r.RunnerID), slog.String("kind", kindLabel(r.Kind)), slog.String("previous_state", reqStateLabel(prev)),
		slog.String("state", reqStateLabel(r.State)), slog.String("end_reason", reqReasonLabel(r.EndReason)))
}

func (e *Engine) logRunner(ctx context.Context, r *Runner, prev tidev1.RunnerState) {
	reason := "NONE"
	if r.EndReason != 0 {
		reason = label(r.EndReason, "RUNNER_END_REASON_")
	}
	e.svc.Logger.LogAttrs(ctx, slog.LevelInfo, "runner_state_changed",
		slog.String("organization_id", r.OrganizationID), slog.String("runner_id", r.ID), slog.String("runner_profile_id", r.ProfileID),
		slog.String("request_id", r.RequestID), slog.String("sandbox_id", r.SandboxID), slog.String("kind", kindLabel(r.Kind)),
		slog.String("previous_state", runStateLabel(prev)), slog.String("state", runStateLabel(r.State)), slog.String("end_reason", reason),
		slog.String("ownsand_end_reason", r.OwnSandEndReason))
}

// NewRequest describes a new request.
type NewRequest struct {
	Profile   *profile.Profile
	Source    Source
	SourceKey string
	// OwnerUserID and Label are set for DEV_SESSION.
	OwnerUserID string
	Label       string
	// PoolKey names the runner pool of a request whose runners the source
	// binds (see Adapter.BindsAtDispatch).
	PoolKey string
	// RequestID is the ID of the new request; empty selects a new UUID v7.
	RequestID string
	// MaxQueueWait, when greater than zero, caps the profile's maximum
	// queue wait for this request (for example, the lifetime of a Claude
	// Code work order).
	MaxQueueWait time.Duration
}

// CreateRequest creates a QUEUED request in tx, or returns the existing
// request of the same organization, source and source key (created is then
// false), so a repeated event or claim never creates a second request. The
// caller kicks the dispatcher after the commit (Service.WorkChanged).
func (e *Engine) CreateRequest(ctx context.Context, tx pgx.Tx, n NewRequest) (*Request, bool, error) {
	p := n.Profile
	id := n.RequestID
	if id == "" {
		id = api.NewID()
	}
	key := n.SourceKey
	if key == "" {
		key = id
	}
	now := e.svc.NowUTC()
	wait := p.MaxQueueWait
	if n.MaxQueueWait > 0 && n.MaxQueueWait < wait {
		wait = n.MaxQueueWait
	}
	var pool any
	if n.PoolKey != "" {
		pool = n.PoolKey
	}
	r, err := scanRequest(tx.QueryRow(ctx, `INSERT INTO requests (request_id, organization_id, runner_profile_id, kind, source, source_key, state, priority,
		queue_time, queue_deadline, owner_user_id, label, pool_key, create_time, update_time)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $9, $9)
		ON CONFLICT (organization_id, source, source_key) DO NOTHING RETURNING `+requestColumns,
		id, p.OrganizationID, p.ID, int16(p.Kind), int16(n.Source), key, int16(reqQueued), int16(p.Priority),
		now, now.Add(wait), n.OwnerUserID, n.Label, pool))
	if errors.Is(err, pgx.ErrNoRows) {
		existing, err := scanRequest(tx.QueryRow(ctx, "SELECT "+requestColumns+" FROM requests WHERE organization_id = $1 AND source = $2 AND source_key = $3",
			p.OrganizationID, int16(n.Source), key))
		return existing, false, err
	}
	if err != nil {
		return nil, false, err
	}
	e.logRequest(ctx, r, tidev1.RequestState_REQUEST_STATE_UNSPECIFIED)
	return r, true, nil
}

// endRequestTx ends an open request of tx. It returns the ended request, or
// nil when the request was already terminal. It records the metric and the
// log and clears the stored work order. Run ReleaseClaim with
// releaseAfterCommit when the request ended before a runner took its work.
func (e *Engine) endRequestTx(ctx context.Context, tx pgx.Tx, id string, end RequestEnd) (*Request, tidev1.RequestState, error) {
	r, err := e.lockRequest(ctx, tx, id)
	if err != nil {
		return nil, 0, err
	}
	if RequestTerminal(r.State) {
		return nil, r.State, nil
	}
	prev := r.State
	now := e.svc.NowUTC()
	r, err = scanRequest(tx.QueryRow(ctx, `UPDATE requests SET state = $2, end_reason = $3, end_time = $4, update_time = $4,
		work_order_key_id = NULL, work_order_ciphertext = NULL WHERE request_id = $1 RETURNING `+requestColumns, id, int16(end.State), int16(end.Reason), now))
	if err != nil {
		return nil, 0, err
	}
	e.logRequest(ctx, r, prev)
	e.m.RequestEnded(kindLabel(r.Kind), reqStateLabel(r.State), reqReasonLabel(r.EndReason))
	if prev == reqQueued {
		e.m.QueueWait(kindLabel(r.Kind), priorityLabel(r.Priority), now.Sub(r.QueueTime).Seconds())
	}
	return r, prev, nil
}

// EndRequestTx ends an open request in the caller's transaction and returns
// it, or nil when it was already terminal. The caller releases a vendor
// claim itself when it needs to.
func (e *Engine) EndRequestTx(ctx context.Context, tx pgx.Tx, id string, end RequestEnd) (*Request, error) {
	r, _, err := e.endRequestTx(ctx, tx, id, end)
	return r, err
}

// EndRequest ends an open request in its own transaction (for example, when
// a source reports that the work completed). It returns nil when the
// request was already terminal. A runner of the request is not changed.
func (e *Engine) EndRequest(ctx context.Context, orgID, id string, end RequestEnd) (*Request, error) {
	var out *Request
	var prev tidev1.RequestState
	err := e.svc.DB.InTx(ctx, func(tx pgx.Tx) error {
		r, err := e.GetRequest(ctx, tx, orgID, id)
		if err != nil {
			return err
		}
		out, prev, err = e.endRequestTx(ctx, tx, r.ID, end)
		return err
	})
	if err != nil {
		return nil, err
	}
	if out != nil && prev != reqRunning {
		e.releaseClaim(ctx, out)
	}
	e.Kick(orgID)
	return out, nil
}

// releaseClaim runs the adapter's ReleaseClaim and logs an error.
func (e *Engine) releaseClaim(ctx context.Context, r *Request) {
	a := e.adapters[r.Kind]
	if a == nil {
		return
	}
	if err := a.ReleaseClaim(context.WithoutCancel(ctx), r); err != nil {
		e.svc.Logger.WarnContext(ctx, "request_claim_release_failed", slog.String("organization_id", r.OrganizationID), slog.String("request_id", r.ID), slog.Any("error", err))
	}
}

// setRunnerTx changes the state of a runner from one of from. It returns
// nil when the runner is in none of them. fields are extra assignments
// (`column = $n` with n from 4) and their values.
func (e *Engine) setRunnerTx(ctx context.Context, tx pgx.Tx, id string, from []tidev1.RunnerState, to tidev1.RunnerState, fields string, args ...any) (*Runner, error) {
	r, err := e.lockRunner(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	ok := false
	for _, s := range from {
		if r.State == s {
			ok = true
		}
	}
	if !ok {
		return nil, nil
	}
	prev := r.State
	set := "state = $2, update_time = $3"
	if RunnerTerminal(to) {
		set += ", end_time = $3"
	}
	if fields != "" {
		set += ", " + fields
	}
	now := e.svc.NowUTC()
	r, err = scanRunner(tx.QueryRow(ctx, "UPDATE runners SET "+set+" WHERE runner_id = $1 RETURNING "+runnerColumns, append([]any{id, int16(to), now}, args...)...))
	if err != nil {
		return nil, err
	}
	e.logRunner(ctx, r, prev)
	if prev != to && (to == runWarm || to == runWaiting || to == runBusy) && r.ReadyTime == nil {
		if _, err := tx.Exec(ctx, "UPDATE runners SET ready_time = $2 WHERE runner_id = $1", id, now); err != nil {
			return nil, err
		}
		e.m.RunnerStart(kindLabel(r.Kind), guestLabel(r.GuestOS), now.Sub(r.CreateTime).Seconds())
	}
	return r, nil
}

// MarkBusy changes a runner that waits for work to BUSY and its request to
// RUNNING (for example, when GitHub reports that the runner took a job).
// requestID binds the request to the runner when the source binds work
// later (GITHUB_JOB); empty keeps the runner's request.
func (e *Engine) MarkBusy(ctx context.Context, orgID, runnerID, requestID string) error {
	err := e.svc.DB.InTx(ctx, func(tx pgx.Tx) error { return e.MarkBusyTx(ctx, tx, orgID, runnerID, requestID) })
	if err == nil {
		e.kickRunner(runnerID)
	}
	return err
}

// MarkBusyTx is MarkBusy in the caller's transaction, for a webhook that
// stores its delivery with the change. After the commit, call
// RunnerChanged. A runner that does not wait for work fails with
// failed_precondition.
func (e *Engine) MarkBusyTx(ctx context.Context, tx pgx.Tx, orgID, runnerID, requestID string) error {
	r, err := e.GetRunner(ctx, tx, orgID, runnerID)
	if err != nil {
		return err
	}
	if requestID == "" {
		requestID = r.RequestID
	}
	r, err = e.setRunnerTx(ctx, tx, r.ID, []tidev1.RunnerState{runWaiting, runPrep, runWarm}, runBusy, "request_id = $4", nullable(requestID))
	if err != nil || r == nil {
		if err == nil {
			err = api.ErrPrecondition("the runner does not wait for work")
		}
		return err
	}
	if requestID != "" {
		return e.startRequestTx(ctx, tx, requestID, r.ID, []tidev1.RequestState{reqQueued, reqStarting}, reqRunning)
	}
	return nil
}

// RunnerChanged wakes the driver of a runner on this instance after a
// change that a transaction of another layer made (MarkBusyTx,
// DeleteRunnerTx). On another instance, the next reconciler step picks the
// change up.
func (e *Engine) RunnerChanged(runnerID string) { e.kickRunner(runnerID) }

// PoolSurplus returns the surplus runners of a runner pool (see
// Adapter.BindsAtDispatch), newest first: the runners of the profile and
// pool in CREATING, PREPARING or WAITING_FOR_WORK without a request beyond
// the pool's QUEUED requests plus reserve (the profile's warm count).
func (e *Engine) PoolSurplus(ctx context.Context, orgID, profileID, poolKey string, reserve int) ([]*Runner, error) {
	var queued int
	if err := e.svc.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM requests WHERE organization_id = $1 AND runner_profile_id = $2 AND pool_key = $3 AND state = $4`,
		orgID, profileID, poolKey, int16(reqQueued)).Scan(&queued); err != nil {
		return nil, err
	}
	rows, err := e.svc.DB.Pool.Query(ctx, "SELECT "+runnerColumns+` FROM runners WHERE organization_id = $1 AND runner_profile_id = $2 AND pool_key = $3
		AND request_id IS NULL AND state = ANY($4) ORDER BY create_time DESC, runner_id DESC`,
		orgID, profileID, poolKey, []int16{int16(runCreating), int16(runPrep), int16(runWaiting)})
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var idle []*Runner
	for rows.Next() {
		r, err := scanRunner(rows)
		if err != nil {
			return nil, err
		}
		idle = append(idle, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if n := len(idle) - queued - max(reserve, 0); n > 0 {
		return idle[:n], nil
	}
	return nil, nil
}

// DeleteIdleRunnerTx deletes a runner with reason in the caller's
// transaction only while it is still in CREATING, PREPARING or
// WAITING_FOR_WORK without a request (a surplus or clean-up of a runner
// pool). It returns nil when the runner changed meanwhile, for example
// because it took work. After the commit, call RunnerChanged (or
// DeleteSandboxOf) for the runner.
func (e *Engine) DeleteIdleRunnerTx(ctx context.Context, tx pgx.Tx, orgID, runnerID string, reason tidev1.RunnerEndReason) (*Runner, error) {
	if _, err := e.GetRunner(ctx, tx, orgID, runnerID); err != nil {
		return nil, err
	}
	cur, err := e.lockRunner(ctx, tx, runnerID)
	if err != nil || cur.RequestID != "" {
		return nil, err
	}
	return e.setRunnerTx(ctx, tx, cur.ID, []tidev1.RunnerState{runCreating, runPrep, runWaiting, runWarm}, runDeleting, "end_reason = $4", int16(reason))
}

// startRequestTx moves a request to STARTING or RUNNING with its runner.
func (e *Engine) startRequestTx(ctx context.Context, tx pgx.Tx, requestID, runnerID string, from []tidev1.RequestState, to tidev1.RequestState) error {
	req, err := e.lockRequest(ctx, tx, requestID)
	if err != nil {
		return err
	}
	ok := false
	for _, s := range from {
		ok = ok || req.State == s
	}
	if !ok {
		return nil
	}
	prev := req.State
	now := e.svc.NowUTC()
	extra := ""
	if to == reqRunning {
		extra = ", start_time = $4, work_order_key_id = NULL, work_order_ciphertext = NULL"
	}
	req, err = scanRequest(tx.QueryRow(ctx, "UPDATE requests SET state = $2, runner_id = $3, update_time = $4"+extra+" WHERE request_id = $1 RETURNING "+requestColumns,
		requestID, int16(to), runnerID, now))
	if err != nil {
		return err
	}
	e.logRequest(ctx, req, prev)
	if prev == reqQueued {
		e.m.QueueWait(kindLabel(req.Kind), priorityLabel(req.Priority), now.Sub(req.QueueTime).Seconds())
	}
	return nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// isCode reports whether err has the Connect code.
func isCode(err error, code connect.Code) bool { return err != nil && connect.CodeOf(err) == code }
