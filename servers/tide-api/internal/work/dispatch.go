// SPDX-License-Identifier: FSL-1.1-ALv2

package work

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/servers/tide-api/internal/api"
	"github.com/delinoio/mono/servers/tide-api/internal/ownsandclient"
	"github.com/delinoio/mono/servers/tide-api/internal/profile"
)

// RunDispatcher is the dispatcher lease component. It runs a pass for an
// organization when Kick names it and a pass for every organization with
// queued requests or warm runner profiles at least every PassInterval.
func (e *Engine) RunDispatcher(ctx context.Context) error {
	t := time.NewTicker(e.timing.PassInterval)
	defer t.Stop()
	e.passAll(ctx)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case orgID := <-e.kicks:
			e.Pass(ctx, orgID)
		case <-t.C:
			e.passAll(ctx)
		}
	}
}

func (e *Engine) passAll(ctx context.Context) {
	rows, err := e.svc.DB.Pool.Query(ctx, `SELECT organization_id::text FROM requests WHERE state = $1
		UNION SELECT organization_id::text FROM runner_profiles WHERE warm_count > 0
		UNION SELECT organization_id::text FROM runners WHERE state = $2`, int16(reqQueued), int16(runWarm))
	if err != nil {
		if ctx.Err() == nil {
			e.svc.Logger.WarnContext(ctx, "dispatcher_list_failed", slog.Any("error", err))
		}
		return
	}
	var orgs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			orgs = append(orgs, id)
		}
	}
	rows.Close()
	for _, org := range orgs {
		if ctx.Err() != nil {
			return
		}
		e.Pass(ctx, org)
	}
}

// Pass runs one dispatcher pass for an organization: it ends the requests
// whose maximum queue wait ended, dispatches the uncovered requests in queue
// order and creates warm runners when no request is uncovered.
func (e *Engine) Pass(ctx context.Context, orgID string) {
	if err := e.pass(ctx, orgID); err != nil && ctx.Err() == nil {
		e.svc.Logger.WarnContext(ctx, "dispatcher_pass_failed", slog.String("organization_id", orgID), slog.Any("error", err))
	}
}

func (e *Engine) pass(ctx context.Context, orgID string) error {
	if err := e.expire(ctx, orgID); err != nil {
		return err
	}
	lk, ok, err := e.links.Get(ctx, e.svc.DB.Pool, orgID)
	if err != nil || !ok || !lk.Dispatchable() {
		return err
	}
	client, _, err := e.links.Client(ctx, orgID)
	if err != nil {
		return err
	}
	profiles, err := e.profiles.ListAll(ctx, e.svc.DB.Pool, orgID)
	if err != nil {
		return err
	}
	byID := map[string]*profile.Profile{}
	for _, p := range profiles {
		byID[p.ID] = p
	}
	queued, err := e.queue(ctx, orgID)
	if err != nil {
		return err
	}
	runners, err := e.openRunners(ctx, orgID)
	if err != nil {
		return err
	}
	uncovered := e.uncovered(queued, runners)
	reclaimed := false
	for _, req := range uncovered {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		p := byID[req.ProfileID]
		a := e.adapters[req.Kind]
		if p == nil || a == nil {
			// A deleted profile's requests end in its deletion hook; a kind
			// without an adapter waits until its maximum queue wait.
			continue
		}
		if a.BindsAtDispatch() {
			if warm := oldestWarm(runners, p.ID); warm != nil {
				if err := e.assignWarm(ctx, req, warm); err != nil {
					e.svc.Logger.WarnContext(ctx, "dispatch_assign_failed", slog.String("request_id", req.ID), slog.Any("error", err))
				}
				warm.RequestID = req.ID
				continue
			}
		}
		res := e.createRunner(ctx, client, p, req)
		if res == createUnavailable && !reclaimed {
			reclaimed = e.reclaimWarm(ctx, runners, p.ID)
		}
	}
	if len(uncovered) > 0 {
		return nil
	}
	// No uncovered request: fill the warm counts.
	for _, p := range profiles {
		a := e.adapters[p.Kind]
		if a == nil || p.WarmCount == 0 {
			continue
		}
		idle := 0
		for _, r := range runners {
			if r.ProfileID == p.ID && r.RequestID == "" && (r.State == runCreating || r.State == runPrep || r.State == runWarm) {
				idle++
			}
		}
		for ; idle < int(p.WarmCount); idle++ {
			if e.createRunner(ctx, client, p, nil) != createOK {
				return nil
			}
		}
	}
	return nil
}

// expire ends the QUEUED requests whose maximum queue wait ended.
func (e *Engine) expire(ctx context.Context, orgID string) error {
	rows, err := e.svc.DB.Pool.Query(ctx, "SELECT request_id::text FROM requests WHERE organization_id = $1 AND state = $2 AND queue_deadline <= $3",
		orgID, int16(reqQueued), e.svc.NowUTC())
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		var ended *Request
		err := e.svc.DB.InTx(ctx, func(tx pgx.Tx) error {
			r, err := e.lockRequest(ctx, tx, id)
			if err != nil || r.State != reqQueued {
				return err
			}
			ended, _, err = e.endRequestTx(ctx, tx, id, RequestEnd{State: reqExpired, Reason: tidev1.RequestEndReason_REQUEST_END_REASON_MAX_QUEUE_WAIT})
			return err
		})
		if err != nil {
			return err
		}
		if ended != nil {
			e.releaseClaim(ctx, ended)
		}
	}
	return nil
}

// queue returns the QUEUED requests in queue order: priority (HIGH first),
// queue time (oldest first), request ID.
func (e *Engine) queue(ctx context.Context, orgID string) ([]*Request, error) {
	rows, err := e.svc.DB.Pool.Query(ctx, "SELECT "+requestColumns+` FROM requests WHERE organization_id = $1 AND state = $2
		ORDER BY priority, queue_time, request_id`, orgID, int16(reqQueued))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Request
	for rows.Next() {
		r, err := scanRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (e *Engine) openRunners(ctx context.Context, orgID string) ([]*Runner, error) {
	rows, err := e.svc.DB.Pool.Query(ctx, "SELECT "+runnerColumns+" FROM runners WHERE organization_id = $1 AND state = ANY($2) ORDER BY create_time, runner_id",
		orgID, openRunnerStates)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Runner
	for rows.Next() {
		r, err := scanRunner(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// uncovered returns the queued requests that no runner covers, in queue
// order. A request of a binding kind is covered only by its own runner, so
// every QUEUED one is uncovered. For the other kinds, the oldest QUEUED
// requests of a runner pool count as covered while the pool has as many
// runners in CREATING, PREPARING or WAITING_FOR_WORK.
func (e *Engine) uncovered(queued []*Request, runners []*Runner) []*Request {
	pools := map[string]int{}
	for _, r := range runners {
		a := e.adapters[r.Kind]
		if a == nil || a.BindsAtDispatch() || r.RequestID != "" {
			continue
		}
		if r.State == runCreating || r.State == runPrep || r.State == runWaiting {
			pools[r.ProfileID+"\x00"+r.PoolKey]++
		}
	}
	// Coverage goes to the oldest requests of a pool, not to the queue
	// order across priorities.
	byAge := append([]*Request(nil), queued...)
	sort.SliceStable(byAge, func(i, j int) bool {
		if !byAge[i].QueueTime.Equal(byAge[j].QueueTime) {
			return byAge[i].QueueTime.Before(byAge[j].QueueTime)
		}
		return byAge[i].ID < byAge[j].ID
	})
	covered := map[string]bool{}
	for _, r := range runners {
		// A QUEUED request whose runner is being created is taken.
		if r.RequestID != "" {
			covered[r.RequestID] = true
		}
	}
	for _, req := range byAge {
		a := e.adapters[req.Kind]
		if a == nil || a.BindsAtDispatch() {
			continue
		}
		key := req.ProfileID + "\x00" + req.PoolKey
		if pools[key] > 0 {
			pools[key]--
			covered[req.ID] = true
		}
	}
	var out []*Request
	for _, req := range queued {
		if !covered[req.ID] {
			out = append(out, req)
		}
	}
	return out
}

func oldestWarm(runners []*Runner, profileID string) *Runner {
	for _, r := range runners {
		if r.ProfileID == profileID && r.State == runWarm && r.RequestID == "" {
			return r
		}
	}
	return nil
}

// assignWarm binds a warm runner to a request; the runner's driver then
// starts the work.
func (e *Engine) assignWarm(ctx context.Context, req *Request, warm *Runner) error {
	err := e.svc.DB.InTx(ctx, func(tx pgx.Tx) error {
		if err := api.LockOrganizationShare(ctx, tx, req.OrganizationID); err != nil {
			return err
		}
		r, err := e.lockRunner(ctx, tx, warm.ID)
		if err != nil || r.State != runWarm || r.RequestID != "" {
			return err
		}
		cur, err := e.lockRequest(ctx, tx, req.ID)
		if err != nil || cur.State != reqQueued {
			return err
		}
		if _, err := tx.Exec(ctx, "UPDATE runners SET request_id = $2, update_time = $3 WHERE runner_id = $1", r.ID, req.ID, e.svc.NowUTC()); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "UPDATE requests SET start_attempts = start_attempts + 1 WHERE request_id = $1", req.ID); err != nil {
			return err
		}
		return e.startRequestTx(ctx, tx, req.ID, r.ID, []tidev1.RequestState{reqQueued}, reqStarting)
	})
	if err == nil {
		e.kickRunner(warm.ID)
	}
	return err
}

type createResult int

const (
	createOK createResult = iota
	createUnavailable
	createFailed
	createSkipped
)

// sandboxRequest builds the CreateSandbox request of a runner: no device
// ID, the profile's source, resources, network and TTLs, and the tide
// labels.
func sandboxRequest(p *profile.Profile, r *Runner) *ownsandv1.CreateSandboxRequest {
	req := &ownsandv1.CreateSandboxRequest{
		IdempotencyKey: r.IdempotencyKey,
		Source:         &ownsandv1.SandboxSource{},
		Resources:      &ownsandv1.Resources{VcpuCount: p.Resources.VCPUCount, MemoryMib: p.Resources.MemoryMiB, DiskGib: p.Resources.DiskGiB},
		IdleTtl:        durationpb.New(p.IdleTTL),
		MaxTtl:         durationpb.New(p.MaxTTL),
		Network:        &ownsandv1.Network{Mode: ownsandv1.NetworkMode_NETWORK_MODE_OUTBOUND},
		Labels: map[string]string{
			ownsandclient.LabelOrganizationID: r.OrganizationID, ownsandclient.LabelRunnerID: r.ID, ownsandclient.LabelRunnerProfileID: p.ID,
		},
	}
	if p.TemplateID != "" {
		req.Source.Source = &ownsandv1.SandboxSource_TemplateId{TemplateId: p.TemplateID}
	} else {
		req.Source.Source = &ownsandv1.SandboxSource_BaseImage{BaseImage: profile.OwnSandBaseImage(p.BaseImage)}
	}
	return req
}

// createRunner stores a new runner with its idempotency key, binds the
// request for binding kinds, and calls CreateSandbox. req is nil for a warm
// runner.
func (e *Engine) createRunner(ctx context.Context, client ownsandclient.Client, p *profile.Profile, req *Request) createResult {
	a := e.adapters[p.Kind]
	r := &Runner{
		ID: api.NewID(), OrganizationID: p.OrganizationID, ProfileID: p.ID, Kind: p.Kind, GuestOS: p.GuestOS,
		IdempotencyKey: api.NewID(), WorkspacePath: p.WorkspacePath,
	}
	bind := req != nil && a.BindsAtDispatch()
	if req != nil {
		r.PoolKey = req.PoolKey
	}
	// The request is stored with the runner, so every retry sends the same
	// parameters with the key.
	sreq := sandboxRequest(p, r)
	createReq, err := proto.MarshalOptions{Deterministic: true}.Marshal(sreq)
	if err != nil {
		e.svc.Logger.WarnContext(ctx, "dispatch_create_runner_failed", slog.String("organization_id", p.OrganizationID), slog.Any("error", err))
		return createFailed
	}
	err = e.svc.DB.InTx(ctx, func(tx pgx.Tx) error {
		if err := api.LockOrganizationShare(ctx, tx, p.OrganizationID); err != nil {
			return err
		}
		// The link row lock serializes the creation with RemoveOwnSandLink
		// and organization deletion, which mark the link as being removed
		// before they delete runners.
		var removing bool
		var state int16
		if err := tx.QueryRow(ctx, "SELECT removing, state FROM ownsand_links WHERE organization_id = $1 FOR SHARE", p.OrganizationID).Scan(&removing, &state); err != nil {
			return err
		}
		if removing || tidev1.OwnSandLinkState(state) != tidev1.OwnSandLinkState_OWN_SAND_LINK_STATE_ACTIVE {
			return errSkip
		}
		if bind {
			// The request stays QUEUED until OwnSand created the sandbox, so
			// a request that finds no capacity does not change state; the
			// bound runner keeps the next pass from dispatching it again.
			cur, err := e.lockRequest(ctx, tx, req.ID)
			if err != nil {
				return err
			}
			if cur.State != reqQueued {
				return errSkip
			}
			r.RequestID = req.ID
		}
		now := e.svc.NowUTC()
		created, err := scanRunner(tx.QueryRow(ctx, `INSERT INTO runners (runner_id, organization_id, runner_profile_id, kind, guest_os, state, request_id, pool_key,
			idempotency_key, create_request, workspace_path, create_time, update_time) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $12) RETURNING `+runnerColumns,
			r.ID, r.OrganizationID, r.ProfileID, int16(r.Kind), int16(r.GuestOS), int16(runCreating), nullable(r.RequestID), nullable(r.PoolKey),
			r.IdempotencyKey, createReq, r.WorkspacePath, now))
		if err != nil {
			return err
		}
		*r = *created
		e.logRunner(ctx, r, tidev1.RunnerState_RUNNER_STATE_UNSPECIFIED)
		return nil
	})
	if errors.Is(err, errSkip) {
		return createSkipped
	}
	if err != nil {
		e.svc.Logger.WarnContext(ctx, "dispatch_create_runner_failed", slog.String("organization_id", p.OrganizationID), slog.Any("error", err))
		return createFailed
	}
	sb, err := client.CreateSandbox(ctx, sreq)
	return e.applyCreate(ctx, r, sb, err)
}

// storedCreateRequest returns the CreateSandbox request that the runner was
// created with.
func (e *Engine) storedCreateRequest(ctx context.Context, runnerID string) (*ownsandv1.CreateSandboxRequest, error) {
	var raw []byte
	if err := e.svc.DB.Pool.QueryRow(ctx, "SELECT create_request FROM runners WHERE runner_id = $1", runnerID).Scan(&raw); err != nil {
		return nil, err
	}
	req := &ownsandv1.CreateSandboxRequest{}
	if err := proto.Unmarshal(raw, req); err != nil {
		return nil, err
	}
	return req, nil
}

var errSkip = errors.New("skip")

// applyCreate applies a CreateSandbox result to a stored CREATING runner.
func (e *Engine) applyCreate(ctx context.Context, r *Runner, sb *ownsandv1.Sandbox, err error) createResult {
	switch {
	case err == nil:
		uerr := e.svc.DB.InTx(ctx, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, "UPDATE runners SET ownsand_sandbox_id = $2, ownsand_device_id = $3, update_time = $4 WHERE runner_id = $1 AND ownsand_sandbox_id IS NULL",
				r.ID, sb.GetSandboxId(), sb.GetDeviceId(), e.svc.NowUTC())
			if err != nil || tag.RowsAffected() == 0 {
				return err
			}
			cur, err := e.lockRunner(ctx, tx, r.ID)
			if err != nil || cur.State != runCreating {
				// A deleted runner keeps the sandbox ID, so its driver deletes
				// the sandbox, and starts nothing.
				return err
			}
			return e.bindStartTx(ctx, tx, cur)
		})
		if uerr != nil {
			// The driver repeats CreateSandbox with the same key and gets
			// the same sandbox.
			e.svc.Logger.WarnContext(ctx, "runner_sandbox_store_failed", slog.String("runner_id", r.ID), slog.Any("error", uerr))
		}
		e.ensureDriver(r.ID)
		e.kickRunner(r.ID)
		return createOK
	case isCode(err, connect.CodeUnavailable):
		// No device has free capacity: the sandbox does not exist, so the
		// runner is removed and its request returns to the queue.
		e.dropRunner(ctx, r, false)
		return createUnavailable
	case isCode(err, connect.CodeFailedPrecondition):
		// No device of the OwnSand organization can ever run the profile.
		e.dropRunner(ctx, r, true)
		return createFailed
	case isCode(err, connect.CodeInvalidArgument), isCode(err, connect.CodeNotFound), isCode(err, connect.CodePermissionDenied):
		// OwnSand rejected the request (for example, an unknown template):
		// a start failure that counts as an attempt.
		if berr := e.svc.DB.InTx(ctx, func(tx pgx.Tx) error { return e.bindStartTx(ctx, tx, r) }); berr != nil {
			e.svc.Logger.WarnContext(ctx, "runner_update_failed", slog.String("runner_id", r.ID), slog.Any("error", berr))
		}
		e.startFailed(ctx, r.ID, "create_sandbox_rejected")
		return createFailed
	default:
		// The response may be lost: the runner stays CREATING with its key,
		// and its driver repeats the call.
		e.ensureDriver(r.ID)
		return createFailed
	}
}

// dropRunner removes a runner whose sandbox was never created. noDevice
// ends the request with NO_ELIGIBLE_DEVICE; otherwise the request returns to
// the queue without counting an attempt.
func (e *Engine) dropRunner(ctx context.Context, r *Runner, noDevice bool) {
	var ended *Request
	err := e.svc.DB.InTx(ctx, func(tx pgx.Tx) error {
		cur, err := e.lockRunner(ctx, tx, r.ID)
		if err != nil || cur.State != runCreating {
			return err
		}
		if noDevice {
			if _, err := e.setRunnerTx(ctx, tx, r.ID, []tidev1.RunnerState{runCreating}, runFailed, "end_reason = $4",
				int16(tidev1.RunnerEndReason_RUNNER_END_REASON_START_FAILED)); err != nil {
				return err
			}
		} else if _, err := tx.Exec(ctx, "DELETE FROM runners WHERE runner_id = $1", r.ID); err != nil {
			return err
		}
		if cur.RequestID == "" {
			return nil
		}
		if noDevice {
			ended, _, err = e.endRequestTx(ctx, tx, cur.RequestID, RequestEnd{State: reqFailed, Reason: tidev1.RequestEndReason_REQUEST_END_REASON_NO_ELIGIBLE_DEVICE})
			return err
		}
		// The request stayed QUEUED.
		return nil
	})
	if err != nil {
		e.svc.Logger.WarnContext(ctx, "dispatch_drop_runner_failed", slog.String("runner_id", r.ID), slog.Any("error", err))
	}
	if ended != nil {
		e.releaseClaim(ctx, ended)
	}
}

// bindStartTx starts the request of a runner whose sandbox OwnSand created:
// the request is STARTING with the runner and one more start attempt.
func (e *Engine) bindStartTx(ctx context.Context, tx pgx.Tx, r *Runner) error {
	if r.RequestID == "" {
		return nil
	}
	req, err := e.lockRequest(ctx, tx, r.RequestID)
	if err != nil || req.State != reqQueued {
		return err
	}
	if _, err := tx.Exec(ctx, "UPDATE requests SET start_attempts = start_attempts + 1 WHERE request_id = $1", req.ID); err != nil {
		return err
	}
	return e.startRequestTx(ctx, tx, req.ID, r.ID, []tidev1.RequestState{reqQueued}, reqStarting)
}

// requeueTx returns a STARTING request to QUEUED with its original queue
// time after a start failure.
func (e *Engine) requeueTx(ctx context.Context, tx pgx.Tx, requestID string) error {
	req, err := e.lockRequest(ctx, tx, requestID)
	if err != nil || req.State != reqStarting {
		return err
	}
	r, err := scanRequest(tx.QueryRow(ctx, "UPDATE requests SET state = $2, runner_id = NULL, update_time = $3 WHERE request_id = $1 RETURNING "+requestColumns,
		requestID, int16(reqQueued), e.svc.NowUTC()))
	if err != nil {
		return err
	}
	e.logRequest(ctx, r, reqStarting)
	return nil
}

// reclaimWarm deletes one warm runner of another profile after OwnSand had
// no capacity: the one of the lowest priority profile first, then the
// oldest. It reports whether it deleted one.
func (e *Engine) reclaimWarm(ctx context.Context, runners []*Runner, profileID string) bool {
	var best *Runner
	var bestPriority tidev1.Priority
	for _, r := range runners {
		if r.ProfileID == profileID || r.State != runWarm || r.RequestID != "" {
			continue
		}
		p, err := e.profiles.Get(ctx, e.svc.DB.Pool, r.OrganizationID, r.ProfileID)
		prio := tidev1.Priority_PRIORITY_LOW + 1 // a deleted profile goes first
		if err == nil {
			prio = p.Priority
		}
		// Priority values grow from HIGH (1) to LOW (3): the larger value is
		// the lower priority. runners is oldest first, so ">" keeps the
		// oldest of equal priority.
		if best == nil || prio > bestPriority {
			best, bestPriority = r, prio
		}
	}
	if best == nil {
		return false
	}
	if _, err := e.deleteRunner(ctx, best.OrganizationID, best.ID, tidev1.RunnerEndReason_RUNNER_END_REASON_WARM_RECLAIMED, 0); err != nil {
		e.svc.Logger.WarnContext(ctx, "warm_reclaim_failed", slog.String("runner_id", best.ID), slog.Any("error", err))
		return false
	}
	best.State = runDeleting
	return true
}
