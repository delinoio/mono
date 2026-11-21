// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/delinoio/mono/go/ownsand/relaybinding"
	"github.com/delinoio/mono/go/ownsand/uuidv7"
	"github.com/delinoio/mono/go/ownsand/validate"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
	"github.com/delinoio/mono/servers/ownsand-api/internal/auth"
	"github.com/delinoio/mono/servers/ownsand-api/internal/authz"
	"github.com/delinoio/mono/servers/ownsand-api/internal/events"
	"github.com/delinoio/mono/servers/ownsand-api/internal/grants"
	"github.com/delinoio/mono/servers/ownsand-api/internal/hub"
	"github.com/delinoio/mono/servers/ownsand-api/internal/ratelimit"
	"github.com/delinoio/mono/servers/ownsand-api/internal/relay"
)

// Data path timing of the contract.
const (
	sessionLifetime  = time.Hour
	authorizeRate    = 10 // AuthorizeRelay calls per second per principal
	relayWaitForConn = 10 * time.Second
)

// session is an open peer-to-peer session that the lease holder signaled.
type session struct {
	id, orgID, sandboxID, deviceID string
	gen                            uint64
	principal                      auth.Principal
	expireAt                       time.Time
}

// revocation is a session revocation that waits for its acknowledgement.
type revocation struct {
	deviceID string
	gen      uint64
}

// DataPlane holds the in-memory data path state of the lease holder:
// sessions, relay grants, pending revocations and agent relay connections.
type DataPlane struct {
	S       *Service
	Grants  *grants.Registry
	Relays  *relay.Registry
	Limits  relay.Limits
	Timing  RelayTiming
	limiter *ratelimit.Limiter
	// failures limits relay RPCs that fail the grant check.
	failures *ratelimit.Limiter

	signals *hub.Waiters[string, *ownsandv1.SignalingAnswer]

	mu          sync.Mutex
	sessions    map[string]*session
	revocations map[string]revocation
}

// RelayTiming holds the relay stream durations; tests shorten them.
type RelayTiming struct {
	// Rotation ends a server stream before 14 minutes.
	Rotation time.Duration
	// Keepalive is the caller-side keepalive interval (at most 60 s).
	Keepalive time.Duration
	// Acceptance is the wait for the agent's accepted notice (30 s).
	Acceptance time.Duration
	// RequestDeadline bounds forwarding the request frame (5 min), and the
	// whole WriteFile call.
	RequestDeadline time.Duration
	// GrantAck bounds the wait for a relay grant acknowledgement (10 s) and
	// Signaling the wait for an SDP answer (30 s).
	GrantAck  time.Duration
	Signaling time.Duration
}

// DefaultRelayTiming returns the contract values.
func DefaultRelayTiming() RelayTiming {
	return RelayTiming{Rotation: 14*time.Minute - 10*time.Second, Keepalive: 50 * time.Second, Acceptance: 30 * time.Second, RequestDeadline: 5 * time.Minute,
		GrantAck: 10 * time.Second, Signaling: 30 * time.Second}
}

// NewDataPlane wires the data path into s.
func NewDataPlane(s *Service, limits relay.Limits, timing RelayTiming) *DataPlane {
	p := &DataPlane{
		S: s, Grants: grants.New(s.Now), Relays: relay.NewRegistry(), Limits: limits, Timing: timing,
		limiter:     ratelimit.New(authorizeRate, authorizeRate, s.Now),
		failures:    ratelimit.New(10.0/60, 10, s.Now),
		signals:     hub.NewWaiters[string, *ownsandv1.SignalingAnswer](),
		sessions:    map[string]*session{},
		revocations: map[string]revocation{},
	}
	s.DataPath = p
	s.Hub.OnClose(p.channelClosed)
	return p
}

// channelClosed ends everything bound to an agent channel: its sessions,
// its relay grants, its pending revocations and its relay connection.
func (p *DataPlane) channelClosed(c *hub.Conn) {
	p.Grants.RemoveChannel(c.DeviceID, c.Generation)
	p.Relays.CloseChannel(c.DeviceID, c.Generation)
	p.mu.Lock()
	for id, s := range p.sessions {
		if s.deviceID == c.DeviceID && s.gen == c.Generation {
			delete(p.sessions, id)
		}
	}
	for id, r := range p.revocations {
		if r.deviceID == c.DeviceID && r.gen == c.Generation {
			delete(p.revocations, id)
		}
	}
	p.mu.Unlock()
}

// HandleEnvelope implements DataPathHooks.
func (p *DataPlane) HandleEnvelope(ctx context.Context, conn *hub.Conn, env *ownsandv1.AgentEnvelope) bool {
	switch m := env.GetMessage().(type) {
	case *ownsandv1.AgentEnvelope_Signaling:
		if a := m.Signaling.GetAnswer(); a != nil {
			p.signals.Deliver(conn.DeviceID+"/"+a.GetSessionId(), a)
		}
	case *ownsandv1.AgentEnvelope_RelayGrantAck:
		a := m.RelayGrantAck
		if g := p.Grants.Get(a.GetGrantId()); g != nil && g.DeviceID == conn.DeviceID && g.ChannelGen == conn.Generation {
			p.Grants.Ack(a.GetGrantId(), a.GetResult(), a.GetReplacedGrantId())
		}
		p.S.Logger.InfoContext(ctx, "relay_grant_ack", slog.String("device_id", conn.DeviceID), slog.String("grant_id", a.GetGrantId()), slog.String("result", a.GetResult().String()))
	case *ownsandv1.AgentEnvelope_RelayGrantRelease:
		p.Grants.Release(m.RelayGrantRelease.GetGrantId(), conn.DeviceID)
		p.S.Logger.InfoContext(ctx, "relay_grant_released", slog.String("device_id", conn.DeviceID), slog.String("grant_id", m.RelayGrantRelease.GetGrantId()))
	case *ownsandv1.AgentEnvelope_RevocationAck:
		p.mu.Lock()
		delete(p.revocations, m.RevocationAck.GetRevocationId())
		p.mu.Unlock()
	default:
		return false
	}
	return true
}

// HandleEvent implements DataPathHooks: it revokes the sessions and relay
// grants that a revocation event matches.
func (p *DataPlane) HandleEvent(ctx context.Context, e events.Event) {
	matchPrincipal := func(orgID string, pr auth.Principal) bool {
		switch e.Kind {
		case events.KindAPIKeyRevoked:
			return pr.IsAPIKey() && pr.ID == e.APIKeyID
		case events.KindMembershipEnded:
			return pr.IsUser() && pr.ID == e.UserID && orgID == e.OrganizationID
		case events.KindOrganizationDeleted:
			return orgID == e.OrganizationID
		}
		return false
	}
	matchSandbox := func(sandboxID, deviceID string) bool {
		return (e.Kind == events.KindSandboxDeleted && sandboxID == e.SandboxID) || (e.Kind == events.KindDeviceRemoved && deviceID == e.DeviceID)
	}
	revoked := p.Grants.RemoveMatching(func(g *grants.Grant) bool {
		return matchPrincipal(g.OrgID, g.Principal) || matchSandbox(g.SandboxID, g.DeviceID)
	})
	for _, g := range revoked {
		p.sendRevocation(ctx, g.DeviceID, g.ChannelGen, &ownsandv1.SessionRevocation{Target: &ownsandv1.SessionRevocation_RelayGrantId{RelayGrantId: g.ID}})
	}
	p.mu.Lock()
	var sessions []*session
	for id, s := range p.sessions {
		if matchPrincipal(s.orgID, s.principal) || matchSandbox(s.sandboxID, s.deviceID) {
			sessions = append(sessions, s)
			delete(p.sessions, id)
		}
	}
	p.mu.Unlock()
	for _, s := range sessions {
		p.sendRevocation(ctx, s.deviceID, s.gen, &ownsandv1.SessionRevocation{Target: &ownsandv1.SessionRevocation_SessionId{SessionId: s.id}})
	}
}

func (p *DataPlane) sendRevocation(ctx context.Context, deviceID string, gen uint64, r *ownsandv1.SessionRevocation) {
	conn := p.S.Hub.Get(deviceID)
	if conn == nil || conn.Generation != gen {
		// The channel that signaled the session or sent the grant is gone,
		// so the agent already ended them.
		return
	}
	r.RevocationId = newID()
	p.mu.Lock()
	p.revocations[r.RevocationId] = revocation{deviceID: deviceID, gen: gen}
	p.mu.Unlock()
	if err := conn.Send(ctx, &ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_SessionRevocation{SessionRevocation: r}}); err != nil {
		p.mu.Lock()
		delete(p.revocations, r.RevocationId)
		p.mu.Unlock()
		return
	}
	p.S.Logger.InfoContext(ctx, "session_revocation_sent", slog.String("device_id", deviceID), slog.String("revocation_id", r.RevocationId),
		slog.String("session_id", r.GetSessionId()), slog.String("grant_id", r.GetRelayGrantId()))
}

// PendingRevocations returns the number of unacknowledged revocations.
func (p *DataPlane) PendingRevocations() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.revocations)
}

// sweepSessions forgets sessions past their absolute expiry.
func (p *DataPlane) sweepSessions(now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, s := range p.sessions {
		if !now.Before(s.expireAt) {
			delete(p.sessions, id)
		}
	}
}

// DataPathHandler serves DataPathService.
type DataPathHandler struct{ *DataPlane }

var _ ownsandv1connect.DataPathServiceHandler = DataPathHandler{}

func (h DataPathHandler) GetIceServers(ctx context.Context, req *connect.Request[ownsandv1.GetIceServersRequest]) (*connect.Response[ownsandv1.GetIceServersResponse], error) {
	if _, _, err := h.S.authorize(ctx, h.S.DB.Pool, req.Msg.GetOrganizationId(), authz.ActionUseSandboxes); err != nil {
		return nil, err
	}
	resp := &ownsandv1.GetIceServersResponse{}
	if len(h.S.STUNURLs) > 0 {
		resp.IceServers = []*ownsandv1.IceServer{{Urls: h.S.STUNURLs}}
	}
	return connect.NewResponse(resp), nil
}

// liveSandbox loads a sandbox for a data path request and applies the state
// rule: UNREACHABLE is unavailable, a state outside allowed is
// failed_precondition, and a device without a live channel is unavailable.
func (p *DataPlane) liveSandbox(ctx context.Context, orgID, sandboxID string, allowed ...ownsandv1.SandboxState) (*sandboxRow, *hub.Conn, error) {
	if err := validateID("sandbox_id", sandboxID); err != nil {
		return nil, nil, err
	}
	sb, err := loadSandbox(ctx, p.S.DB.Pool, orgID, sandboxID, false)
	if err != nil {
		return nil, nil, err
	}
	state, _ := sb.shownState()
	ok := false
	for _, a := range allowed {
		ok = ok || state == a
	}
	if !ok {
		return nil, nil, stateErr(state, "the data path")
	}
	conn := p.S.Hub.Live(sb.DeviceID)
	if conn == nil {
		return nil, nil, errUnavailable("the sandbox's device has no live agent channel on this instance")
	}
	return sb, conn, nil
}

func (h DataPathHandler) OpenSession(ctx context.Context, req *connect.Request[ownsandv1.OpenSessionRequest]) (*connect.Response[ownsandv1.OpenSessionResponse], error) {
	m := req.Msg
	pr, _, err := h.S.authorize(ctx, h.S.DB.Pool, m.GetOrganizationId(), authz.ActionUseSandboxes)
	if err != nil {
		return nil, err
	}
	if m.GetSdpOffer() == "" || len(m.GetSdpOffer()) > 64<<10 {
		return nil, errInvalidf("invalid sdp_offer: must be 1 byte to 64 KiB")
	}
	sb, conn, err := h.liveSandbox(ctx, m.GetOrganizationId(), m.GetSandboxId(), ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	if err != nil {
		return nil, h.S.internal(ctx, "open session", err)
	}
	now := h.S.now()
	h.sweepSessions(now)
	sess := &session{id: newID(), orgID: sb.OrgID, sandboxID: sb.ID, deviceID: sb.DeviceID, gen: conn.Generation, principal: pr, expireAt: now.Add(sessionLifetime)}
	// Register the session as pending first, so a revocation processed
	// before the answer ends it as well.
	h.mu.Lock()
	h.sessions[sess.id] = sess
	h.mu.Unlock()
	fail := func(err error) (*connect.Response[ownsandv1.OpenSessionResponse], error) {
		h.mu.Lock()
		delete(h.sessions, sess.id)
		h.mu.Unlock()
		return nil, err
	}
	// Authorize again after registering, as the contract requires.
	if _, _, err := h.S.authorize(ctx, h.S.DB.Pool, m.GetOrganizationId(), authz.ActionUseSandboxes); err != nil {
		return fail(err)
	}
	key := conn.DeviceID + "/" + sess.id
	wait := h.signals.Register(key)
	defer h.signals.Cancel(key)
	if err := conn.Send(ctx, &ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_Signaling{Signaling: &ownsandv1.SignalingMessage{
		Message: &ownsandv1.SignalingMessage_Offer{Offer: &ownsandv1.SignalingOffer{
			SessionId: sess.id, SandboxId: sb.ID, Principal: pr.Proto(), ExpireTime: ts(sess.expireAt), ExpiresIn: durationpb.New(sessionLifetime), Sdp: m.GetSdpOffer(),
		}},
	}}}); err != nil {
		return fail(errUnavailable("the agent channel closed"))
	}
	timer := time.NewTimer(h.Timing.Signaling)
	defer timer.Stop()
	select {
	case ans := <-wait:
		if e := ans.GetError(); e != nil {
			return fail(errorFromDetail(e))
		}
		h.mu.Lock()
		_, alive := h.sessions[sess.id]
		h.mu.Unlock()
		if !alive {
			return fail(errUnavailable("the session ended before it opened"))
		}
		h.S.Logger.InfoContext(ctx, "data_path_session_opened", slog.String("session_id", sess.id), slog.String("sandbox_id", sb.ID),
			slog.String("transport", ownsandv1.DataPathTransport_DATA_PATH_TRANSPORT_PEER_TO_PEER.String()))
		return connect.NewResponse(&ownsandv1.OpenSessionResponse{SessionId: sess.id, SdpAnswer: ans.GetSdp(), ExpireTime: ts(sess.expireAt),
			ExpiresIn: durationpb.New(sess.expireAt.Sub(h.S.now()))}), nil
	case <-timer.C:
		return fail(errUnavailable("the agent did not answer the offer"))
	case <-conn.Done():
		return fail(errUnavailable("the agent channel closed"))
	case <-ctx.Done():
		return fail(ctx.Err())
	}
}

// errorFromDetail turns an ErrorDetail into a Connect error with the same
// code and the session end reason and retry delay in its detail.
func errorFromDetail(d *ownsandv1.ErrorDetail) error {
	code := connect.Code(d.GetCode())
	if code < connect.CodeCanceled || code > connect.CodeUnauthenticated {
		code = connect.CodeInternal
	}
	e := connect.NewError(code, errors.New(d.GetMessage()))
	if detail, err := connect.NewErrorDetail(d); err == nil {
		e.AddDetail(detail)
	}
	return e
}

func (h DataPathHandler) AuthorizeRelay(ctx context.Context, req *connect.Request[ownsandv1.AuthorizeRelayRequest]) (*connect.Response[ownsandv1.AuthorizeRelayResponse], error) {
	m := req.Msg
	pr, _, err := h.S.authorize(ctx, h.S.DB.Pool, m.GetOrganizationId(), authz.ActionUseSandboxes)
	if err != nil {
		return nil, err
	}
	if ok, retry := h.limiter.Allow(fmt.Sprintf("%d/%s", pr.Kind, pr.ID), 1); !ok {
		return nil, errRateLimited(retry)
	}
	if err := validateID("authorization_id", m.GetAuthorizationId()); err != nil {
		return nil, err
	}
	if err := validateID("operation_id", m.GetOperationId()); err != nil {
		return nil, err
	}
	// The agent compares exec IDs as raw text, so the grant of an exec must
	// carry the lowercase spelling that its requests are required to use.
	switch m.GetOperationKind() {
	case ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC, ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_ATTACH_EXEC:
		if err := validate.ExecID("operation_id", m.GetOperationId()); err != nil {
			return nil, errInvalid(err)
		}
	}
	allowed := []ownsandv1.SandboxState{ownsandv1.SandboxState_SANDBOX_STATE_RUNNING}
	switch m.GetOperationKind() {
	case ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_ATTACH_EXEC:
		if len(m.GetRequestDigest()) != 0 {
			return nil, errInvalidf("invalid request_digest: must be absent for ATTACH_EXEC")
		}
		allowed = append(allowed, ownsandv1.SandboxState_SANDBOX_STATE_PAUSED)
	case ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC:
		allowed = append(allowed, ownsandv1.SandboxState_SANDBOX_STATE_PAUSED)
		fallthrough
	case ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_READ_FILE, ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_WRITE_FILE:
		if len(m.GetRequestDigest()) != relaybinding.DigestSize {
			return nil, errInvalidf("invalid request_digest: must be %d bytes", relaybinding.DigestSize)
		}
	default:
		return nil, errInvalidf("invalid operation_kind")
	}
	if m.ReplacesGrantId != nil && !uuidv7.Valid(m.GetReplacesGrantId()) {
		return nil, errInvalidf("invalid replaces_grant_id")
	}
	sb, conn, err := h.liveSandbox(ctx, m.GetOrganizationId(), m.GetSandboxId(), allowed...)
	if err != nil {
		return nil, h.S.internal(ctx, "authorize relay", err)
	}
	g, existing, err := h.Grants.Reserve(grants.Request{
		AuthorizationID: m.GetAuthorizationId(), OrgID: sb.OrgID, SandboxID: sb.ID, DeviceID: sb.DeviceID, ChannelGen: conn.Generation,
		Principal: pr, Kind: m.GetOperationKind(), OperationID: m.GetOperationId(), Digest: m.GetRequestDigest(), ReplacesID: m.GetReplacesGrantId(),
	}, newID)
	switch {
	case errors.Is(err, grants.ErrLimit):
		return nil, errRateLimited(time.Second)
	case errors.Is(err, grants.ErrReuse):
		return nil, errInvalidf("invalid authorization_id: it was used with another request")
	case errors.Is(err, grants.ErrReplaceActive):
		return nil, connect.NewError(connect.CodeAborted, errors.New("the grant already has a pending replacement"))
	case err != nil:
		return nil, h.S.internal(ctx, "authorize relay", err)
	}
	// The caller's authorization is checked again after the grant is
	// registered, so a revocation in between reaches the pending grant.
	if _, _, err := h.S.authorize(ctx, h.S.DB.Pool, m.GetOrganizationId(), authz.ActionUseSandboxes); err != nil {
		if !existing {
			h.Grants.RemoveMatching(func(x *grants.Grant) bool { return x.ID == g.ID })
		}
		return nil, err
	}
	if !existing {
		now := h.S.now()
		msg := &ownsandv1.RelayGrant{
			GrantId: g.ID, SandboxId: g.SandboxID, OperationKind: g.Kind, OperationId: g.OperationID, RequestDigest: g.Digest,
			Principal: pr.Proto(), ExpireTime: ts(g.ExpireAt), StreamDeadline: ts(g.StreamDeadline),
			ExpiresIn: durationpb.New(g.ExpireAt.Sub(now)), StreamDeadlineIn: durationpb.New(g.StreamDeadlineIn),
		}
		if g.Replaces != "" {
			msg.ReplacesGrantId = &g.Replaces
		}
		if err := conn.Send(ctx, &ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_RelayGrant{RelayGrant: msg}}); err != nil {
			return nil, errUnavailable("the agent channel closed")
		}
	}
	timer := time.NewTimer(h.Timing.GrantAck)
	defer timer.Stop()
	select {
	case <-g.Done():
	case <-timer.C:
		// The pending grant stays until it stops being admittable: a late
		// acknowledgement installs it and a retry returns it.
		return nil, errUnavailable("the agent did not acknowledge the relay grant")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if g.Rejected() {
		return nil, errRateLimited(time.Second)
	}
	now := h.S.now()
	h.S.Logger.InfoContext(ctx, "relay_grant_issued", slog.String("grant_id", g.ID), slog.String("sandbox_id", g.SandboxID),
		slog.String("operation_kind", g.Kind.String()), slog.Bool("retry", existing), slog.Bool("replacement", m.ReplacesGrantId != nil),
		slog.String("transport", ownsandv1.DataPathTransport_DATA_PATH_TRANSPORT_RELAY.String()))
	return connect.NewResponse(&ownsandv1.AuthorizeRelayResponse{GrantId: g.ID, ExpireTime: ts(g.ExpireAt), ExpiresIn: durationpb.New(g.ExpireAt.Sub(now))}), nil
}
