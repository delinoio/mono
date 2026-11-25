// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/delinoio/mono/go/ownsand/relaybinding"
	"github.com/delinoio/mono/go/ownsand/validate"
	"github.com/delinoio/mono/go/uuidv7"
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
	sessionLifetime = time.Hour
	authorizeRate   = 10 // AuthorizeRelay calls per second per principal
)

// session is an open peer-to-peer session that the lease holder signaled.
type session struct {
	id, orgID, sandboxID, deviceID string
	gen                            uint64
	principal                      auth.Principal
	membership                     auth.MembershipSnapshot
	expireAt                       time.Time
}

// revocationTarget is what a pending revocation ends, for logs.
type revocationTarget string

const (
	revocationTargetSession    revocationTarget = "session"
	revocationTargetRelayGrant revocationTarget = "relay_grant"
)

// abandonRevocationTimeout bounds the enqueue of the revocation that
// OpenSession sends for a session whose caller canceled or whose signaling
// wait timed out after the offer was sent.
const abandonRevocationTimeout = 2 * time.Second

type revocationDelivery uint8

const (
	revocationSending revocationDelivery = iota
	revocationDeferred
	revocationQueued
)

// revocation tracks delivery and acknowledgement on the original channel.
type revocation struct {
	deviceID string
	gen      uint64
	target   revocationTarget
	envelope *ownsandv1.AgentEnvelope
	delivery revocationDelivery
	// expireAt is when the revoked session reaches its absolute expiry or the
	// revoked grant stops being revocable. The agent never needs the
	// revocation after that, so the sweep deletes it even when no
	// acknowledgement arrived.
	expireAt time.Time
}

func (r revocation) logQueued(ctx context.Context, logger *slog.Logger) {
	message := r.envelope.GetSessionRevocation()
	logger.InfoContext(ctx, "session_revocation_sent", slog.String("device_id", r.deviceID),
		slog.String("revocation_id", message.GetRevocationId()), slog.String("session_id", message.GetSessionId()),
		slog.String("grant_id", message.GetRelayGrantId()))
}

func (r revocation) logExpired(ctx context.Context, logger *slog.Logger) {
	logger.InfoContext(ctx, "session_revocation_expired",
		slog.String("revocation_id", r.envelope.GetSessionRevocation().GetRevocationId()),
		slog.String("device_id", r.deviceID), slog.String("target", string(r.target)), slog.Time("expire_time", r.expireAt))
}

// removedDeviceKey marks a channel that is waiting for device clean-up.
type removedDeviceKey struct{}

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
	// grantSender replaces the agent channel enqueue of a relay grant; nil
	// uses hub.Conn.Send. Tests set it to inject an enqueue failure on a live
	// channel, which a running channel produces only when the outbound queue
	// is full and the caller's context ends.
	grantSender func(ctx context.Context, conn *hub.Conn, env *ownsandv1.AgentEnvelope) error
	// grantWaitObserver is called with the grant ID when a retried call finds
	// an existing reservation and is about to wait for its outcome; nil does
	// nothing. Tests use it to know that a concurrent call is waiting.
	grantWaitObserver func(grantID string)
	// afterWriteResponseDequeued is a test hook that WriteFile calls with the
	// call context after dequeuing an agent result or terminal frame and
	// before checking the deadline. It is nil in production.
	afterWriteResponseDequeued atomic.Pointer[func(cctx context.Context)]
}

// SetGrantWaitObserver sets a callback that AuthorizeRelay calls just before
// a call for an already reserved grant waits for the outcome (tests only); nil
// removes it. It never changes how the call behaves.
func (p *DataPlane) SetGrantWaitObserver(fn func(grantID string)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.grantWaitObserver = fn
}

func (p *DataPlane) observeGrantWait(grantID string) {
	p.mu.Lock()
	fn := p.grantWaitObserver
	p.mu.Unlock()
	if fn != nil {
		fn(grantID)
	}
}

// SetGrantSender replaces how AuthorizeRelay enqueues a relay grant for the
// agent (tests only); nil restores hub.Conn.Send.
func (p *DataPlane) SetGrantSender(fn func(ctx context.Context, conn *hub.Conn, env *ownsandv1.AgentEnvelope) error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.grantSender = fn
}

func (p *DataPlane) sendGrant(ctx context.Context, conn *hub.Conn, env *ownsandv1.AgentEnvelope) error {
	p.mu.Lock()
	fn := p.grantSender
	p.mu.Unlock()
	if fn != nil {
		return fn(ctx, conn, env)
	}
	return conn.Send(ctx, env)
}

// RelayTiming holds the relay stream durations; tests shorten them.
type RelayTiming struct {
	// Rotation ends a server stream before 14 minutes.
	Rotation time.Duration
	// Keepalive is the caller-side keepalive interval (at most 60 s).
	Keepalive time.Duration
	// Acceptance is the wait for the agent's accepted notice (30 s).
	Acceptance time.Duration
	// RequestDeadline bounds the request upload and forwarding the request
	// frame (5 min), and the whole WriteFile call. It counts from the
	// stream_id allocation at the relay request gate, before Connect reads
	// the request body.
	RequestDeadline time.Duration
	// GrantAck bounds the wait for a relay grant acknowledgement (10 s) and
	// Signaling the wait for an SDP answer (30 s).
	GrantAck  time.Duration
	Signaling time.Duration
	// ConnWait bounds the wait for the agent relay connection of the grant's
	// device at relay admission (10 s). Zero means the default.
	ConnWait time.Duration
}

// defaultRelayConnWait is the contract wait for the agent relay connection.
const defaultRelayConnWait = 10 * time.Second

// connWait returns the wait for the agent relay connection; a zero ConnWait
// keeps the contract value.
func (t RelayTiming) connWait() time.Duration {
	if t.ConnWait <= 0 {
		return defaultRelayConnWait
	}
	return t.ConnWait
}

// DefaultRelayTiming returns the contract values.
func DefaultRelayTiming() RelayTiming {
	return RelayTiming{Rotation: 14*time.Minute - 10*time.Second, Keepalive: 50 * time.Second, Acceptance: 30 * time.Second, RequestDeadline: 5 * time.Minute,
		GrantAck: 10 * time.Second, Signaling: 30 * time.Second, ConnWait: defaultRelayConnWait}
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
		id := m.RevocationAck.GetRevocationId()
		if r, ok := p.revocations[id]; ok && r.deviceID == conn.DeviceID && r.gen == conn.Generation {
			delete(p.revocations, id)
		}
		p.mu.Unlock()
	default:
		return false
	}
	return true
}

// HandleEvent implements DataPathHooks: it revokes the sessions and relay
// grants that a revocation event matches.
func (p *DataPlane) HandleEvent(ctx context.Context, e events.Event) {
	matchPrincipal := func(orgID string, pr auth.Principal, membership auth.MembershipSnapshot) bool {
		switch e.Kind {
		case events.KindAPIKeyRevoked:
			return pr.IsAPIKey() && pr.ID == e.APIKeyID
		case events.KindMembershipEnded:
			return pr.IsUser() && pr.ID == e.UserID && orgID == e.OrganizationID && e.ID > membership.DeliveryID
		case events.KindOrganizationDeleted:
			return orgID == e.OrganizationID
		}
		return false
	}
	matchSandbox := func(sandboxID, deviceID string) bool {
		return (e.Kind == events.KindSandboxDeleted && sandboxID == e.SandboxID) || (e.Kind == events.KindDeviceRemoved && deviceID == e.DeviceID)
	}
	revoked := p.Grants.RemoveMatching(func(g *grants.Grant) bool {
		return matchPrincipal(g.OrgID, g.Principal, g.Membership) || matchSandbox(g.SandboxID, g.DeviceID)
	})
	p.mu.Lock()
	if e.Kind == events.KindDeviceRemoved {
		if conn := p.S.Hub.Get(e.DeviceID); conn != nil {
			conn.SetValue(removedDeviceKey{}, true)
		}
		for id, r := range p.revocations {
			if r.deviceID == e.DeviceID {
				delete(p.revocations, id)
			}
		}
	}
	var sessions []*session
	for id, s := range p.sessions {
		if matchPrincipal(s.orgID, s.principal, s.membership) || matchSandbox(s.sandboxID, s.deviceID) {
			sessions = append(sessions, s)
			delete(p.sessions, id)
		}
	}
	p.mu.Unlock()
	// Device clean-up ends the sessions and streams of the removed device.
	if e.Kind == events.KindDeviceRemoved {
		return
	}
	for _, g := range revoked {
		p.sendRevocation(ctx, g.DeviceID, g.ChannelGen, revocationTargetRelayGrant, g.RevocableUntil(), &ownsandv1.SessionRevocation{Target: &ownsandv1.SessionRevocation_RelayGrantId{RelayGrantId: g.ID}})
	}
	for _, s := range sessions {
		p.sendRevocation(ctx, s.deviceID, s.gen, revocationTargetSession, s.expireAt, &ownsandv1.SessionRevocation{Target: &ownsandv1.SessionRevocation_SessionId{SessionId: s.id}})
	}
}

// sendRevocation makes the initial bounded enqueue. The pending record owns
// delivery after a failed enqueue on a channel that stays live.
func (p *DataPlane) sendRevocation(ctx context.Context, deviceID string, gen uint64, target revocationTarget, expireAt time.Time, message *ownsandv1.SessionRevocation) {
	p.sweepRevocations(ctx, p.S.now(), false)
	p.mu.Lock()
	conn := p.S.Hub.Get(deviceID)
	if conn == nil || conn.Generation != gen || conn.Value(removedDeviceKey{}) != nil {
		p.mu.Unlock()
		return
	}
	id := newID()
	message.RevocationId = id
	r := revocation{
		deviceID: deviceID, gen: gen, target: target, expireAt: expireAt, delivery: revocationSending,
		envelope: &ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_SessionRevocation{SessionRevocation: message}},
	}
	if p.S.now().Before(expireAt) {
		p.revocations[id] = r
	}
	p.mu.Unlock()

	err := conn.Send(ctx, r.envelope)
	p.mu.Lock()
	_, pending := p.revocations[id]
	deferred, expired := false, false
	if pending {
		switch {
		case err == nil:
			r.delivery = revocationQueued
			p.revocations[id] = r
		case p.S.Hub.Get(deviceID) != conn:
			delete(p.revocations, id)
		case !p.S.now().Before(expireAt):
			delete(p.revocations, id)
			expired = true
		default:
			r.delivery = revocationDeferred
			p.revocations[id] = r
			deferred = true
		}
	}
	p.mu.Unlock()

	if err == nil {
		r.logQueued(ctx, p.S.Logger)
	} else if expired {
		r.logExpired(ctx, p.S.Logger)
	} else if deferred {
		p.S.Logger.InfoContext(ctx, "session_revocation_enqueue_deferred", slog.String("device_id", deviceID),
			slog.String("revocation_id", id), slog.String("target", string(target)))
	}
}

// PendingRevocations returns the number of unacknowledged revocations.
func (p *DataPlane) PendingRevocations() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.revocations)
}

// SweepExpired deletes expired revocations and retries deferred enqueues. It
// implements DataPathHooks and runs periodically, including on idle channels.
func (p *DataPlane) SweepExpired(ctx context.Context) {
	p.sweepRevocations(ctx, p.S.now(), true)
}

// RPC paths only need expiry. The periodic sweep also tries each deferred
// enqueue while holding mu, so acknowledgement and terminal cleanup cannot
// race the retry. TrySend does not wait for space in the queue.
func (p *DataPlane) sweepRevocations(ctx context.Context, now time.Time, retry bool) {
	var expired, queued []revocation
	p.mu.Lock()
	for id, r := range p.revocations {
		if !now.Before(r.expireAt) {
			expired = append(expired, r)
			delete(p.revocations, id)
			continue
		}
		if !retry || r.delivery != revocationDeferred {
			continue
		}
		sent, err := p.S.Hub.TrySend(r.deviceID, r.gen, r.envelope)
		if err != nil {
			delete(p.revocations, id)
		} else if sent {
			r.delivery = revocationQueued
			p.revocations[id] = r
			queued = append(queued, r)
		}
	}
	p.mu.Unlock()
	for _, r := range expired {
		r.logExpired(ctx, p.S.Logger)
	}
	for _, r := range queued {
		r.logQueued(ctx, p.S.Logger)
	}
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
	sandboxID, err := normalizeID("sandbox_id", sandboxID)
	if err != nil {
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
	orgID, err := normalizeID("organization_id", m.GetOrganizationId())
	if err != nil {
		return nil, err
	}
	pr, membership, err := h.S.authorizeDataPath(ctx, h.S.DB.Pool, orgID)
	if err != nil {
		return nil, err
	}
	if err := h.S.checkBilling(ctx, h.S.DB.Pool, orgID, restrictWhenSuspended); err != nil {
		return nil, err
	}
	if m.GetSdpOffer() == "" || len(m.GetSdpOffer()) > 64<<10 {
		return nil, errInvalidf("invalid sdp_offer: must be 1 byte to 64 KiB")
	}
	sb, conn, err := h.liveSandbox(ctx, orgID, m.GetSandboxId(), ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	if err != nil {
		return nil, h.S.internal(ctx, "open session", err)
	}
	now := h.S.now()
	h.sweepSessions(now)
	h.sweepRevocations(ctx, now, false)
	sess := &session{id: newID(), orgID: sb.OrgID, sandboxID: sb.ID, deviceID: sb.DeviceID, gen: conn.Generation, principal: pr, membership: membership, expireAt: now.Add(sessionLifetime)}
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
	// abandon ends a session whose offer the agent has received (the send
	// succeeded) but whose caller stopped waiting: the request was canceled
	// or the agent did not answer in time. The agent may still set the
	// session up, or may have set it up already, so the server revokes it on
	// the channel generation that signaled it. The revocation does not use
	// the request context, which is canceled in the first case, and has a
	// short bound of its own, because the pending revocation record, not the
	// enqueue, owns the delivery. When a revocation event removed the session
	// first, HandleEvent already sent its revocation.
	abandon := func(cause string, err error) (*connect.Response[ownsandv1.OpenSessionResponse], error) {
		h.mu.Lock()
		_, pending := h.sessions[sess.id]
		delete(h.sessions, sess.id)
		h.mu.Unlock()
		if pending {
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), abandonRevocationTimeout)
			defer cancel()
			h.S.Logger.WarnContext(rctx, "data_path_session_abandoned", slog.String("session_id", sess.id), slog.String("sandbox_id", sb.ID),
				slog.String("device_id", sess.deviceID), slog.String("cause", cause))
			h.sendRevocation(rctx, sess.deviceID, sess.gen, revocationTargetSession, sess.expireAt, &ownsandv1.SessionRevocation{Target: &ownsandv1.SessionRevocation_SessionId{SessionId: sess.id}})
		}
		return nil, err
	}
	// Authorize again after registering, as the contract requires.
	if err := h.S.reauthorizeDataPath(ctx, h.S.DB.Pool, orgID, sess.membership.ID); err != nil {
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
		// The select can receive an answer even when the request has ended.
		if err := ctx.Err(); err != nil {
			return abandon("request_canceled", err)
		}
		if !alive {
			return fail(errUnavailable("the session ended before it opened"))
		}
		h.S.Logger.InfoContext(ctx, "data_path_session_opened", slog.String("session_id", sess.id), slog.String("sandbox_id", sb.ID),
			slog.String("transport", ownsandv1.DataPathTransport_DATA_PATH_TRANSPORT_PEER_TO_PEER.String()))
		return connect.NewResponse(&ownsandv1.OpenSessionResponse{SessionId: sess.id, SdpAnswer: ans.GetSdp(), ExpireTime: ts(sess.expireAt),
			ExpiresIn: durationpb.New(sess.expireAt.Sub(h.S.now()))}), nil
	case <-timer.C:
		return abandon("signaling_timeout", errUnavailable("the agent did not answer the offer"))
	case <-conn.Done():
		return fail(errUnavailable("the agent channel closed"))
	case <-ctx.Done():
		return abandon("request_canceled", ctx.Err())
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

// enqueueErrorCategory names why queueing a message for the agent failed, for
// logs: the channel closed, or the caller's context ended before the queue
// accepted it.
func enqueueErrorCategory(err error) string {
	switch {
	case errors.Is(err, hub.ErrChannelClosed):
		return "channel_closed"
	case errors.Is(err, context.Canceled):
		return "context_canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "context_deadline_exceeded"
	default:
		return "other"
	}
}

func (h DataPathHandler) AuthorizeRelay(ctx context.Context, req *connect.Request[ownsandv1.AuthorizeRelayRequest]) (*connect.Response[ownsandv1.AuthorizeRelayResponse], error) {
	m := req.Msg
	orgID, err := normalizeID("organization_id", m.GetOrganizationId())
	if err != nil {
		return nil, err
	}
	pr, membership, err := h.S.authorizeDataPath(ctx, h.S.DB.Pool, orgID)
	if err != nil {
		return nil, err
	}
	if err := h.S.checkBilling(ctx, h.S.DB.Pool, orgID, restrictWhenSuspended); err != nil {
		return nil, err
	}
	if ok, retry := h.limiter.Allow(fmt.Sprintf("%d/%s", pr.Kind, pr.ID), 1); !ok {
		return nil, errRateLimited(retry)
	}
	// The grant registry and the agent compare identifiers as text, so the
	// grant carries the lowercase spelling of every identifier.
	authorizationID, err := normalizeID("authorization_id", m.GetAuthorizationId())
	if err != nil {
		return nil, err
	}
	operationID, err := normalizeID("operation_id", m.GetOperationId())
	if err != nil {
		return nil, err
	}
	// The agent compares exec IDs as raw text and the request is required to
	// use the lowercase spelling, so an exec grant accepts only that
	// spelling; the other operation IDs accept either hex case and are
	// normalized above.
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
	replacesID := ""
	if m.ReplacesGrantId != nil {
		if !uuidv7.Valid(m.GetReplacesGrantId()) {
			return nil, errInvalidf("invalid replaces_grant_id")
		}
		replacesID = canonicalID(m.GetReplacesGrantId())
	}
	sb, conn, err := h.liveSandbox(ctx, orgID, m.GetSandboxId(), allowed...)
	if err != nil {
		return nil, h.S.internal(ctx, "authorize relay", err)
	}
	g, existing, err := h.Grants.Reserve(grants.Request{
		AuthorizationID: authorizationID, OrgID: sb.OrgID, SandboxID: sb.ID, DeviceID: sb.DeviceID, ChannelGen: conn.Generation,
		Principal: pr, Membership: membership, Kind: m.GetOperationKind(), OperationID: operationID, Digest: m.GetRequestDigest(), ReplacesID: replacesID,
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
	if err := h.S.reauthorizeDataPath(ctx, h.S.DB.Pool, orgID, g.Membership.ID); err != nil {
		if !existing {
			h.Grants.RemoveMatching(func(x *grants.Grant) bool { return x.ID == g.ID })
		}
		return nil, err
	}
	if !existing {
		now := h.S.now()
		// Snapshot the predecessor once under the registry mutex: the registry
		// clears the field on release or ack while the writer still serializes
		// the queued message.
		replaces := h.Grants.ReplacesOf(g)
		msg := &ownsandv1.RelayGrant{
			GrantId: g.ID, SandboxId: g.SandboxID, OperationKind: g.Kind, OperationId: g.OperationID, RequestDigest: g.Digest,
			Principal: pr.Proto(), ExpireTime: ts(g.ExpireAt), StreamDeadline: ts(g.StreamDeadline),
			ExpiresIn: durationpb.New(g.ExpireAt.Sub(now)), StreamDeadlineIn: durationpb.New(g.StreamDeadlineIn),
		}
		if replaces != "" {
			msg.ReplacesGrantId = &replaces
		}
		if err := h.sendGrant(ctx, conn, &ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_RelayGrant{RelayGrant: msg}}); err != nil {
			// The grant never entered the outbound queue, so the agent cannot
			// acknowledge it. Release the exact reservation (and the
			// predecessor's pending-successor link) before the caller sees the
			// failure; a grant that was queued is never removed here.
			removed := h.Grants.RemoveUndelivered(g.ID)
			h.S.Logger.WarnContext(ctx, "relay_grant_enqueue_failed", slog.String("grant_id", g.ID), slog.String("sandbox_id", g.SandboxID),
				slog.String("operation_kind", g.Kind.String()), slog.Bool("replacement", replaces != ""),
				slog.String("error_category", enqueueErrorCategory(err)), slog.Bool("reservation_removed", removed))
			return nil, errUnavailable("the agent channel closed")
		}
	}
	if existing {
		h.observeGrantWait(g.ID)
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
	switch g.Outcome() {
	case grants.OutcomeAccepted:
	case grants.OutcomeRejectedLimit:
		return nil, errRateLimited(time.Second)
	case grants.OutcomeDeliveryFailed:
		// Another call of the same authorization_id failed to queue the grant;
		// the reservation is gone, so a retry sends it again.
		return nil, errUnavailable("the relay grant could not be delivered to the agent")
	default:
		// The pending grant was removed without an acknowledgement (the agent
		// channel closed, a revocation, a release or the expiry): not a
		// capacity rejection, so the caller retries without the grant-limit delay.
		return nil, errUnavailable("the agent channel closed before it acknowledged the relay grant")
	}
	now := h.S.now()
	h.S.Logger.InfoContext(ctx, "relay_grant_issued", slog.String("grant_id", g.ID), slog.String("sandbox_id", g.SandboxID),
		slog.String("operation_kind", g.Kind.String()), slog.Bool("retry", existing), slog.Bool("replacement", m.ReplacesGrantId != nil),
		slog.String("transport", ownsandv1.DataPathTransport_DATA_PATH_TRANSPORT_RELAY.String()))
	return connect.NewResponse(&ownsandv1.AuthorizeRelayResponse{GrantId: g.ID, ExpireTime: ts(g.ExpireAt), ExpiresIn: durationpb.New(g.ExpireAt.Sub(now))}), nil
}
