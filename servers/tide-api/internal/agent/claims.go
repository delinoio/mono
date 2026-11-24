// SPDX-License-Identifier: FSL-1.1-ALv2

package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"connectrpc.com/connect"
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
	Scope string
	// Endpoint is a non-secret address that the runner program needs
	// (OpenAI Codex: the session's environment.remote_url); empty for the
	// other vendors.
	Endpoint string
	// WorkspaceDirectory is the guest directory that the session expects
	// (OpenAI Codex: environment.workspace_directory); empty otherwise.
	WorkspaceDirectory string
	State              ClaimState
	CreateTime         time.Time
}

const claimColumns = `c.request_id::text, c.organization_id::text, c.agent_integration_id::text, c.vendor, c.vendor_request_id, c.demand_marker,
	c.worker_id, c.scope, c.endpoint, c.workspace_directory, c.state, c.create_time`

func scanClaim(row interface{ Scan(dest ...any) error }) (*Claim, error) {
	var c Claim
	var vendor, state int16
	dest := []any{&c.RequestID, &c.OrganizationID, &c.IntegrationID, &vendor, &c.VendorRequestID, &c.DemandMarker,
		&c.WorkerID, &c.Scope, &c.Endpoint, &c.WorkspaceDirectory, &state, &c.CreateTime}
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

// latestVendorClaim returns the newest claim of a vendor request, or nil.
func (h *Hub) latestVendorClaim(ctx context.Context, q db.Querier, orgID string, vendor tidev1.AgentVendor, vendorRequestID string) (*Claim, error) {
	c, err := scanClaim(q.QueryRow(ctx, `SELECT `+claimColumns+` FROM agent_claims c WHERE c.organization_id = $1 AND c.vendor = $2 AND c.vendor_request_id = $3
		ORDER BY c.create_time DESC, c.request_id DESC LIMIT 1`, orgID, int16(vendor), vendorRequestID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return c, err
}

// latestClaim returns the newest claim of a vendor request with its request.
func (h *Hub) latestClaim(ctx context.Context, q db.Querier, orgID string, vendor tidev1.AgentVendor, vendorRequestID string) (*Claim, *work.Request, error) {
	c, err := h.latestVendorClaim(ctx, q, orgID, vendor, vendorRequestID)
	if err != nil || c == nil {
		return nil, nil, err
	}
	req, err := h.engine.GetRequest(ctx, q, orgID, c.RequestID)
	if err != nil {
		return nil, nil, err
	}
	return c, req, nil
}

// nextSourceKey returns the source key of a new request of a vendor
// request: the vendor request ID when no request of the source has it, and
// otherwise `<vendor request ID>#<n>` with n one more than the highest
// suffix of the stored requests (the first request counts as 1). The
// retention job deletes old ended requests with their claims, so a count of
// the stored claims could select a key that a newer request still has.
func (h *Hub) nextSourceKey(ctx context.Context, q db.Querier, orgID string, source work.Source, vendorRequestID string) (string, error) {
	rows, err := q.Query(ctx, `SELECT source_key FROM requests WHERE organization_id = $1 AND source = $2
		AND (source_key = $3 OR left(source_key, length($3) + 1) = $3 || '#')`, orgID, int16(source), vendorRequestID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	high := 0
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return "", err
		}
		if key == vendorRequestID {
			high = max(high, 1)
		} else if n, err := strconv.Atoi(key[len(vendorRequestID)+1:]); err == nil && n > 0 {
			high = max(high, n)
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if high == 0 {
		return vendorRequestID, nil
	}
	return vendorRequestID + "#" + strconv.Itoa(high+1), nil
}

// demandProfileID selects the oldest AGENT_SESSION runner profile of the
// integration. The other profiles can still keep warm runners.
func demandProfileID(ctx context.Context, q db.Querier, in integration.Integration) (string, error) {
	var id string
	err := q.QueryRow(ctx, `SELECT runner_profile_id::text FROM runner_profiles WHERE organization_id = $1 AND agent_integration_id = $2 AND workload_kind = $3
		ORDER BY create_time, runner_profile_id LIMIT 1`, in.OrganizationID, in.ID, int16(tidev1.WorkloadKind_WORKLOAD_KIND_AGENT_SESSION)).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// lockDemandProfile locks the organization and profile in that order, then
// checks that the selected profile still serves this integration.
func (h *Hub) lockDemandProfile(ctx context.Context, tx pgx.Tx, in integration.Integration, id string) (*profile.Profile, error) {
	if err := api.LockOrganizationShare(ctx, tx, in.OrganizationID); err != nil {
		if connect.CodeOf(err) == connect.CodeNotFound {
			return nil, ErrNoProfile
		}
		return nil, err
	}
	if err := profile.LockShare(ctx, tx, in.OrganizationID, id); err != nil {
		if connect.CodeOf(err) == connect.CodeNotFound {
			return nil, ErrNoProfile
		}
		return nil, err
	}
	p, err := h.engine.Profiles().Get(ctx, tx, in.OrganizationID, id)
	if err != nil {
		return nil, err
	}
	if p.Kind != tidev1.WorkloadKind_WORKLOAD_KIND_AGENT_SESSION || p.AgentIntegrationID != in.ID {
		return nil, ErrNoProfile
	}
	return p, nil
}

// admit claims a pending vendor request and creates its tide request:
//
//   - When an open request of the vendor request exists and is QUEUED or
//     STARTING, the vendor shows the entry as pending again: either a
//     repeated event, or tide's claim lapsed (for example, at a Devin claim
//     deadline). tide claims it again with the same worker ID; a conflict
//     changes nothing, because the claim can still be tide's.
//   - When the newest request of the vendor request ended and tide still
//     holds its claim, tide releases that claim first; while the vendor
//     does not confirm the release, tide claims nothing.
//   - When the newest request of the vendor request ended because a user
//     canceled it in tide, the same pending entry (same marker) is not
//     claimed again.
//   - Otherwise tide claims it with a new worker ID and creates a request
//     with the oldest profile that names the integration. The source key is
//     the vendor request ID, with `#<n>` for a later request of the same
//     vendor request (a Cursor follow-up or a Devin resume reuses the ID);
//     see nextSourceKey.
func (h *Hub) admit(ctx context.Context, v Vendor, in integration.Integration, d Demand) error {
	logAttrs := []slog.Attr{slog.String("organization_id", in.OrganizationID), slog.String("agent_integration_id", in.ID),
		slog.String("vendor", VendorLabel(in.Vendor)), slog.String("vendor_request_id", d.VendorRequestID)}
	unlock := h.lockVendorRequest(in.OrganizationID, in.Vendor, d.VendorRequestID)
	defer unlock()
	c, req, err := h.latestClaim(ctx, h.svc.DB.Pool, in.OrganizationID, in.Vendor, d.VendorRequestID)
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
	if c != nil && c.State == ClaimHeld {
		// The earlier request ended, but tide did not release its claim
		// yet. Release it before the new claim, because a later release
		// would end the new claim (see lockVendorRequest).
		done, err := h.releaseClaimLocked(ctx, c, req)
		if err != nil {
			return fmt.Errorf("release the earlier claim: %w", err)
		}
		if !done {
			// The next list or event admits the entry.
			h.log.LogAttrs(ctx, slog.LevelInfo, "agent_demand_deferred", append(logAttrs, slog.String("request_id", c.RequestID), slog.String("cause", "earlier_claim_held"))...)
			return nil
		}
	}
	if c != nil && canceledBeforeStart(req) && c.DemandMarker == d.Marker {
		return nil
	}
	profileID, err := demandProfileID(ctx, h.svc.DB.Pool, in)
	if err != nil {
		return err
	}
	if profileID == "" {
		h.log.LogAttrs(ctx, slog.LevelDebug, "agent_demand_ignored", append(logAttrs, slog.String("cause", "no_profile"))...)
		return nil
	}
	key, err := h.nextSourceKey(ctx, h.svc.DB.Pool, in.OrganizationID, v.Source(), d.VendorRequestID)
	if err != nil {
		return err
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
	claim := &Claim{RequestID: requestID, OrganizationID: in.OrganizationID, IntegrationID: in.ID, Vendor: in.Vendor, VendorRequestID: d.VendorRequestID,
		DemandMarker: d.Marker, WorkerID: workerID, Scope: d.Scope, State: ClaimHeld}
	var created bool
	err = h.inTx(ctx, func(tx pgx.Tx) error {
		p, err := h.lockDemandProfile(ctx, tx, in, profileID)
		if errors.Is(err, ErrNoProfile) {
			h.log.LogAttrs(ctx, slog.LevelDebug, "agent_demand_ignored", append(logAttrs, slog.String("cause", "profile_deleted"))...)
			return nil
		}
		if err != nil {
			return err
		}
		r, ok, err := h.engine.CreateRequest(ctx, tx, work.NewRequest{Profile: p, Source: v.Source(), SourceKey: key, RequestID: requestID})
		created = ok
		if err != nil || !ok {
			return err
		}
		now := h.svc.NowUTC()
		_, err = tx.Exec(ctx, `INSERT INTO agent_claims (request_id, organization_id, agent_integration_id, vendor, vendor_request_id, demand_marker,
			worker_id, scope, state, create_time, update_time) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $10)`,
			r.ID, in.OrganizationID, in.ID, int16(in.Vendor), d.VendorRequestID, d.Marker, workerID, d.Scope, int16(ClaimHeld), now)
		return err
	})
	if err != nil || !created {
		// Give the claim back when this admission did not store a request,
		// so the vendor does not wait for a worker that never comes.
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
	h.log.LogAttrs(ctx, slog.LevelInfo, "agent_request_claimed", append(logAttrs, slog.String("request_id", requestID), slog.String("runner_profile_id", profileID))...)
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

// lost records that tide no longer holds the claim. Only a request that
// is still queued under the dispatch lock ends with CANCELED_BY_SOURCE.
func (h *Hub) lost(ctx context.Context, orgID string, vendor tidev1.AgentVendor, vendorRequestID string) error {
	unlock := h.lockVendorRequest(orgID, vendor, vendorRequestID)
	defer unlock()

	var canceled *work.Request
	err := h.inTx(ctx, func(tx pgx.Tx) error {
		c, err := h.latestVendorClaim(ctx, tx, orgID, vendor, vendorRequestID)
		if err != nil || c == nil {
			return err
		}
		req, err := h.engine.LockRequest(ctx, tx, c.RequestID)
		if connect.CodeOf(err) == connect.CodeNotFound {
			// Retention can remove the request and claim after the lookup.
			return nil
		}
		if err != nil || work.RequestTerminal(req.State) {
			return err
		}
		if err := h.setClaimState(ctx, tx, req.ID, ClaimHeld, ClaimReleased); err != nil {
			return err
		}
		if req.State != tidev1.RequestState_REQUEST_STATE_QUEUED {
			return nil
		}
		canceled, err = h.engine.EndRequestTx(ctx, tx, req.ID, work.RequestEnd{State: tidev1.RequestState_REQUEST_STATE_CANCELED,
			Reason: tidev1.RequestEndReason_REQUEST_END_REASON_CANCELED_BY_SOURCE})
		return err
	})
	if err == nil && canceled != nil {
		h.engine.Kick(orgID)
	}
	return err
}
