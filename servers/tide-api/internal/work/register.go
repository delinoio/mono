// SPDX-License-Identifier: FSL-1.1-ALv2

package work

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/protos/delino/tide/v1/tidev1connect"
	"github.com/delinoio/mono/servers/tide-api/internal/api"
	"github.com/delinoio/mono/servers/tide-api/internal/app"
	"github.com/delinoio/mono/servers/tide-api/internal/jobs"
	"github.com/delinoio/mono/servers/tide-api/internal/lease"
	"github.com/delinoio/mono/servers/tide-api/internal/link"
	"github.com/delinoio/mono/servers/tide-api/internal/metrics"
	"github.com/delinoio/mono/servers/tide-api/internal/profile"
)

// Register mounts WorkService, registers the lease components, the daily
// retention of ended requests and runners (30 days) and the clean-up hooks
// of OwnSand link removal, runner profile deletion and organization
// deletion, and returns the engine for the adapters of later layers.
func Register(r *app.Registry, links *link.Links, profiles *profile.Profiles, m *metrics.Metrics, timing Timing) *Engine {
	e := New(r.Service, links, profiles, m, timing)
	r.Handle(tidev1connect.NewWorkServiceHandler(&Handler{e: e}, r.HandlerOptions()...))
	r.AddLeaseComponent(lease.ComponentFunc{ComponentName: "dispatcher", RunFunc: e.RunDispatcher})
	r.AddLeaseComponent(lease.ComponentFunc{ComponentName: "runner_reconciler", RunFunc: e.RunReconciler})
	r.AddLeaseComponent(lease.ComponentFunc{ComponentName: "work_gauges", RunFunc: e.RunGauges})
	r.AddDailyJob(jobs.SQLJob(r.Service.DB, "ended_requests", "DELETE FROM requests WHERE end_time < $1::timestamptz - interval '30 days'"))
	r.AddDailyJob(jobs.SQLJob(r.Service.DB, "ended_runners", "DELETE FROM runners WHERE end_time < $1::timestamptz - interval '30 days'"))

	links.OnRemoval(link.RemovalHook{
		Name: "work",
		Prepare: func(ctx context.Context, orgID string) error {
			return e.cleanUpRunners(ctx, orgID, func(*Runner) bool { return true })
		},
		InTx: func(ctx context.Context, tx pgx.Tx, orgID string) error {
			ended, err := e.endOpenTx(ctx, tx, orgID, "", RequestEnd{State: reqCanceled, Reason: tidev1.RequestEndReason_REQUEST_END_REASON_INTEGRATION_REMOVED})
			e.pendingReleases.Store("link/"+orgID, ended)
			return err
		},
		AfterCommit: func(ctx context.Context, orgID string) { e.releasePending(ctx, "link/"+orgID) },
	})
	profiles.OnDeletion(profile.DeletionHook{
		Name: "work",
		Prepare: func(ctx context.Context, orgID, profileID string) error {
			return e.cleanUpRunners(ctx, orgID, func(r *Runner) bool {
				return r.ProfileID == profileID && r.State != runBusy && (r.RequestID == "" || r.State == runWaiting)
			})
		},
		InTx: e.deleteProfileWorkTx,
	})
	unmark := func(ctx context.Context, orgID string) {
		if _, err := r.Service.DB.Pool.Exec(context.WithoutCancel(ctx), "UPDATE ownsand_links SET removing = false WHERE organization_id = $1", orgID); err != nil {
			r.Service.Logger.WarnContext(ctx, "ownsand_link_unmark_failed", slog.String("organization_id", orgID), slog.Any("error", err))
		}
	}
	r.OnOrganizationDeletion(api.OrganizationDeletionHook{
		Name: "work",
		Prepare: func(ctx context.Context, d api.OrganizationDeletion) error {
			// Stop dispatch first, then delete every runner. The organization
			// rows (and with them the requests, runners and the link) are
			// deleted by the core in the same transaction as InTx.
			if _, err := r.Service.DB.Pool.Exec(ctx, "UPDATE ownsand_links SET removing = true WHERE organization_id = $1", d.OrganizationID); err != nil {
				return err
			}
			if err := e.cleanUpRunners(ctx, d.OrganizationID, func(*Runner) bool { return true }); err != nil {
				// The core runs Abort only for the hooks before the one that
				// failed, so this hook clears its own mark: the organization
				// stays and its dispatch continues.
				unmark(ctx, d.OrganizationID)
				return err
			}
			return nil
		},
		Abort: func(ctx context.Context, d api.OrganizationDeletion) { unmark(ctx, d.OrganizationID) },
		InTx: func(ctx context.Context, tx pgx.Tx, d api.OrganizationDeletion) error {
			ended, err := e.endOpenTx(ctx, tx, d.OrganizationID, "", RequestEnd{State: reqCanceled, Reason: tidev1.RequestEndReason_REQUEST_END_REASON_CANCELED_BY_USER})
			e.pendingReleases.Store("org/"+d.OrganizationID, ended)
			return err
		},
		AfterCommit: func(ctx context.Context, d api.OrganizationDeletion) { e.releasePending(ctx, "org/"+d.OrganizationID) },
	})
	return e
}

func (e *Engine) deleteProfileWorkTx(ctx context.Context, tx pgx.Tx, orgID, profileID string) (func(context.Context), error) {
	// GitHub can bind a waiting runner while holding its request lock.
	// Lock queued requests before runners to use the same order.
	ended, err := e.endQueuedTx(ctx, tx, orgID, profileID, RequestEnd{State: reqCanceled, Reason: tidev1.RequestEndReason_REQUEST_END_REASON_PROFILE_DELETED})
	if err != nil {
		return nil, err
	}
	// The profile lock excludes new reservations. Mark the runners that
	// arrived after Prepare, and check their eligibility under their locks.
	rows, err := tx.Query(ctx, `SELECT runner_id::text FROM runners WHERE organization_id = $1 AND runner_profile_id = $2
		AND ((request_id IS NULL AND state IN ($3, $4, $5)) OR state = $6) ORDER BY runner_id FOR UPDATE`,
		orgID, profileID, int16(runCreating), int16(runPrep), int16(runWarm), int16(runWaiting))
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	var deletions []*Deletion
	for _, id := range ids {
		d, err := e.DeleteRunnerTx(ctx, tx, orgID, id, tidev1.RunnerEndReason_RUNNER_END_REASON_CLEAN_UP, DeleteCleanUp)
		if err != nil {
			return nil, err
		}
		deletions = append(deletions, d)
	}
	return func(ctx context.Context) {
		for _, r := range ended {
			e.releaseClaim(ctx, r)
		}
		for _, d := range deletions {
			e.FinishDeletion(ctx, d)
		}
	}, nil
}

// releasePending releases the vendor claims of the requests that a clean-up
// transaction ended (after its commit).
func (e *Engine) releasePending(ctx context.Context, key string) {
	v, ok := e.pendingReleases.LoadAndDelete(key)
	if !ok {
		return
	}
	for _, r := range v.([]*Request) {
		if r.State == reqCanceled {
			e.releaseClaim(ctx, r)
		}
	}
}

// cleanUpRunners deletes the open runners that match: each one is DELETING
// (CLEAN_UP) with its request ended, then its sandbox is deleted. It fails
// when a sandbox cannot be deleted, so the caller keeps the OwnSand key.
func (e *Engine) cleanUpRunners(ctx context.Context, orgID string, match func(*Runner) bool) error {
	runners, err := e.openRunners(ctx, orgID)
	if err != nil {
		return err
	}
	client, _, cerr := e.links.ClientForRemoval(ctx, orgID)
	var errs []error
	for _, r := range runners {
		if !match(r) {
			continue
		}
		r, err := e.deleteRunnerMatching(ctx, orgID, r.ID, tidev1.RunnerEndReason_RUNNER_END_REASON_CLEAN_UP, DeleteCleanUp, match)
		if err != nil {
			if isCode(err, connect.CodeNotFound) {
				// OwnSand rejected the creation and the dispatcher removed
				// the reservation after the list.
				continue
			}
			errs = append(errs, err)
			continue
		}
		if r == nil {
			continue
		}
		if err := e.removeRegistration(ctx, r); err != nil {
			errs = append(errs, fmt.Errorf("remove the registration of runner %s: %w", r.ID, err))
			continue
		}
		if cerr != nil {
			errs = append(errs, cerr)
			continue
		}
		id := r.SandboxID
		if id == "" {
			// The CreateSandbox response may be lost or still in flight.
			var ok bool
			if id, ok = e.resolveCreate(ctx, client, r); !ok {
				errs = append(errs, fmt.Errorf("resolve the sandbox creation of runner %s", r.ID))
				continue
			}
		}
		if id != "" {
			if _, err := client.DeleteSandbox(ctx, id); err != nil && !isCode(err, connect.CodeNotFound) && !isCode(err, connect.CodeUnauthenticated) {
				// A key that OwnSand no longer accepts cannot delete the
				// sandbox; its idle_ttl ends it.
				errs = append(errs, fmt.Errorf("delete sandbox of runner %s: %w", r.ID, err))
				continue
			}
		}
		e.finishDelete(ctx, r.ID)
	}
	return errors.Join(errs...)
}

// endQueuedTx ends the QUEUED requests of an organization (of one profile
// when profileID is set).
func (e *Engine) endQueuedTx(ctx context.Context, tx pgx.Tx, orgID, profileID string, end RequestEnd) ([]*Request, error) {
	return e.endWhereTx(ctx, tx, orgID, profileID, []int16{int16(reqQueued)}, end)
}

// endOpenTx ends every open request of an organization: QUEUED ones with
// end, and STARTING or RUNNING ones whose runner the clean-up deleted with
// the adapter's deletion outcome. It also ends the runner rows that remain.
func (e *Engine) endOpenTx(ctx context.Context, tx pgx.Tx, orgID, profileID string, end RequestEnd) ([]*Request, error) {
	out, err := e.endWhereTx(ctx, tx, orgID, profileID, []int16{int16(reqQueued)}, end)
	if err != nil {
		return nil, err
	}
	rest, err := e.endWhereTx(ctx, tx, orgID, profileID, []int16{int16(reqStarting), int16(reqRunning)},
		RequestEnd{State: reqFailed, Reason: tidev1.RequestEndReason_REQUEST_END_REASON_INTEGRATION_REMOVED})
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, "UPDATE runners SET state = $2, end_reason = $3, end_time = $4, update_time = $4 WHERE organization_id = $1 AND state = ANY($5)",
		orgID, int16(runDeleted), int16(tidev1.RunnerEndReason_RUNNER_END_REASON_CLEAN_UP), e.svc.NowUTC(), openRunnerStates); err != nil {
		return nil, err
	}
	return append(out, rest...), nil
}

func (e *Engine) endWhereTx(ctx context.Context, tx pgx.Tx, orgID, profileID string, states []int16, end RequestEnd) ([]*Request, error) {
	rows, err := tx.Query(ctx, "SELECT request_id::text FROM requests WHERE organization_id = $1 AND ($2 = '' OR runner_profile_id::text = $2) AND state = ANY($3) ORDER BY request_id FOR UPDATE",
		orgID, profileID, states)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	var out []*Request
	for _, id := range ids {
		r, _, err := e.endRequestTx(ctx, tx, id, end)
		if err != nil {
			return nil, err
		}
		if r != nil {
			out = append(out, r)
		}
	}
	return out, nil
}

// RunGauges is the lease component that records tide_requests and
// tide_runners.
func (e *Engine) RunGauges(ctx context.Context) error {
	seenReq, seenRun := map[[3]string]bool{}, map[[3]string]bool{}
	t := time.NewTicker(e.timing.GaugeInterval)
	defer t.Stop()
	for {
		e.recordGauges(ctx, seenReq, seenRun)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

func (e *Engine) recordGauges(ctx context.Context, seenReq, seenRun map[[3]string]bool) {
	now := map[[3]string]int64{}
	rows, err := e.svc.DB.Pool.Query(ctx, "SELECT kind, priority, state, count(*) FROM requests WHERE state = ANY($1) GROUP BY kind, priority, state", openRequestStates)
	if err != nil {
		return
	}
	for rows.Next() {
		var kind, prio, state int16
		var n int64
		if rows.Scan(&kind, &prio, &state, &n) == nil {
			now[[3]string{kindLabel(tidev1.WorkloadKind(kind)), priorityLabel(tidev1.Priority(prio)), reqStateLabel(tidev1.RequestState(state))}] = n
		}
	}
	rows.Close()
	for k := range seenReq {
		if _, ok := now[k]; !ok {
			now[k] = 0
		}
	}
	for k, n := range now {
		seenReq[k] = true
		e.m.Requests(k[0], k[1], k[2], n)
	}
	now = map[[3]string]int64{}
	rows, err = e.svc.DB.Pool.Query(ctx, "SELECT kind, guest_os, state, count(*) FROM runners WHERE state = ANY($1) GROUP BY kind, guest_os, state", openRunnerStates)
	if err != nil {
		return
	}
	for rows.Next() {
		var kind, guest, state int16
		var n int64
		if rows.Scan(&kind, &guest, &state, &n) == nil {
			now[[3]string{kindLabel(tidev1.WorkloadKind(kind)), guestLabel(tidev1.GuestOs(guest)), runStateLabel(tidev1.RunnerState(state))}] = n
		}
	}
	rows.Close()
	for k := range seenRun {
		if _, ok := now[k]; !ok {
			now[k] = 0
		}
	}
	for k, n := range now {
		seenRun[k] = true
		e.m.Runners(k[0], k[1], k[2], n)
	}
}
