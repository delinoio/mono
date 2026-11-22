// SPDX-License-Identifier: FSL-1.1-ALv2

package work

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	"github.com/delinoio/mono/go/ownsand"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/servers/tide-api/internal/ownsandclient"
)

// RunReconciler is the runner reconciler lease component: it starts a
// driver for every runner that is not ended and keeps them running until ctx
// ends, then waits for them.
func (e *Engine) RunReconciler(ctx context.Context) error {
	wg := &sync.WaitGroup{}
	e.mu.Lock()
	e.runCtx, e.runWG = ctx, wg
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.runCtx, e.runWG = nil, nil
		e.mu.Unlock()
		wg.Wait()
	}()
	t := time.NewTicker(e.timing.ReconcileInterval)
	defer t.Stop()
	for {
		e.reconcile(ctx)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

func (e *Engine) reconcile(ctx context.Context) {
	rows, err := e.svc.DB.Pool.Query(ctx, "SELECT runner_id::text FROM runners WHERE state = ANY($1)", openRunnerStates)
	if err != nil {
		if ctx.Err() == nil {
			e.svc.Logger.WarnContext(ctx, "reconcile_list_failed", slog.Any("error", err))
		}
		return
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
		e.ensureDriver(id)
	}
}

// ensureDriver starts the driver of a runner when the reconciler runs on
// this instance (the lease holder) and no driver of the runner runs.
func (e *Engine) ensureDriver(runnerID string) {
	e.mu.Lock()
	ctx, wg := e.runCtx, e.runWG
	if ctx == nil || ctx.Err() != nil {
		e.mu.Unlock()
		return
	}
	if _, ok := e.drivers[runnerID]; ok {
		e.mu.Unlock()
		return
	}
	kick := make(chan struct{}, 1)
	e.drivers[runnerID] = kick
	e.mu.Unlock()
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			e.mu.Lock()
			delete(e.drivers, runnerID)
			e.mu.Unlock()
		}()
		e.drive(ctx, runnerID, kick)
	}()
}

// drive moves one runner through its lifecycle until it is ended or ctx
// ends. Every state change is a compare-and-set, so an RPC that deletes the
// runner meanwhile wins and the driver follows the new state.
func (e *Engine) drive(ctx context.Context, runnerID string, kick chan struct{}) {
	// followCtx ends the program follower when the driver returns.
	followCtx, stopFollow := context.WithCancel(ctx)
	defer stopFollow()
	following := false
	wait := func(d time.Duration) bool {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return false
		case <-kick:
		case <-t.C:
		}
		return true
	}
	for ctx.Err() == nil {
		r, err := scanRunner(e.svc.DB.Pool.QueryRow(ctx, "SELECT "+runnerColumns+" FROM runners WHERE runner_id = $1", runnerID))
		if err != nil {
			if err == pgx.ErrNoRows {
				return
			}
			if !wait(e.timing.SandboxPoll) {
				return
			}
			continue
		}
		if RunnerTerminal(r.State) {
			return
		}
		client, _, err := e.links.ClientForRemoval(ctx, r.OrganizationID)
		if err != nil {
			if r.State == runDeleting && connect.CodeOf(err) == connect.CodeFailedPrecondition {
				// The link and its key are gone: the sandbox cannot be
				// deleted through tide anymore (its idle_ttl ends it).
				e.finishDelete(ctx, r.ID)
				return
			}
			if !wait(e.timing.SandboxPoll) {
				return
			}
			continue
		}
		if e.orphaned(ctx, r) {
			continue
		}
		next := e.timing.SandboxPoll
		switch r.State {
		case runCreating:
			next = e.stepCreating(ctx, client, r)
		case runPrep:
			e.stepPreparing(ctx, client, r)
			next = 0
		case runWarm:
			if r.RequestID != "" {
				e.stepStart(ctx, client, r)
				next = 0
			} else {
				e.checkSandbox(ctx, client, r)
			}
		case runWaiting, runBusy:
			if r.ProgramExecID != "" && !following {
				following = true
				go e.followProgram(followCtx, client, r)
			}
			e.checkSandbox(ctx, client, r)
		case runDeleting:
			e.stepDeleting(ctx, client, r)
		}
		if next > 0 && !wait(next) {
			return
		}
	}
}

// orphaned deletes a runner that was created for a request that ended
// before the runner started its work (for example, a dev session that was
// deleted while its sandbox was created). It reports whether it did.
func (e *Engine) orphaned(ctx context.Context, r *Runner) bool {
	if r.RequestID == "" || (r.State != runCreating && r.State != runPrep && r.State != runWarm) {
		return false
	}
	req, err := e.GetRequest(ctx, e.svc.DB.Pool, r.OrganizationID, r.RequestID)
	if err != nil || !RequestTerminal(req.State) {
		return false
	}
	if _, err := e.deleteRunner(ctx, r.OrganizationID, r.ID, tidev1.RunnerEndReason_RUNNER_END_REASON_CLEAN_UP, 0); err != nil {
		e.svc.Logger.WarnContext(ctx, "runner_update_failed", slog.String("runner_id", r.ID), slog.Any("error", err))
		return false
	}
	return true
}

// stepCreating creates the sandbox (again with the stored request and key
// when the response was lost) and waits for RUNNING.
func (e *Engine) stepCreating(ctx context.Context, client ownsandclient.Client, r *Runner) time.Duration {
	if r.SandboxID == "" {
		// The stored request, not the profile as it is now: OwnSand rejects
		// the key with other parameters, and a deleted profile must not
		// leave a created sandbox behind.
		req, err := e.storedCreateRequest(ctx, r.ID)
		if err != nil {
			e.svc.Logger.WarnContext(ctx, "runner_create_request_load_failed", slog.String("runner_id", r.ID), slog.Any("error", err))
			return e.timing.SandboxPoll
		}
		sb, err := client.CreateSandbox(ctx, req)
		switch e.applyCreate(ctx, r, sb, err) {
		case createOK:
			return 0
		default:
			return e.timing.SandboxPoll
		}
	}
	sb, err := client.GetSandbox(ctx, r.SandboxID)
	if err != nil {
		if isCode(err, connect.CodeNotFound) {
			e.startFailed(ctx, r.ID, "sandbox_not_found")
		}
		return e.timing.SandboxPoll
	}
	switch {
	case sb.GetState() == ownsandv1.SandboxState_SANDBOX_STATE_RUNNING:
		err := e.svc.DB.InTx(ctx, func(tx pgx.Tx) error {
			_, err := e.setRunnerTx(ctx, tx, r.ID, []tidev1.RunnerState{runCreating}, runPrep, "ownsand_device_id = $4", sb.GetDeviceId())
			return err
		})
		if err != nil {
			e.svc.Logger.WarnContext(ctx, "runner_update_failed", slog.String("runner_id", r.ID), slog.Any("error", err))
		}
		return 0
	case ownsand.IsTerminal(sb.GetState()):
		e.startFailed(ctx, r.ID, "sandbox_ended_while_creating")
		return 0
	case e.svc.NowUTC().Sub(r.CreateTime) > e.timing.StartTimeout:
		e.startFailed(ctx, r.ID, "sandbox_start_timeout")
		return 0
	}
	return min(e.timing.SandboxPoll, 2*time.Second)
}

func (e *Engine) guest(ctx context.Context, client ownsandclient.Client, r *Runner) *Guest {
	g := &Guest{Client: client, Runner: r}
	if r.RequestID != "" {
		g.Request, _ = e.GetRequest(ctx, e.svc.DB.Pool, r.OrganizationID, r.RequestID)
	}
	g.Profile, _ = e.profiles.Get(ctx, e.svc.DB.Pool, r.OrganizationID, r.ProfileID)
	return g
}

// stepPreparing runs Adapter.Prepare, then starts the work or makes the
// runner WARM.
func (e *Engine) stepPreparing(ctx context.Context, client ownsandclient.Client, r *Runner) {
	a := e.adapters[r.Kind]
	if a == nil {
		e.startFailed(ctx, r.ID, "no_adapter")
		return
	}
	if err := a.Prepare(ctx, e.guest(ctx, client, r)); err != nil {
		if ctx.Err() == nil {
			e.svc.Logger.WarnContext(ctx, "runner_prepare_failed", slog.String("runner_id", r.ID), slog.Any("error", err))
			e.startFailed(ctx, r.ID, "prepare_failed")
		}
		return
	}
	if r.RequestID != "" || !a.BindsAtDispatch() {
		e.stepStart(ctx, client, r)
		return
	}
	err := e.svc.DB.InTx(ctx, func(tx pgx.Tx) error {
		_, err := e.setRunnerTx(ctx, tx, r.ID, []tidev1.RunnerState{runPrep}, runWarm, "")
		return err
	})
	if err != nil {
		e.svc.Logger.WarnContext(ctx, "runner_update_failed", slog.String("runner_id", r.ID), slog.Any("error", err))
	}
	e.Kick(r.OrganizationID)
}

// stepStart runs Adapter.Start (lifecycle step 3).
func (e *Engine) stepStart(ctx context.Context, client ownsandclient.Client, r *Runner) {
	a := e.adapters[r.Kind]
	res, err := a.Start(ctx, e.guest(ctx, client, r))
	if err != nil {
		if ctx.Err() == nil {
			e.svc.Logger.WarnContext(ctx, "runner_start_failed", slog.String("runner_id", r.ID), slog.Any("error", err))
			e.startFailed(ctx, r.ID, "start_failed")
		}
		return
	}
	err = e.svc.DB.InTx(ctx, func(tx pgx.Tx) error {
		from := []tidev1.RunnerState{runPrep, runWarm}
		if res.Busy {
			n, err := e.setRunnerTx(ctx, tx, r.ID, from, runBusy, "")
			if err != nil || n == nil || n.RequestID == "" {
				return err
			}
			return e.startRequestTx(ctx, tx, n.RequestID, n.ID, []tidev1.RequestState{reqStarting}, reqRunning)
		}
		_, err := e.setRunnerTx(ctx, tx, r.ID, from, runWaiting, "program_exec_id = $4", nullable(res.ProgramExecID))
		return err
	})
	if err != nil {
		e.svc.Logger.WarnContext(ctx, "runner_update_failed", slog.String("runner_id", r.ID), slog.Any("error", err))
	}
}

// checkSandbox ends the runner when OwnSand ended its sandbox.
func (e *Engine) checkSandbox(ctx context.Context, client ownsandclient.Client, r *Runner) {
	sb, err := client.GetSandbox(ctx, r.SandboxID)
	if err != nil {
		if isCode(err, connect.CodeNotFound) {
			e.sandboxEnded(ctx, r.ID, ownsandv1.SandboxEndReason_SANDBOX_END_REASON_UNSPECIFIED)
		}
		return
	}
	if ownsand.IsTerminal(sb.GetState()) {
		e.sandboxEnded(ctx, r.ID, sb.GetEndReason())
	}
}

func ownsandReasonLabel(reason ownsandv1.SandboxEndReason) string {
	if reason == 0 {
		return "UNKNOWN"
	}
	return label(reason, "SANDBOX_END_REASON_")
}

// sandboxEnded applies an end of the sandbox that tide did not ask for
// (lifecycle step 6).
func (e *Engine) sandboxEnded(ctx context.Context, runnerID string, reason ownsandv1.SandboxEndReason) {
	var ended *Request
	var orgID string
	err := e.svc.DB.InTx(ctx, func(tx pgx.Tx) error {
		r, err := e.lockRunner(ctx, tx, runnerID)
		if err != nil || RunnerTerminal(r.State) || r.State == runDeleting {
			return err
		}
		orgID = r.OrganizationID
		a := e.adapters[r.Kind]
		var req *Request
		if r.RequestID != "" {
			req, _ = e.lockRequest(ctx, tx, r.RequestID)
		}
		out := BaseAdapter{}.SandboxEnded(req, reason)
		if a != nil {
			out = a.SandboxEnded(req, reason)
		}
		if _, err := e.setRunnerTx(ctx, tx, r.ID, []tidev1.RunnerState{r.State}, out.RunnerState, "end_reason = $4, ownsand_end_reason = $5",
			int16(out.RunnerReason), ownsandReasonLabel(reason)); err != nil {
			return err
		}
		if req != nil && !out.KeepRequest {
			ended, _, err = e.endRequestTx(ctx, tx, req.ID, out.Request)
		}
		return err
	})
	if err != nil {
		e.svc.Logger.WarnContext(ctx, "runner_sandbox_end_failed", slog.String("runner_id", runnerID), slog.Any("error", err))
		return
	}
	if ended != nil {
		e.svc.Logger.InfoContext(ctx, "runner_sandbox_ended", slog.String("runner_id", runnerID), slog.String("request_id", ended.ID),
			slog.String("ownsand_end_reason", ownsandReasonLabel(reason)))
	}
	if orgID != "" {
		e.Kick(orgID)
	}
}

// followProgram follows the runner program with AttachExec. The SDK
// resumes the stream after each relay rotation; when the result arrives,
// the request ends as the adapter says and tide deletes the sandbox. When
// tide misses the exit (for example, the exec result expired), the
// profile's idle_ttl deletes the sandbox.
func (e *Engine) followProgram(ctx context.Context, client ownsandclient.Client, r *Runner) {
	for ctx.Err() == nil {
		res, err := client.AttachExec(ctx, r.SandboxID, r.ProgramExecID, ownsand.AttachParams{OnEvent: func(ownsand.ExecEvent) error { return nil }})
		if err != nil {
			if ctx.Err() != nil || isCode(err, connect.CodeNotFound) {
				return
			}
			t := time.NewTimer(e.timing.SandboxPoll)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
			continue
		}
		e.programExited(ctx, r.ID, res)
		return
	}
}

func (e *Engine) programExited(ctx context.Context, runnerID string, res *ownsand.ExecResult) {
	var orgID string
	err := e.svc.DB.InTx(ctx, func(tx pgx.Tx) error {
		r, err := e.lockRunner(ctx, tx, runnerID)
		if err != nil || (r.State != runWaiting && r.State != runBusy) {
			return err
		}
		orgID = r.OrganizationID
		if res.Kind == ownsand.ExecResultSandboxEnded {
			return nil
		}
		if r.RequestID != "" {
			if a := e.adapters[r.Kind]; a != nil {
				req, err := e.lockRequest(ctx, tx, r.RequestID)
				if err != nil {
					return err
				}
				if end, keep := a.ProgramExited(req, res); !keep {
					if _, _, err := e.endRequestTx(ctx, tx, req.ID, end); err != nil {
						return err
					}
				}
			}
		}
		_, err = e.setRunnerTx(ctx, tx, r.ID, []tidev1.RunnerState{runWaiting, runBusy}, runDeleting, "end_reason = $4", int16(tidev1.RunnerEndReason_RUNNER_END_REASON_WORK_DONE))
		return err
	})
	if err != nil {
		e.svc.Logger.WarnContext(ctx, "runner_program_exit_failed", slog.String("runner_id", runnerID), slog.Any("error", err))
	}
	e.kickRunner(runnerID)
	if orgID != "" {
		e.Kick(orgID)
	}
}

// stepDeleting deletes the sandbox and ends the runner.
func (e *Engine) stepDeleting(ctx context.Context, client ownsandclient.Client, r *Runner) {
	if e.removeSandbox(ctx, client, r) {
		e.finishDelete(ctx, r.ID)
	}
}

// removeSandbox deletes the sandbox of a DELETING runner and reports
// whether the runner can end. A runner without a stored sandbox ID first
// resolves its creation with resolveCreate.
func (e *Engine) removeSandbox(ctx context.Context, client ownsandclient.Client, r *Runner) bool {
	id := r.SandboxID
	if id == "" {
		var ok bool
		if id, ok = e.resolveCreate(ctx, client, r); !ok {
			return false
		}
		if id == "" {
			return true
		}
	}
	if _, err := client.DeleteSandbox(ctx, id); err != nil && !isCode(err, connect.CodeNotFound) {
		e.svc.Logger.WarnContext(ctx, "runner_sandbox_delete_failed", slog.String("runner_id", r.ID), slog.Any("error", err))
		return false
	}
	return true
}

// resolveCreate finds the sandbox of a runner that has no stored sandbox ID
// before its deletion ends: the CreateSandbox response may have been lost,
// or a call may still be in flight. It repeats CreateSandbox with the
// stored request and key, which returns the sandbox of the key (OwnSand
// serializes the calls of one key), or creates it when no call created it;
// the caller then deletes it. It stores the sandbox ID and returns it, ""
// when OwnSand created nothing for the key, and ok=false while the result
// is uncertain (the driver tries again).
func (e *Engine) resolveCreate(ctx context.Context, client ownsandclient.Client, r *Runner) (sandboxID string, ok bool) {
	req, err := e.storedCreateRequest(ctx, r.ID)
	if err != nil {
		e.svc.Logger.WarnContext(ctx, "runner_create_request_load_failed", slog.String("runner_id", r.ID), slog.Any("error", err))
		return "", false
	}
	sb, err := client.CreateSandbox(ctx, req)
	switch {
	case err == nil:
		if _, err := e.svc.DB.Pool.Exec(ctx, "UPDATE runners SET ownsand_sandbox_id = $2, ownsand_device_id = $3, update_time = $4 WHERE runner_id = $1 AND ownsand_sandbox_id IS NULL",
			r.ID, sb.GetSandboxId(), sb.GetDeviceId(), e.svc.NowUTC()); err != nil {
			e.svc.Logger.WarnContext(ctx, "runner_sandbox_store_failed", slog.String("runner_id", r.ID), slog.Any("error", err))
		}
		return sb.GetSandboxId(), true
	case rejected(err, connect.CodeUnavailable), rejected(err, connect.CodeFailedPrecondition), rejected(err, connect.CodeInvalidArgument),
		rejected(err, connect.CodeNotFound), rejected(err, connect.CodePermissionDenied):
		// OwnSand answered: the key created nothing.
		return "", true
	case isCode(err, connect.CodeUnauthenticated):
		// A key that OwnSand no longer accepts cannot reach the sandbox; its
		// idle_ttl ends it.
		return "", true
	}
	return "", false
}

func (e *Engine) finishDelete(ctx context.Context, runnerID string) {
	var orgID string
	err := e.svc.DB.InTx(ctx, func(tx pgx.Tx) error {
		r, err := e.setRunnerTx(ctx, tx, runnerID, []tidev1.RunnerState{runDeleting}, runDeleted, "")
		if r != nil {
			orgID = r.OrganizationID
		}
		return err
	})
	if err != nil {
		e.svc.Logger.WarnContext(ctx, "runner_update_failed", slog.String("runner_id", runnerID), slog.Any("error", err))
	}
	if orgID != "" {
		e.Kick(orgID)
	}
}

// startFailed applies a failure in lifecycle steps 1 to 3: the runner is
// FAILED, its sandbox is deleted, and its request returns to QUEUED with its
// original queue time, or ends with RUNNER_START_FAILED after
// MaxStartAttempts.
func (e *Engine) startFailed(ctx context.Context, runnerID, cause string) {
	var r *Runner
	var ended *Request
	err := e.svc.DB.InTx(ctx, func(tx pgx.Tx) error {
		cur, err := e.lockRunner(ctx, tx, runnerID)
		if err != nil || (cur.State != runCreating && cur.State != runPrep && cur.State != runWarm) {
			return err
		}
		r, err = e.setRunnerTx(ctx, tx, runnerID, []tidev1.RunnerState{cur.State}, runFailed, "end_reason = $4", int16(tidev1.RunnerEndReason_RUNNER_END_REASON_START_FAILED))
		if err != nil || r == nil || r.RequestID == "" {
			return err
		}
		req, err := e.lockRequest(ctx, tx, r.RequestID)
		if err != nil || req.State != reqStarting {
			return err
		}
		if req.StartAttempts >= MaxStartAttempts {
			ended, _, err = e.endRequestTx(ctx, tx, req.ID, RequestEnd{State: reqFailed, Reason: tidev1.RequestEndReason_REQUEST_END_REASON_RUNNER_START_FAILED})
			return err
		}
		return e.requeueTx(ctx, tx, req.ID)
	})
	if err != nil || r == nil {
		if err != nil {
			e.svc.Logger.WarnContext(ctx, "runner_start_failure_update_failed", slog.String("runner_id", runnerID), slog.Any("error", err))
		}
		return
	}
	e.svc.Logger.WarnContext(ctx, "runner_start_failure", slog.String("runner_id", runnerID), slog.String("organization_id", r.OrganizationID), slog.String("cause", cause))
	if r.SandboxID != "" {
		if client, _, err := e.links.ClientForRemoval(ctx, r.OrganizationID); err == nil {
			if _, err := client.DeleteSandbox(context.WithoutCancel(ctx), r.SandboxID); err != nil && !isCode(err, connect.CodeNotFound) {
				e.svc.Logger.WarnContext(ctx, "runner_sandbox_delete_failed", slog.String("runner_id", runnerID), slog.Any("error", err))
			}
		}
	}
	if ended != nil {
		e.releaseClaim(ctx, ended)
	}
	e.Kick(r.OrganizationID)
}

// deleteRunner changes an open runner to DELETING with reason and ends its
// request as the adapter says for cause (0: the runner has no request to
// end, for example a warm runner). It returns the runner, or nil when it was
// already ended. The driver, or DeleteRunnerNow, deletes the sandbox.
func (e *Engine) deleteRunner(ctx context.Context, orgID, runnerID string, reason tidev1.RunnerEndReason, cause DeleteCause) (*Runner, error) {
	var out *Runner
	var ended *Request
	var prevReq tidev1.RequestState
	err := e.svc.DB.InTx(ctx, func(tx pgx.Tx) error {
		var err error
		out, ended, prevReq, err = e.deleteRunnerTx(ctx, tx, orgID, runnerID, reason, cause)
		return err
	})
	if err != nil {
		return nil, err
	}
	if ended != nil && prevReq != reqRunning {
		e.releaseClaim(ctx, ended)
	}
	if out != nil {
		e.kickRunner(out.ID)
		e.Kick(orgID)
	}
	return out, nil
}

// deleteRunnerTx is deleteRunner in the caller's transaction.
func (e *Engine) deleteRunnerTx(ctx context.Context, tx pgx.Tx, orgID, runnerID string, reason tidev1.RunnerEndReason, cause DeleteCause) (*Runner, *Request, tidev1.RequestState, error) {
	cur, err := e.GetRunner(ctx, tx, orgID, runnerID)
	if err != nil {
		return nil, nil, 0, err
	}
	if RunnerTerminal(cur.State) || cur.State == runDeleting {
		return nil, nil, 0, nil
	}
	r, err := e.setRunnerTx(ctx, tx, runnerID, []tidev1.RunnerState{cur.State}, runDeleting, "end_reason = $4", int16(reason))
	if err != nil || r == nil {
		return nil, nil, 0, err
	}
	if r.RequestID == "" || cause == 0 {
		return r, nil, 0, nil
	}
	req, err := e.lockRequest(ctx, tx, r.RequestID)
	if err != nil {
		return nil, nil, 0, err
	}
	if RequestTerminal(req.State) {
		return r, nil, 0, nil
	}
	end, keep := BaseAdapter{}.RunnerDeleted(req, cause)
	if a := e.adapters[r.Kind]; a != nil {
		end, keep = a.RunnerDeleted(req, cause)
	}
	if keep {
		return r, nil, 0, nil
	}
	ended, prev, err := e.endRequestTx(ctx, tx, req.ID, end)
	return r, ended, prev, err
}

// Deletion is a runner deletion that a transaction started.
type Deletion struct {
	runner  *Runner
	ended   *Request
	prevReq tidev1.RequestState
}

// DeleteRunnerTx starts the deletion of a runner in the caller's
// transaction, as DeleteRunner, DeleteDevSession and the clean-up hooks
// require: the runner is DELETING with reason and its request ends as the
// adapter says for cause, in one transaction. It returns nil when the runner
// is already ended or being deleted. After the commit, call FinishDeletion.
func (e *Engine) DeleteRunnerTx(ctx context.Context, tx pgx.Tx, orgID, runnerID string, reason tidev1.RunnerEndReason, cause DeleteCause) (*Deletion, error) {
	r, ended, prev, err := e.deleteRunnerTx(ctx, tx, orgID, runnerID, reason, cause)
	if err != nil || r == nil {
		return nil, err
	}
	return &Deletion{runner: r, ended: ended, prevReq: prev}, nil
}

// FinishDeletion runs after the commit of DeleteRunnerTx: it releases a
// vendor claim, deletes the sandbox at once (the driver of the lease holder
// retries a failed deletion) and returns the runner as it is then. It
// accepts nil.
func (e *Engine) FinishDeletion(ctx context.Context, d *Deletion) *Runner {
	if d == nil {
		return nil
	}
	if d.ended != nil && d.prevReq != reqRunning {
		e.releaseClaim(ctx, d.ended)
	}
	e.kickRunner(d.runner.ID)
	e.Kick(d.runner.OrganizationID)
	e.deleteSandboxNow(ctx, d.runner)
	r, err := e.GetRunner(ctx, e.svc.DB.Pool, d.runner.OrganizationID, d.runner.ID)
	if err != nil {
		return d.runner
	}
	return r
}

// DeleteSandboxOf deletes the sandbox of a DELETING runner at once and ends
// the runner; the driver of the lease holder retries a failed deletion.
func (e *Engine) DeleteSandboxOf(ctx context.Context, orgID, runnerID string) {
	r, err := e.GetRunner(ctx, e.svc.DB.Pool, orgID, runnerID)
	if err != nil || r.State != runDeleting {
		return
	}
	e.deleteSandboxNow(ctx, r)
}

func (e *Engine) deleteSandboxNow(ctx context.Context, r *Runner) {
	if r.State != runDeleting {
		return
	}
	client, _, err := e.links.ClientForRemoval(ctx, r.OrganizationID)
	if err != nil {
		return
	}
	if e.removeSandbox(context.WithoutCancel(ctx), client, r) {
		e.finishDelete(ctx, r.ID)
	}
}
