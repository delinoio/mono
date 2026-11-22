// SPDX-License-Identifier: FSL-1.1-ALv2

package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/servers/tide-api/internal/api"
	"github.com/delinoio/mono/servers/tide-api/internal/db"
	"github.com/delinoio/mono/servers/tide-api/internal/integration"
	"github.com/delinoio/mono/servers/tide-api/internal/profile"
	"github.com/delinoio/mono/servers/tide-api/internal/work"
)

// ClaimState is the state of a stored vendor claim.
type ClaimState int16

const (
	// ClaimHeld: tide holds the claim at the vendor.
	ClaimHeld ClaimState = 1
	// ClaimReleased: tide released the claim, or the vendor no longer
	// gives it to tide.
	ClaimReleased ClaimState = 2
)

// Claim is the vendor claim of one request.
type Claim struct {
	RequestID       string
	OrganizationID  string
	IntegrationID   string
	Vendor          tidev1.AgentVendor
	VendorRequestID string
	DemandMarker    string
	// WorkerID is the worker ID (Cursor) or acceptor ID (Devin) that tide
	// chose for the claim; the runner program uses the same value.
	WorkerID string
	// Scope is what the claim names: the Cursor pool name or the Devin
	// outpost ID.
	Scope      string
	State      ClaimState
	CreateTime time.Time
}

const claimColumns = `c.request_id::text, c.organization_id::text, c.agent_integration_id::text, c.vendor, c.vendor_request_id, c.demand_marker,
	c.worker_id, c.scope, c.state, c.create_time`

func scanClaim(row interface{ Scan(dest ...any) error }) (*Claim, error) {
	var c Claim
	var vendor, state int16
	dest := []any{&c.RequestID, &c.OrganizationID, &c.IntegrationID, &vendor, &c.VendorRequestID, &c.DemandMarker,
		&c.WorkerID, &c.Scope, &state, &c.CreateTime}
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}
	c.Vendor, c.State = tidev1.AgentVendor(vendor), ClaimState(state)
	return &c, nil
}

var errNoClaim = errors.New("the request has no vendor claim")

// claim loads the claim of a request.
func (h *Hub) claim(ctx context.Context, q db.Querier, requestID string) (*Claim, error) {
	c, err := scanClaim(q.QueryRow(ctx, "SELECT "+claimColumns+" FROM agent_claims c WHERE c.request_id = $1", requestID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNoClaim
	}
	return c, err
}

// Claim returns the claim of a request, for tests and vendor layers.
func (h *Hub) Claim(ctx context.Context, requestID string) (*Claim, error) {
	return h.claim(ctx, h.svc.DB.Pool, requestID)
}

func (h *Hub) setClaimState(ctx context.Context, q db.Querier, requestID string, from, to ClaimState) error {
	_, err := q.Exec(ctx, "UPDATE agent_claims SET state = $3, update_time = $4 WHERE request_id = $1 AND state = $2", requestID, int16(from), int16(to), h.svc.NowUTC())
	return err
}

// heldClaim is a held claim with its request.
type heldClaim struct {
	claim *Claim
	req   *work.Request
}

// heldClaims returns the held claims of registered vendors whose request
// matches where (c is agent_claims, r is requests), with their requests.
func (h *Hub) heldClaims(ctx context.Context, where string, args ...any) ([]heldClaim, error) {
	vendors := make([]int16, 0, len(h.vendors))
	for v := range h.vendors {
		vendors = append(vendors, int16(v))
	}
	args = append(args, vendors)
	rows, err := h.svc.DB.Pool.Query(ctx, "SELECT "+claimColumns+" FROM agent_claims c JOIN requests r ON r.request_id = c.request_id WHERE c.state = 1 AND c.vendor = ANY($"+
		strconv.Itoa(len(args))+") AND "+where+" ORDER BY c.create_time", args...)
	if err != nil {
		return nil, err
	}
	var claims []*Claim
	for rows.Next() {
		c, err := scanClaim(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		claims = append(claims, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]heldClaim, 0, len(claims))
	for _, c := range claims {
		req, err := h.engine.GetRequest(ctx, h.svc.DB.Pool, c.OrganizationID, c.RequestID)
		if err != nil {
			continue
		}
		out = append(out, heldClaim{claim: c, req: req})
	}
	return out, nil
}

// Demand is one pending vendor request that a watch saw.
type Demand struct {
	// VendorRequestID is the Cursor request ID or the Devin session ID.
	VendorRequestID string
	// Marker identifies this pending entry of the vendor request (Cursor
	// createdAtMs, Devin metadata.created_at).
	Marker string
	// Scope is what the claim names (Cursor pool name, Devin outpost ID).
	Scope string
	// Claim claims the vendor request for workerID. It returns
	// ErrClaimTaken when another worker holds the request.
	Claim func(ctx context.Context, workerID string) error
}

// ErrClaimTaken reports that another worker holds a vendor request; it is
// normal operation, not a failure.
var ErrClaimTaken = errors.New("another worker holds the vendor request")

// latestClaim returns the newest claim of a vendor request of the
// organization with the state of its request, and the number of claims.
func (h *Hub) latestClaim(ctx context.Context, q db.Querier, orgID string, vendor tidev1.AgentVendor, vendorRequestID string) (*Claim, *work.Request, int, error) {
	var n int
	if err := q.QueryRow(ctx, "SELECT count(*) FROM agent_claims WHERE organization_id = $1 AND vendor = $2 AND vendor_request_id = $3",
		orgID, int16(vendor), vendorRequestID).Scan(&n); err != nil {
		return nil, nil, 0, err
	}
	if n == 0 {
		return nil, nil, 0, nil
	}
	c, err := scanClaim(q.QueryRow(ctx, `SELECT `+claimColumns+` FROM agent_claims c WHERE c.organization_id = $1 AND c.vendor = $2 AND c.vendor_request_id = $3
		ORDER BY c.create_time DESC, c.request_id DESC LIMIT 1`, orgID, int16(vendor), vendorRequestID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, n, nil
	}
	if err != nil {
		return nil, nil, 0, err
	}
	req, err := h.engine.GetRequest(ctx, q, orgID, c.RequestID)
	if err != nil {
		return nil, nil, 0, err
	}
	return c, req, n, nil
}

// profileFor returns the runner profile that receives the requests of an
// integration (recorded decision): the oldest AGENT_SESSION profile that
// names it. Other profiles that name the same integration get no
// requests; they can still keep warm runners.
func (h *Hub) profileFor(ctx context.Context, q db.Querier, in integration.Integration) (*profile.Profile, error) {
	var id string
	err := q.QueryRow(ctx, `SELECT runner_profile_id::text FROM runner_profiles WHERE organization_id = $1 AND agent_integration_id = $2 AND workload_kind = $3
		ORDER BY create_time, runner_profile_id LIMIT 1`, in.OrganizationID, in.ID, int16(tidev1.WorkloadKind_WORKLOAD_KIND_AGENT_SESSION)).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return h.engine.Profiles().Get(ctx, q, in.OrganizationID, id)
}

// admit claims a pending vendor request and creates its tide request:
//
//   - When an open request of the vendor request exists and is QUEUED or
//     STARTING, the vendor shows the entry as pending again: either a
//     repeated event, or tide's claim lapsed (for example, at a Devin claim
//     deadline). tide claims it again with the same worker ID; a conflict
//     changes nothing, because the claim can still be tide's.
//   - When the newest request of the vendor request ended because a user
//     canceled it in tide, the same pending entry (same marker) is not
//     claimed again.
//   - Otherwise tide claims it with a new worker ID and creates a request
//     with the oldest profile that names the integration. The source key is
//     the vendor request ID, with `#<n>` for the n-th request of the same
//     vendor request (a Cursor follow-up or a Devin resume reuses the ID).
func (h *Hub) admit(ctx context.Context, v Vendor, in integration.Integration, d Demand) error {
	logAttrs := []slog.Attr{slog.String("organization_id", in.OrganizationID), slog.String("agent_integration_id", in.ID),
		slog.String("vendor", VendorLabel(in.Vendor)), slog.String("vendor_request_id", d.VendorRequestID)}
	c, req, n, err := h.latestClaim(ctx, h.svc.DB.Pool, in.OrganizationID, in.Vendor, d.VendorRequestID)
	if err != nil {
		return err
	}
	if c != nil && !work.RequestTerminal(req.State) {
		if req.State == tidev1.RequestState_REQUEST_STATE_RUNNING {
			return nil
		}
		err := d.Claim(ctx, c.WorkerID)
		if errors.Is(err, ErrClaimTaken) {
			// The claim may still be tide's (a repeated event of an entry
			// that tide holds) or another worker's; only an explicit vendor
			// report (Watch.Lost) ends the request.
			return nil
		}
		if err != nil {
			return err
		}
		if c.State != ClaimHeld {
			if err := h.setClaimState(ctx, h.svc.DB.Pool, c.RequestID, ClaimReleased, ClaimHeld); err != nil {
				return err
			}
		}
		h.log.LogAttrs(ctx, slog.LevelInfo, "agent_claim_renewed", append(logAttrs, slog.String("request_id", c.RequestID))...)
		return nil
	}
	if c != nil && canceledBeforeStart(req) && c.DemandMarker == d.Marker {
		return nil
	}
	p, err := h.profileFor(ctx, h.svc.DB.Pool, in)
	if err != nil {
		return err
	}
	if p == nil {
		h.log.LogAttrs(ctx, slog.LevelDebug, "agent_demand_ignored", append(logAttrs, slog.String("cause", "no_profile"))...)
		return nil
	}
	requestID := api.NewID()
	workerID := "tide-" + requestID
	if err := d.Claim(ctx, workerID); err != nil {
		if errors.Is(err, ErrClaimTaken) {
			h.log.LogAttrs(ctx, slog.LevelInfo, "agent_demand_ignored", append(logAttrs, slog.String("cause", "claimed_elsewhere"))...)
			return nil
		}
		return err
	}
	key := d.VendorRequestID
	if n > 0 {
		key += "#" + strconv.Itoa(n+1)
	}
	claim := &Claim{RequestID: requestID, OrganizationID: in.OrganizationID, IntegrationID: in.ID, Vendor: in.Vendor, VendorRequestID: d.VendorRequestID,
		DemandMarker: d.Marker, WorkerID: workerID, Scope: d.Scope, State: ClaimHeld}
	var created bool
	err = h.inTx(ctx, func(tx pgx.Tx) error {
		r, ok, err := h.engine.CreateRequest(ctx, tx, work.NewRequest{Profile: p, Source: v.Source(), SourceKey: key, RequestID: requestID})
		if err != nil || !ok {
			created = false
			return err
		}
		created = true
		now := h.svc.NowUTC()
		_, err = tx.Exec(ctx, `INSERT INTO agent_claims (request_id, organization_id, agent_integration_id, vendor, vendor_request_id, demand_marker,
			worker_id, scope, state, create_time, update_time) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $10)`,
			r.ID, in.OrganizationID, in.ID, int16(in.Vendor), d.VendorRequestID, d.Marker, workerID, d.Scope, int16(ClaimHeld), now)
		return err
	})
	if err != nil || !created {
		// Another admission of the same vendor request won, or the store
		// failed: give the claim back so the vendor does not wait for a
		// worker that never comes.
		probe := &work.Request{ID: requestID, OrganizationID: in.OrganizationID, State: tidev1.RequestState_REQUEST_STATE_CANCELED,
			EndReason: tidev1.RequestEndReason_REQUEST_END_REASON_CANCELED_BY_SOURCE}
		if _, rerr := v.Release(context.WithoutCancel(ctx), in, claim, probe); rerr != nil {
			h.log.LogAttrs(ctx, slog.LevelWarn, "agent_claim_release_failed", append(logAttrs, slog.Any("error", rerr))...)
		}
		if err != nil {
			return fmt.Errorf("store request: %w", err)
		}
		return nil
	}
	h.log.LogAttrs(ctx, slog.LevelInfo, "agent_request_claimed", append(logAttrs, slog.String("request_id", requestID), slog.String("runner_profile_id", p.ID))...)
	h.svc.WorkChanged(in.OrganizationID)
	return nil
}

// canceledBeforeStart reports whether a user canceled the request in tide
// before a runner took its session; then tide does not claim the same
// vendor entry again. The rule reads the request's end, which the
// cancellation commits, and not a flag that a later release step sets, so
// it also holds when the claim poller releases the claim after a restart
// or before the cancellation's own release step.
func canceledBeforeStart(req *work.Request) bool {
	return req.StartTime == nil && req.EndReason == tidev1.RequestEndReason_REQUEST_END_REASON_CANCELED_BY_USER
}

// lost marks a claim as no longer tide's and ends its request when no
// runner started for it yet.
func (h *Hub) lost(ctx context.Context, c *Claim, req *work.Request) error {
	if err := h.setClaimState(ctx, h.svc.DB.Pool, c.RequestID, ClaimHeld, ClaimReleased); err != nil {
		return err
	}
	if req.State != tidev1.RequestState_REQUEST_STATE_QUEUED {
		// A runner starts or serves the request; its program exit ends it.
		return nil
	}
	_, err := h.engine.EndRequest(ctx, c.OrganizationID, c.RequestID, work.RequestEnd{State: tidev1.RequestState_REQUEST_STATE_CANCELED,
		Reason: tidev1.RequestEndReason_REQUEST_END_REASON_CANCELED_BY_SOURCE})
	return err
}
