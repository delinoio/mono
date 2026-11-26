// SPDX-License-Identifier: FSL-1.1-ALv2

package agent

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/sandrun-api/internal/api"
	"github.com/delinoio/mono/servers/sandrun-api/internal/db"
	"github.com/delinoio/mono/servers/sandrun-api/internal/integration"
	"github.com/delinoio/mono/servers/sandrun-api/internal/work"
)

// Optional Vendor interfaces.
//
// Pusher marks a vendor that pushes its demand to SandRun (an OpenAI webhook)
// instead of being watched: the Hub runs no watcher for its integrations.
//
// Restarter is a vendor whose watch is a child process (the Claude Code
// orchestrator): after the watch ended with an error, the Hub calls
// Restarted instead of counting sandrun_vendor_watch_reconnects_total and
// logging vendor_watch_reconnect, so the vendor counts and logs its own
// restart.
//
// SandboxEnder maps an unexpected sandbox end of the vendor's runners
// differently from the default (the runner FAILED, the request FAILED with
// RUNNER_LOST); ok=false keeps the default.
type (
	Pusher interface {
		PushesDemand()
	}
	Restarter interface {
		Restarted(ctx context.Context, log *slog.Logger, backoff time.Duration, err error)
	}
	SandboxEnder interface {
		SandboxEnded(req *work.Request, reason ownsandv1.SandboxEndReason) (out work.SandboxOutcome, ok bool)
	}
)

// ErrNoProfile reports that no AGENT_SESSION runner profile names the
// integration, so SandRun cannot queue its demand.
var ErrNoProfile = errors.New("no runner profile names the agent integration")

// DirectDemand is demand that a vendor pushed to sandrun: a Claude Code spawn
// request or an OpenAI environment connection. SandRun claims nothing at the
// vendor; the claim row only records the vendor identity of the request,
// so the shared adapter, the poller and the release path work as for the
// claimed vendors.
type DirectDemand struct {
	// VendorRequestID identifies the demand at the vendor (Claude Code
	// order ID, OpenAI session ID).
	VendorRequestID string
	// Scope, Endpoint and WorkspaceDirectory are stored with the claim
	// (Claude Code: the session ID; OpenAI: the environment ID, the remote
	// URL and the workspace directory).
	Scope, Endpoint, WorkspaceDirectory string
	// Marker is the demand marker of the claim row.
	Marker string
	// SourceKey is the request key; empty selects VendorRequestID.
	SourceKey string
	// Repeat: when the newest request of the vendor request ended, a new
	// request is created with `#<n>` appended to the source key. Without
	// Repeat, any earlier request of the vendor request makes the demand a
	// duplicate.
	Repeat bool
	// MaxQueueWait caps the profile's maximum queue wait (zero: no cap).
	MaxQueueWait time.Duration
	// Store runs in the transaction right after the request was created,
	// for example to store the encrypted Claude Code work order.
	Store func(ctx context.Context, tx pgx.Tx, req *work.Request) error
}

// CreateDirectTx creates the request of pushed demand and its claim row in
// the caller's transaction. It returns the request and created=false when
// an open request of the vendor request exists, or, without Repeat, when
// any request of it exists: a repeated delivery never creates a second
// request. It fails with ErrNoProfile when no profile names the
// integration. The caller calls Service.WorkChanged after the commit.
func (h *Hub) CreateDirectTx(ctx context.Context, tx pgx.Tx, v Vendor, in integration.Integration, d DirectDemand) (*work.Request, bool, error) {
	c, req, err := h.latestClaim(ctx, tx, in.OrganizationID, in.Vendor, d.VendorRequestID)
	if err != nil {
		return nil, false, err
	}
	if c != nil && (!d.Repeat || !work.RequestTerminal(req.State)) {
		return req, false, nil
	}
	profileID, err := demandProfileID(ctx, tx, in)
	if err != nil {
		return nil, false, err
	}
	if profileID == "" {
		return nil, false, ErrNoProfile
	}
	p, err := h.lockDemandProfile(ctx, tx, in, profileID)
	if err != nil {
		return nil, false, err
	}
	base := d.SourceKey
	if base == "" {
		base = d.VendorRequestID
	}
	key, err := h.nextSourceKey(ctx, tx, in.OrganizationID, v.Source(), base)
	if err != nil {
		return nil, false, err
	}
	requestID := api.NewID()
	r, created, err := h.engine.CreateRequest(ctx, tx, work.NewRequest{Profile: p, Source: v.Source(), SourceKey: key, RequestID: requestID, MaxQueueWait: d.MaxQueueWait})
	if err != nil || !created {
		return r, false, err
	}
	now := h.svc.NowUTC()
	if _, err := tx.Exec(ctx, `INSERT INTO agent_claims (request_id, organization_id, agent_integration_id, vendor, vendor_request_id, demand_marker,
		worker_id, scope, endpoint, workspace_directory, state, create_time, update_time) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $12)`,
		r.ID, in.OrganizationID, in.ID, int16(in.Vendor), d.VendorRequestID, d.Marker, "sandrun-"+r.ID, d.Scope, d.Endpoint, d.WorkspaceDirectory, int16(ClaimHeld), now); err != nil {
		return nil, false, err
	}
	if d.Store != nil {
		if err := d.Store(ctx, tx, r); err != nil {
			return nil, false, err
		}
	}
	return r, true, nil
}

// LatestClaim returns the newest claim of a vendor request of an
// integration's organization and its request, or nil.
func (h *Hub) LatestClaim(ctx context.Context, q db.Querier, in integration.Integration, vendorRequestID string) (*Claim, *work.Request, error) {
	return h.latestClaim(ctx, q, in.OrganizationID, in.Vendor, vendorRequestID)
}

// Integrations returns the integration store.
func (h *Hub) Integrations() *integration.Store { return h.integrations }
