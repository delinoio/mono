// SPDX-License-Identifier: FSL-1.1-ALv2

// Package agent is the AGENT_SESSION workload of tide-api: the work.Adapter
// of the kind, which dispatches to one Vendor per agent vendor, and the
// building blocks that the vendor layers share. The owning contract is the
// Agent integrations, Cursor and Devin, Runner lifecycle and Runner
// installation sections of docs/servers-tide-api-foundation.md.
//
// Building blocks (a later Claude Code or OpenAI Codex layer reuses them):
//
//   - The Hub registers the AGENT_SESSION adapter and asks the Vendor of a
//     request (by work.Source) or of a profile (by the vendor of its agent
//     integration) to prepare the guest, to start the runner program and to
//     release a claim.
//   - Watchers (watch.go): on the lease holder, one watch goroutine per agent
//     integration of a registered vendor whose organization has an ACTIVE
//     OwnSand link and a runner profile that names the integration. The Hub
//     starts, restarts (when the settings or credentials change) and stops
//     them, and reconnects an ended watch with exponential backoff (1 second
//     to 1 minute), counting tide_vendor_watch_reconnects_total{vendor}.
//   - Claims (claims.go): Watch.Admit claims a vendor request and creates
//     the tide request with its claim row in one transaction, so a repeated
//     event never creates a second request. Claims of ended requests are
//     released by Adapter.ReleaseClaim at once and by the claim poller until
//     the vendor confirms; the poller also reports runners as BUSY when the
//     vendor says that the runner program took its session.
//   - Guest helpers (guest.go): a runner directory that only the guest user
//     reads, the credential file, install scripts and the runner program
//     start with its output in a guest log file.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/delinoio/mono/go/ownsand"
	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/servers/tide-api/internal/api"
	"github.com/delinoio/mono/servers/tide-api/internal/app"
	"github.com/delinoio/mono/servers/tide-api/internal/integration"
	"github.com/delinoio/mono/servers/tide-api/internal/lease"
	"github.com/delinoio/mono/servers/tide-api/internal/work"
)

// Vendor is one agent vendor whose demand tide claims from a vendor API
// (Cursor, Devin). Each method gets the integration without credentials;
// a method that calls the vendor decrypts the credential it needs with
// Hub.Credential right before the call.
type Vendor interface {
	// Vendor is the agent vendor; Source is the work.Source of its requests.
	Vendor() tidev1.AgentVendor
	Source() work.Source
	// Watch runs one watch session of an integration: it lists the pending
	// vendor requests, admits each one with w.Admit, then follows the
	// vendor's stream until ctx ends or the stream ends. It returns nil for
	// a scheduled end (the Hub lists again at once and counts no reconnect)
	// and an error for a failure or an unexpected end (the Hub waits with
	// backoff and counts a reconnect).
	Watch(ctx context.Context, w *Watch) error
	// CheckStarted returns the request IDs of claims whose runner program
	// took its session, among claims of one integration whose request is
	// STARTING with a runner that waits for work.
	CheckStarted(ctx context.Context, in integration.Integration, claims []*Claim) ([]string, error)
	// Release gives up a held claim of a request that ended, or that the
	// Hub gives up (organization deletion). done=false keeps the claim
	// held, and the poller calls Release again later.
	Release(ctx context.Context, in integration.Integration, c *Claim, req *work.Request) (done bool, err error)
	// Prepare checks for the runner program in the guest and installs it
	// when it is missing (lifecycle step 2).
	Prepare(ctx context.Context, g *work.Guest, in integration.Integration) error
	// Start writes the runner credential to a guest file and starts the
	// runner program for the claim's session (lifecycle step 3).
	Start(ctx context.Context, g *work.Guest, in integration.Integration, c *Claim) (work.StartResult, error)
}

// Timing holds the intervals of the Hub.
type Timing struct {
	// SyncInterval is the interval of the watcher set check (integration,
	// link and profile changes on any instance).
	SyncInterval time.Duration
	// PollInterval is the interval of the claim poller.
	PollInterval time.Duration
	// BackoffMin and BackoffMax bound the reconnect backoff of a watch.
	BackoffMin, BackoffMax time.Duration
}

// DefaultTiming returns the production intervals.
func DefaultTiming() Timing {
	return Timing{SyncInterval: 5 * time.Second, PollInterval: 10 * time.Second, BackoffMin: time.Second, BackoffMax: time.Minute}
}

// Hub is the AGENT_SESSION workload: the adapter, the vendors, the
// watchers and the claims.
type Hub struct {
	svc          *api.Service
	engine       *work.Engine
	integrations *integration.Store
	timing       Timing
	log          *slog.Logger

	vendors  map[tidev1.AgentVendor]Vendor
	bySource map[work.Source]Vendor

	mu       sync.Mutex
	watchers map[string]*watcher
	// stopped holds the organizations whose deletion runs: no watcher runs
	// for them until the deletion ends.
	stopped map[string]bool
	syncNow chan struct{}
}

// Register registers the AGENT_SESSION adapter with the engine, the
// watcher and claim poller lease components and the organization deletion
// hook. Vendor layers add themselves with AddVendor before the app runs.
func Register(r *app.Registry, engine *work.Engine, integrations *integration.Store, timing Timing) *Hub {
	if timing == (Timing{}) {
		timing = DefaultTiming()
	}
	h := &Hub{svc: r.Service, engine: engine, integrations: integrations, timing: timing, log: r.Service.Logger,
		vendors: map[tidev1.AgentVendor]Vendor{}, bySource: map[work.Source]Vendor{},
		watchers: map[string]*watcher{}, stopped: map[string]bool{}, syncNow: make(chan struct{}, 1)}
	engine.RegisterAdapter(&Adapter{h: h})
	r.AddLeaseComponent(lease.ComponentFunc{ComponentName: "agent_watchers", RunFunc: h.runWatchers})
	r.AddLeaseComponent(lease.ComponentFunc{ComponentName: "agent_claims", RunFunc: h.runPoller})
	r.OnOrganizationDeletion(api.OrganizationDeletionHook{
		Name:    "agent",
		Prepare: h.prepareOrgDeletion,
		Abort:   func(ctx context.Context, d api.OrganizationDeletion) { h.resumeOrg(d.OrganizationID) },
		AfterCommit: func(ctx context.Context, d api.OrganizationDeletion) {
			h.resumeOrg(d.OrganizationID)
		},
	})
	return h
}

// AddVendor registers a vendor. Call it before the app runs.
func (h *Hub) AddVendor(v Vendor) {
	h.vendors[v.Vendor()] = v
	h.bySource[v.Source()] = v
}

// Service returns the service.
func (h *Hub) Service() *api.Service { return h.svc }

// Engine returns the work engine.
func (h *Hub) Engine() *work.Engine { return h.engine }

// Timing returns the intervals of the Hub.
func (h *Hub) Timing() Timing { return h.timing }

// Credential decrypts one credential of an integration for the call that
// uses it. Callers never log it.
func (h *Hub) Credential(ctx context.Context, in integration.Integration, cred tidev1.AgentCredential) (string, error) {
	return h.integrations.Credential(ctx, in.OrganizationID, in.ID, cred)
}

// VendorLabel returns the stable label of a vendor for metrics and logs,
// for example CURSOR.
func VendorLabel(v tidev1.AgentVendor) string {
	s := v.String()
	if len(s) > len("AGENT_VENDOR_") {
		return s[len("AGENT_VENDOR_"):]
	}
	return s
}

// vendorOfProfile returns the vendor and integration of a runner's profile.
func (h *Hub) vendorOfProfile(ctx context.Context, g *work.Guest) (Vendor, integration.Integration, error) {
	if g.Profile == nil {
		return nil, integration.Integration{}, errors.New("the runner profile was deleted")
	}
	in, err := h.integrations.Get(ctx, h.svc.DB.Pool, g.Profile.OrganizationID, g.Profile.AgentIntegrationID)
	if err != nil {
		return nil, integration.Integration{}, fmt.Errorf("load agent integration: %w", err)
	}
	v := h.vendors[in.Vendor]
	if v == nil {
		return nil, in, fmt.Errorf("no adapter serves %s runners", VendorLabel(in.Vendor))
	}
	return v, in, nil
}

// Adapter is the work.Adapter of AGENT_SESSION. The dispatcher binds one
// runner to one request; Prepare installs the vendor's runner program, and
// Start starts it for the request's claimed session.
type Adapter struct {
	work.BaseAdapter
	h *Hub
}

var _ work.Adapter = (*Adapter)(nil)

func (*Adapter) Kind() tidev1.WorkloadKind { return tidev1.WorkloadKind_WORKLOAD_KIND_AGENT_SESSION }

func (*Adapter) BindsAtDispatch() bool { return true }

// Prepare installs the runner program of the profile's vendor. A warm
// runner has no request, so the vendor comes from the profile.
func (a *Adapter) Prepare(ctx context.Context, g *work.Guest) error {
	v, in, err := a.h.vendorOfProfile(ctx, g)
	if err != nil {
		return err
	}
	return v.Prepare(ctx, g, in)
}

// Start starts the runner program for the request's held claim.
func (a *Adapter) Start(ctx context.Context, g *work.Guest) (work.StartResult, error) {
	if g.Request == nil {
		return work.StartResult{}, errors.New("the runner has no request")
	}
	c, err := a.h.claim(ctx, a.h.svc.DB.Pool, g.Request.ID)
	if err != nil {
		return work.StartResult{}, err
	}
	if c.State != ClaimHeld {
		return work.StartResult{}, errors.New("tide no longer holds the vendor claim of the request")
	}
	v := a.h.vendors[c.Vendor]
	if v == nil {
		return work.StartResult{}, fmt.Errorf("no adapter serves %s runners", VendorLabel(c.Vendor))
	}
	in, err := a.h.integrations.Get(ctx, a.h.svc.DB.Pool, c.OrganizationID, c.IntegrationID)
	if err != nil {
		return work.StartResult{}, fmt.Errorf("load agent integration: %w", err)
	}
	return v.Start(ctx, g, in, c)
}

// ProgramExited (recorded decision): the runner program serves one session
// and exits. Exit code 0 (the session ended and the worker was released,
// for example at Cursor's idle release timeout) ends the request with ENDED
// and COMPLETED. Any other result (a non-zero exit code, a signal, a
// timeout, an exec failure) ends it with FAILED and RUNNER_LOST, like a
// lost sandbox; the vendor decides whether the session runs again. The
// claim poller then releases the claim when the vendor still holds it.
func (*Adapter) ProgramExited(_ *work.Request, res *ownsand.ExecResult) (work.RequestEnd, bool) {
	if res != nil && res.Kind == ownsand.ExecResultExited && res.ExitCode != nil && *res.ExitCode == 0 && !res.TimedOut {
		return work.RequestEnd{State: tidev1.RequestState_REQUEST_STATE_ENDED, Reason: tidev1.RequestEndReason_REQUEST_END_REASON_COMPLETED}, false
	}
	return work.RequestEnd{State: tidev1.RequestState_REQUEST_STATE_FAILED, Reason: tidev1.RequestEndReason_REQUEST_END_REASON_RUNNER_LOST}, false
}

// ReleaseClaim releases the vendor claim of a request that tide ended
// before a runner took its session.
func (a *Adapter) ReleaseClaim(ctx context.Context, req *work.Request) error {
	return a.h.releaseRequest(ctx, req)
}

// releaseRequest releases the held claim of an ended request (no claim: no
// call). A user cancellation before a runner took the session also marks
// the claim declined, so the watcher does not claim the same vendor entry
// again.
func (h *Hub) releaseRequest(ctx context.Context, req *work.Request) error {
	c, err := h.claim(ctx, h.svc.DB.Pool, req.ID)
	if errors.Is(err, errNoClaim) {
		return nil
	}
	if err != nil {
		return err
	}
	if req.StartTime == nil && req.EndReason == tidev1.RequestEndReason_REQUEST_END_REASON_CANCELED_BY_USER && !c.Declined {
		if _, err := h.svc.DB.Pool.Exec(ctx, "UPDATE agent_claims SET declined = true, update_time = $2 WHERE request_id = $1", c.RequestID, h.svc.NowUTC()); err != nil {
			return err
		}
	}
	if c.State != ClaimHeld {
		return nil
	}
	return h.releaseClaim(ctx, c, req)
}

// releaseClaim asks the vendor to release c and marks it released when the
// vendor confirms.
func (h *Hub) releaseClaim(ctx context.Context, c *Claim, req *work.Request) error {
	v := h.vendors[c.Vendor]
	if v == nil {
		return nil
	}
	in, err := h.integrations.Get(ctx, h.svc.DB.Pool, c.OrganizationID, c.IntegrationID)
	if err != nil {
		return fmt.Errorf("load agent integration: %w", err)
	}
	done, err := v.Release(ctx, in, c, req)
	if done {
		if err := h.setClaimState(ctx, h.svc.DB.Pool, c.RequestID, ClaimHeld, ClaimReleased); err != nil {
			return err
		}
		h.log.InfoContext(ctx, "agent_claim_released", slog.String("organization_id", c.OrganizationID), slog.String("request_id", c.RequestID),
			slog.String("vendor", VendorLabel(c.Vendor)), slog.String("vendor_request_id", c.VendorRequestID))
	}
	return err
}

// prepareOrgDeletion stops the watchers of the organization and releases
// the claims of its queued requests while the credentials still exist. The
// work layer's hook (registered earlier) already deleted the runners, which
// released the claims of their requests.
func (h *Hub) prepareOrgDeletion(ctx context.Context, d api.OrganizationDeletion) error {
	h.mu.Lock()
	h.stopped[d.OrganizationID] = true
	var stop []*watcher
	for id, w := range h.watchers {
		if w.in.OrganizationID == d.OrganizationID {
			stop = append(stop, w)
			delete(h.watchers, id)
		}
	}
	h.mu.Unlock()
	for _, w := range stop {
		w.stop()
	}
	claims, err := h.heldClaims(ctx, "c.organization_id = $1 AND r.state = ANY($2)", d.OrganizationID, work.OpenRequestStates())
	if err != nil {
		return err
	}
	for _, hc := range claims {
		if err := h.releaseClaim(ctx, hc.claim, hc.req); err != nil {
			h.log.WarnContext(ctx, "agent_claim_release_failed", slog.String("organization_id", d.OrganizationID), slog.String("request_id", hc.claim.RequestID), slog.Any("error", err))
		}
	}
	return nil
}

func (h *Hub) resumeOrg(orgID string) {
	h.mu.Lock()
	delete(h.stopped, orgID)
	h.mu.Unlock()
	h.kickSync()
}

func (h *Hub) kickSync() {
	select {
	case h.syncNow <- struct{}{}:
	default:
	}
}

// inTx runs fn in a transaction.
func (h *Hub) inTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	return h.svc.DB.InTx(ctx, fn)
}
