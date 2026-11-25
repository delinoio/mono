// SPDX-License-Identifier: FSL-1.1-ALv2

// Package agent is the AGENT_SESSION workload of tide-api: the work.Adapter
// of the kind, which dispatches to one Vendor per agent vendor, and the
// building blocks that the vendor layers share. The owning contract is the
// Agent integrations, Cursor and Devin, Runner lifecycle and Runner
// installation sections of docs/servers-tide-api-foundation.md.
//
// Building blocks (the vendor layers share them):
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
//   - Pushed demand (direct.go): a vendor that pushes its demand to tide
//     (the Claude Code spawn-runner hook, OpenAI webhooks) creates the
//     request and its claim row with CreateDirectTx in its own
//     transaction, without a claim at the vendor; optional interfaces let
//     a vendor skip the watcher (Pusher), count its own restarts
//     (Restarter) and map a sandbox end (SandboxEnder).
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
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
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
	// STARTING with a runner that waits for work. When the vendor cannot
	// answer for some claims, it returns the request IDs that it confirmed
	// together with an error; the Hub applies the confirmed ones.
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

	// claimLocks serializes the claim and release calls of one vendor
	// request; see lockVendorRequest.
	claimLocksMu sync.Mutex
	claimLocks   map[string]*claimLock

	mu       sync.Mutex
	watchers map[string]*watcher
	// draining keeps watchers visible to organization deletion until the
	// vendor watch returns. It is keyed by organization and watcher identity.
	draining map[string]map[*watcher]struct{}
	// stopped counts deletion attempts that are holding an organization's
	// watchers stopped.
	stopped map[string]int
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
		claimLocks: map[string]*claimLock{}, watchers: map[string]*watcher{}, draining: map[string]map[*watcher]struct{}{},
		stopped: map[string]int{}, syncNow: make(chan struct{}, 1)}
	engine.RegisterAdapter(&Adapter{h: h})
	integrations.OnDeletion(h.checkIntegrationDeletion)
	r.AddLeaseComponent(lease.ComponentFunc{ComponentName: "agent_watchers", RunFunc: h.runWatchers})
	r.AddLeaseComponent(lease.ComponentFunc{ComponentName: "agent_claims", RunFunc: h.runPoller})
	r.OnOrganizationDeletion(api.OrganizationDeletionHook{
		Name: "agent",
		Prepare: func(ctx context.Context, d api.OrganizationDeletion) (api.OrganizationDeletionSteps, error) {
			if err := h.prepareOrgDeletion(ctx, d); err != nil {
				return api.OrganizationDeletionSteps{}, err
			}
			resume := func(context.Context) { h.resumeOrg(d.OrganizationID) }
			return api.OrganizationDeletionSteps{Abort: resume, AfterCommit: resume}, nil
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

// SandboxEnded asks the request's vendor (SandboxEnder) and otherwise
// keeps the default: the runner FAILED and the request FAILED with
// RUNNER_LOST.
func (a *Adapter) SandboxEnded(req *work.Request, reason ownsandv1.SandboxEndReason) work.SandboxOutcome {
	if req != nil {
		if se, ok := a.h.bySource[req.Source].(SandboxEnder); ok {
			if out, ok := se.SandboxEnded(req, reason); ok {
				return out
			}
		}
	}
	return work.BaseAdapter{}.SandboxEnded(req, reason)
}

// ReleaseClaim releases the vendor claim of a request that tide ended
// before a runner took its session.
func (a *Adapter) ReleaseClaim(ctx context.Context, req *work.Request) error {
	return a.h.releaseRequest(ctx, req)
}

// releaseRequest releases the held claim of an ended request (no claim: no
// call).
func (h *Hub) releaseRequest(ctx context.Context, req *work.Request) error {
	c, err := h.claim(ctx, h.svc.DB.Pool, req.ID)
	if errors.Is(err, errNoClaim) {
		return nil
	}
	if err != nil {
		return err
	}
	if c.State != ClaimHeld {
		return nil
	}
	_, err = h.releaseClaim(ctx, c, req)
	return err
}

// claimLock is one entry of Hub.claimLocks.
type claimLock struct {
	mu    sync.Mutex
	users int
}

// lockVendorRequest serializes claim, release and loss handling for one
// vendor request and returns the unlock function. The vendor APIs identify a claim only by
// the vendor request ID (Cursor `claims/{id}/release`, Devin
// `devins/{session_id}/release`), so a release of an earlier claim that
// runs after the claim of a follow-up would end the follow-up's claim.
// admit releases the earlier claim first, and the lock keeps the claim
// poller from releasing it at the same time. The lock is per process: the
// watchers and the claim poller run on the lease holder. Other instances
// release only claims of requests that they ended before a runner took the
// session, and the vendor queues the follow-up of such a request only after
// that release took effect.
func (h *Hub) lockVendorRequest(orgID string, vendor tidev1.AgentVendor, vendorRequestID string) func() {
	key := orgID + "/" + VendorLabel(vendor) + "/" + vendorRequestID
	h.claimLocksMu.Lock()
	l := h.claimLocks[key]
	if l == nil {
		l = &claimLock{}
		h.claimLocks[key] = l
	}
	l.users++
	h.claimLocksMu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		h.claimLocksMu.Lock()
		if l.users--; l.users == 0 {
			delete(h.claimLocks, key)
		}
		h.claimLocksMu.Unlock()
	}
}

// releaseClaim asks the vendor to release c and marks it released when the
// vendor confirms. It returns whether the claim is released.
func (h *Hub) releaseClaim(ctx context.Context, c *Claim, req *work.Request) (bool, error) {
	unlock := h.lockVendorRequest(c.OrganizationID, c.Vendor, c.VendorRequestID)
	defer unlock()
	return h.releaseClaimLocked(ctx, c, req)
}

// releaseClaimLocked is releaseClaim for a caller that holds the lock of
// the vendor request. It reads the claim again, because another caller
// may have released it meanwhile.
func (h *Hub) releaseClaimLocked(ctx context.Context, c *Claim, req *work.Request) (bool, error) {
	cur, err := h.claim(ctx, h.svc.DB.Pool, c.RequestID)
	if errors.Is(err, errNoClaim) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if cur.State != ClaimHeld {
		return true, nil
	}
	v := h.vendors[cur.Vendor]
	if v == nil {
		return false, nil
	}
	in, err := h.integrations.Get(ctx, h.svc.DB.Pool, cur.OrganizationID, cur.IntegrationID)
	if err != nil {
		return false, fmt.Errorf("load agent integration: %w", err)
	}
	done, err := v.Release(ctx, in, cur, req)
	if done {
		if err := h.setClaimState(ctx, h.svc.DB.Pool, cur.RequestID, ClaimHeld, ClaimReleased); err != nil {
			return false, err
		}
		h.log.InfoContext(ctx, "agent_claim_released", slog.String("organization_id", cur.OrganizationID), slog.String("request_id", cur.RequestID),
			slog.String("vendor", VendorLabel(cur.Vendor)), slog.String("vendor_request_id", cur.VendorRequestID))
	}
	return done, err
}

// prepareOrgDeletion stops the organization's watchers and releases its
// held claims while the credentials still exist. This includes claims of
// requests the work layer already ended, because its release is best effort.
// A lookup error or an unconfirmed release keeps the organization and its
// credentials for a retry. The hook resumes its own watchers on error
// because the core calls Abort only for hooks whose Prepare succeeded.
func (h *Hub) prepareOrgDeletion(ctx context.Context, d api.OrganizationDeletion) (err error) {
	h.mu.Lock()
	h.stopped[d.OrganizationID]++
	for _, w := range h.watchers {
		if w.in.OrganizationID == d.OrganizationID {
			h.drainWatcher(w)
		}
	}
	stop := make([]*watcher, 0, len(h.draining[d.OrganizationID]))
	for w := range h.draining[d.OrganizationID] {
		stop = append(stop, w)
	}
	h.mu.Unlock()
	defer func() {
		if err != nil {
			h.resumeOrg(d.OrganizationID)
		}
	}()
	stopWatchers(stop)
	claims, err := h.heldClaims(ctx, strictClaimLookup, "c.organization_id = $1", d.OrganizationID)
	if err != nil {
		return err
	}
	var errs []error
	for _, hc := range claims {
		done, err := h.releaseClaim(ctx, hc.claim, hc.req)
		if err == nil && !done {
			err = errors.New("the vendor did not confirm the release")
		}
		if err != nil {
			h.log.WarnContext(ctx, "agent_claim_release_failed", slog.String("organization_id", d.OrganizationID), slog.String("request_id", hc.claim.RequestID), slog.Any("error", err))
			errs = append(errs, fmt.Errorf("release the vendor claim of request %s: %w", hc.claim.RequestID, err))
		}
	}
	return errors.Join(errs...)
}

// checkIntegrationDeletion refuses to delete an integration while tide
// holds a vendor claim of it, for example the claim of a busy runner that
// outlived the deletion of its profile, or a claim whose release failed.
// The deletion would remove the credentials and the claim row, so no
// poller could release the vendor claim. The lock of the integration row
// (FOR UPDATE) conflicts with the foreign key lock of a new claim row, so no
// claim appears after the check.
func (h *Hub) checkIntegrationDeletion(ctx context.Context, tx pgx.Tx, orgID, integrationID string) error {
	var held bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM agent_claims WHERE organization_id = $1 AND agent_integration_id = $2 AND state = $3)",
		orgID, integrationID, int16(ClaimHeld)).Scan(&held); err != nil {
		return err
	}
	if held {
		return api.ErrPrecondition("tide still holds a vendor claim of this agent integration; try again when its runners ended and tide released the claims")
	}
	return nil
}

func (h *Hub) resumeOrg(orgID string) {
	h.mu.Lock()
	if h.stopped[orgID] > 1 {
		h.stopped[orgID]--
		h.mu.Unlock()
		return
	}
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
