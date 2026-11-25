// SPDX-License-Identifier: FSL-1.1-ALv2

package agent

import (
	"context"
	"log/slog"
	"time"

	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/servers/tide-api/internal/integration"
	"github.com/delinoio/mono/servers/tide-api/internal/work"
)

// Watch is one integration's watch, as a Vendor sees it.
type Watch struct {
	h  *Hub
	v  Vendor
	In integration.Integration
	// Logger has the organization, integration and vendor attributes.
	Logger *slog.Logger
}

// Credential decrypts one credential of the integration for the call that
// uses it.
func (w *Watch) Credential(ctx context.Context, cred tidev1.AgentCredential) (string, error) {
	return w.h.Credential(ctx, w.In, cred)
}

// Admit claims a pending vendor request and creates its tide request; a
// repeated event of the same entry creates nothing. Errors are logged by
// the caller; the next list retries the entry.
func (w *Watch) Admit(ctx context.Context, d Demand) error { return w.h.admit(ctx, w.v, w.In, d) }

// Lost reports that another worker holds a vendor request or that it left
// the vendor queue: a QUEUED tide request of it ends with CANCELED and
// CANCELED_BY_SOURCE.
func (w *Watch) Lost(ctx context.Context, vendorRequestID string) error {
	return w.h.lost(ctx, w.In.OrganizationID, w.In.Vendor, vendorRequestID)
}

// Claim returns the newest claim of a vendor request and its request, or
// nil.
func (w *Watch) Claim(ctx context.Context, vendorRequestID string) (*Claim, *work.Request, error) {
	c, req, err := w.h.latestClaim(ctx, w.h.svc.DB.Pool, w.In.OrganizationID, w.In.Vendor, vendorRequestID)
	return c, req, err
}

// Taken reports that the runner program with workerID took the session of
// a vendor request: the runner becomes BUSY and the request RUNNING.
func (w *Watch) Taken(ctx context.Context, vendorRequestID, workerID string) error {
	c, req, err := w.h.latestClaim(ctx, w.h.svc.DB.Pool, w.In.OrganizationID, w.In.Vendor, vendorRequestID)
	if err != nil || c == nil || c.WorkerID != workerID || req.State != tidev1.RequestState_REQUEST_STATE_STARTING {
		return err
	}
	return w.h.markBusy(ctx, req)
}

// markBusy marks the runner of a STARTING request BUSY when it waits for
// work.
func (h *Hub) markBusy(ctx context.Context, req *work.Request) error {
	if req.RunnerID == "" {
		return nil
	}
	r, err := h.engine.GetRunner(ctx, h.svc.DB.Pool, req.OrganizationID, req.RunnerID)
	if err != nil || r.State != tidev1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK {
		return err
	}
	if err := h.engine.MarkBusy(ctx, req.OrganizationID, r.ID, ""); err != nil {
		return err
	}
	h.log.InfoContext(ctx, "agent_session_taken", slog.String("organization_id", req.OrganizationID), slog.String("request_id", req.ID), slog.String("runner_id", r.ID))
	return nil
}

// watcher is one running watch goroutine.
type watcher struct {
	in      integration.Integration
	version string
	cancel  context.CancelFunc
	done    chan struct{}
}

// drainWatcher moves an active watcher to the draining set under h.mu.
// The caller cancels and joins it after releasing h.mu.
func (h *Hub) drainWatcher(w *watcher) {
	delete(h.watchers, w.in.ID)
	select {
	case <-w.done:
		return
	default:
	}
	orgID := w.in.OrganizationID
	if h.draining[orgID] == nil {
		h.draining[orgID] = map[*watcher]struct{}{}
	}
	h.draining[orgID][w] = struct{}{}
}

func (h *Hub) finishWatcher(w *watcher) {
	h.mu.Lock()
	defer h.mu.Unlock()
	close(w.done)
	orgID := w.in.OrganizationID
	delete(h.draining[orgID], w)
	if len(h.draining[orgID]) == 0 {
		delete(h.draining, orgID)
	}
}

func stopWatchers(watchers []*watcher) {
	for _, w := range watchers {
		w.cancel()
	}
	for _, w := range watchers {
		<-w.done
	}
}

// desired is one integration that needs a watcher.
type desired struct {
	in      integration.Integration
	version string
}

// desiredWatchers returns the integrations of registered vendors whose
// organization has an ACTIVE OwnSand link that is not being removed and a
// runner profile that names the integration. The version changes when the
// settings or a credential change, so the watcher restarts with them.
func (h *Hub) desiredWatchers(ctx context.Context) (map[string]desired, error) {
	vendors := make([]int16, 0, len(h.vendors))
	for v, impl := range h.vendors {
		if _, push := impl.(Pusher); !push {
			vendors = append(vendors, int16(v))
		}
	}
	rows, err := h.svc.DB.Pool.Query(ctx, `SELECT i.agent_integration_id::text, i.organization_id::text, i.vendor, i.name, i.pool_name, i.outpost_name,
		i.create_time, i.update_time, COALESCE((SELECT max(c.set_time) FROM agent_integration_credentials c WHERE c.agent_integration_id = i.agent_integration_id), i.create_time)
		FROM agent_integrations i JOIN ownsand_links l ON l.organization_id = i.organization_id
		WHERE l.state = 1 AND NOT l.removing AND i.vendor = ANY($1)
		AND EXISTS (SELECT 1 FROM runner_profiles p WHERE p.organization_id = i.organization_id AND p.agent_integration_id = i.agent_integration_id)`, vendors)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]desired{}
	for rows.Next() {
		var in integration.Integration
		var vendor int16
		var credTime time.Time
		if err := rows.Scan(&in.ID, &in.OrganizationID, &vendor, &in.Name, &in.PoolName, &in.OutpostName, &in.CreateTime, &in.UpdateTime, &credTime); err != nil {
			return nil, err
		}
		in.Vendor = tidev1.AgentVendor(vendor)
		out[in.ID] = desired{in: in, version: in.UpdateTime.UTC().Format(time.RFC3339Nano) + "/" + credTime.UTC().Format(time.RFC3339Nano)}
	}
	return out, rows.Err()
}

// runWatchers is the agent_watchers lease component: it keeps one watcher
// per desired integration and stops them all when ctx ends.
func (h *Hub) runWatchers(ctx context.Context) error {
	defer func() {
		h.mu.Lock()
		for _, w := range h.watchers {
			h.drainWatcher(w)
		}
		var stop []*watcher
		for _, watchers := range h.draining {
			for w := range watchers {
				stop = append(stop, w)
			}
		}
		h.mu.Unlock()
		stopWatchers(stop)
	}()
	t := time.NewTicker(h.timing.SyncInterval)
	defer t.Stop()
	for {
		h.syncWatchers(ctx)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		case <-h.syncNow:
		}
	}
}

func (h *Hub) syncWatchers(ctx context.Context) {
	want, err := h.desiredWatchers(ctx)
	if err != nil {
		if ctx.Err() == nil {
			h.log.WarnContext(ctx, "agent_watchers_sync_failed", slog.Any("error", err))
		}
		return
	}
	h.mu.Lock()
	var stop []*watcher
	for id, w := range h.watchers {
		d, ok := want[id]
		if !ok || d.version != w.version || h.stopped[w.in.OrganizationID] > 0 {
			stop = append(stop, w)
			h.drainWatcher(w)
		}
	}
	var start []desired
	for id, d := range want {
		if _, ok := h.watchers[id]; !ok && h.stopped[d.in.OrganizationID] == 0 {
			start = append(start, d)
		}
	}
	h.mu.Unlock()
	stopWatchers(stop)
	for _, w := range stop {
		h.log.InfoContext(ctx, "vendor_watch_stopped", slog.String("organization_id", w.in.OrganizationID), slog.String("agent_integration_id", w.in.ID),
			slog.String("vendor", VendorLabel(w.in.Vendor)))
	}
	for _, d := range start {
		v := h.vendors[d.in.Vendor]
		wctx, cancel := context.WithCancel(ctx)
		w := &watcher{in: d.in, version: d.version, cancel: cancel, done: make(chan struct{})}
		h.mu.Lock()
		if h.stopped[d.in.OrganizationID] > 0 {
			h.mu.Unlock()
			cancel()
			continue
		}
		h.watchers[d.in.ID] = w
		h.mu.Unlock()
		go func() {
			defer h.finishWatcher(w)
			h.watch(wctx, v, d.in)
		}()
	}
}

// watch runs the watch sessions of one integration until ctx ends. After a
// session that failed or ended unexpectedly, it waits with exponential
// backoff (BackoffMin to BackoffMax) and counts a reconnect; after a
// scheduled end, it lists again at once. A session that ran longer than
// BackoffMax resets the backoff.
func (h *Hub) watch(ctx context.Context, v Vendor, in integration.Integration) {
	log := h.log.With(slog.String("organization_id", in.OrganizationID), slog.String("agent_integration_id", in.ID), slog.String("vendor", VendorLabel(in.Vendor)))
	log.InfoContext(ctx, "vendor_watch_started")
	w := &Watch{h: h, v: v, In: in, Logger: log}
	backoff := h.timing.BackoffMin
	for ctx.Err() == nil {
		began := time.Now()
		err := v.Watch(ctx, w)
		if ctx.Err() != nil {
			return
		}
		if time.Since(began) > h.timing.BackoffMax {
			backoff = h.timing.BackoffMin
		}
		if err == nil {
			log.InfoContext(ctx, "vendor_watch_reconnect", slog.String("cause", "scheduled"))
			backoff = h.timing.BackoffMin
			continue
		}
		if rs, ok := v.(Restarter); ok {
			rs.Restarted(ctx, log, backoff, err)
		} else {
			h.engine.Metrics().VendorWatchReconnect(VendorLabel(in.Vendor))
			log.WarnContext(ctx, "vendor_watch_reconnect", slog.String("cause", "error"), slog.Duration("backoff", backoff), slog.Any("error", err))
		}
		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		backoff = min(2*backoff, h.timing.BackoffMax)
	}
}

// runPoller is the agent_claims lease component: every PollInterval it
// releases the held claims of ended requests and asks the vendors whether
// the runner programs of STARTING requests took their sessions.
func (h *Hub) runPoller(ctx context.Context) error {
	t := time.NewTicker(h.timing.PollInterval)
	defer t.Stop()
	for {
		h.poll(ctx)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// Poll runs one round of the claim poller, for tests.
func (h *Hub) Poll(ctx context.Context) { h.poll(ctx) }

func (h *Hub) poll(ctx context.Context) {
	ended, err := h.heldClaims(ctx, bestEffortClaimLookup, "NOT (r.state = ANY($1))", work.OpenRequestStates())
	if err != nil {
		if ctx.Err() == nil {
			h.log.WarnContext(ctx, "agent_claims_poll_failed", slog.Any("error", err))
		}
		return
	}
	for _, hc := range ended {
		if _, err := h.releaseClaim(ctx, hc.claim, hc.req); err != nil && ctx.Err() == nil {
			h.log.WarnContext(ctx, "agent_claim_release_failed", slog.String("organization_id", hc.claim.OrganizationID), slog.String("request_id", hc.claim.RequestID),
				slog.Any("error", err))
		}
	}
	starting, err := h.heldClaims(ctx, bestEffortClaimLookup, `r.state = $1 AND EXISTS (SELECT 1 FROM runners n WHERE n.request_id = r.request_id AND n.state = $2)`,
		int16(tidev1.RequestState_REQUEST_STATE_STARTING), int16(tidev1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK))
	if err != nil {
		return
	}
	byIntegration := map[string][]heldClaim{}
	for _, hc := range starting {
		byIntegration[hc.claim.IntegrationID] = append(byIntegration[hc.claim.IntegrationID], hc)
	}
	for id, list := range byIntegration {
		c0 := list[0].claim
		in, err := h.integrations.Get(ctx, h.svc.DB.Pool, c0.OrganizationID, id)
		if err != nil {
			continue
		}
		v := h.vendors[in.Vendor]
		claims := make([]*Claim, len(list))
		reqs := map[string]*work.Request{}
		for i, hc := range list {
			claims[i] = hc.claim
			reqs[hc.claim.RequestID] = hc.req
		}
		// A failed check of some claims does not discard the claims that the
		// vendor confirmed: CheckStarted returns both.
		taken, err := v.CheckStarted(ctx, in, claims)
		if err != nil && ctx.Err() == nil {
			h.log.WarnContext(ctx, "agent_session_check_failed", slog.String("organization_id", in.OrganizationID), slog.String("agent_integration_id", in.ID), slog.Any("error", err))
		}
		for _, reqID := range taken {
			if req := reqs[reqID]; req != nil {
				if err := h.markBusy(ctx, req); err != nil {
					h.log.WarnContext(ctx, "runner_update_failed", slog.String("request_id", reqID), slog.Any("error", err))
				}
			}
		}
	}
}
