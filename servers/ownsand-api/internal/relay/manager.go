// SPDX-License-Identifier: FSL-1.1-ALv2

// Package relay implements the data path on the lease holder: peer-to-peer
// session signaling (DataPathService), relay grants with their limits,
// idempotency and replacement, revocations, the agent relay connection
// registry (RelayAgentService.Connect) and the forwarding of relay RPCs
// (RelayService). Contract: docs/servers-ownsand-api-foundation.md
// sections "Data path signaling and relay", "Relay authentication" and
// "Relay".
package relay

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/agent"
	"github.com/delinoio/mono/servers/ownsand-api/internal/api"
	"github.com/delinoio/mono/servers/ownsand-api/internal/auth"
	"github.com/delinoio/mono/servers/ownsand-api/internal/ratelimit"
	"github.com/delinoio/mono/servers/ownsand-api/internal/store"
)

// Limits are the data path limits. Defaults returns the contract values and
// the values this implementation chose; tests shorten the timers.
type Limits struct {
	GrantsPerDevice    int
	GrantsPerPrincipal int
	AuthorizePerSecond float64
	RetryDelay         time.Duration
	GrantAckTimeout    time.Duration
	SignalTimeout      time.Duration
	GrantExpiry        time.Duration
	StreamDeadline     time.Duration
	AdmissionTolerance time.Duration
	DeliveryMargin     time.Duration
	SessionLifetime    time.Duration

	ConnectionWait        time.Duration // wait for an agent relay connection
	ConnectionKeepalive   time.Duration // send a keepalive after this idle time
	ConnectionReceiveIdle time.Duration // close after this long without a byte
	StreamKeepalive       time.Duration // keepalive event toward the caller
	StreamRotation        time.Duration // end an accepted stream with OK
	RequestDeadline       time.Duration // forward the request frame within
	AcceptTimeout         time.Duration // accepted event after the request
	WriteFileDeadline     time.Duration
	StalledReader         time.Duration // caller has read nothing for
	StreamBuffer          int64         // per-stream buffer toward the caller
	ConnectionBuffer      int64         // sum of stream buffers per connection
	StreamsPerGrant       int
	MaxPendingFrames      int // queued frames toward one agent connection
}

// Defaults returns the production limits.
func Defaults() Limits {
	return Limits{
		GrantsPerDevice: 256, GrantsPerPrincipal: 32, AuthorizePerSecond: 10, RetryDelay: time.Second,
		GrantAckTimeout: 10 * time.Second, SignalTimeout: 15 * time.Second,
		GrantExpiry: 15 * time.Minute, StreamDeadline: 30 * time.Minute, AdmissionTolerance: 30 * time.Second,
		DeliveryMargin: 60 * time.Second, SessionLifetime: time.Hour,
		ConnectionWait: 10 * time.Second, ConnectionKeepalive: 30 * time.Second, ConnectionReceiveIdle: 90 * time.Second,
		StreamKeepalive: 60 * time.Second, StreamRotation: 14 * time.Minute, RequestDeadline: 5 * time.Minute,
		AcceptTimeout: 30 * time.Second, WriteFileDeadline: 5 * time.Minute, StalledReader: 60 * time.Second,
		StreamBuffer: 2 << 20, ConnectionBuffer: 64 << 20, StreamsPerGrant: 4, MaxPendingFrames: 1024,
	}
}

// Manager holds the data path state of the lease holder.
type Manager struct {
	Hub        *agent.Hub
	Store      *store.Store
	API        *api.Server
	Logger     *slog.Logger
	Limits     Limits
	IceServers []*ownsandv1.IceServer
	Now        func() time.Time

	authorizeRate *ratelimit.Limiter
	failRate      *ratelimit.Limiter

	mu          sync.Mutex
	sessions    map[string]*session
	grants      map[string]*grant
	byAuth      map[string]*grant // principal key + authorization_id
	pending     map[*pendingAuth]struct{}
	revocations map[string]*revocation
	signals     map[string]chan *ownsandv1.SignalingAnswer
	conns       map[string]*agentConn
	connWaiters map[string][]chan struct{}
}

// NewManager returns a manager and registers it as the hub extension.
func NewManager(hub *agent.Hub, st *store.Store, apiServer *api.Server, logger *slog.Logger, limits Limits, ice []*ownsandv1.IceServer) *Manager {
	m := &Manager{
		Hub: hub, Store: st, API: apiServer, Logger: logger, Limits: limits, IceServers: ice,
		authorizeRate: ratelimit.New(limits.AuthorizePerSecond, int(limits.AuthorizePerSecond), nil),
		failRate:      ratelimit.New(1, 10, nil),
		sessions:      map[string]*session{}, grants: map[string]*grant{}, byAuth: map[string]*grant{},
		pending: map[*pendingAuth]struct{}{}, revocations: map[string]*revocation{},
		signals: map[string]chan *ownsandv1.SignalingAnswer{}, conns: map[string]*agentConn{}, connWaiters: map[string][]chan struct{}{},
	}
	hub.Extension = m
	return m
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func principalKey(p auth.Principal) string { return p.Kind.String() + "/" + p.ID }

// session is an open peer-to-peer session.
type session struct {
	id        string
	principal auth.Principal
	org       string
	sandbox   string
	device    string
	gen       uint64
	expiresAt time.Time
	revoked   bool
}

type grantState int

const (
	grantPending grantState = iota
	grantInstalled
)

// grant is a relay grant. It is never persisted.
type grant struct {
	id              string
	authorizationID string
	principal       auth.Principal
	org, sandbox    string
	device          string
	gen             uint64
	kind            ownsandv1.RelayOperationKind
	opID            string
	digest          []byte
	expiresAt       time.Time
	streamDeadline  time.Time
	deadlineIn      time.Duration
	ackAt           time.Time
	state           grantState
	revoked         bool
	replaces        *grant
	successor       *grant
	decided         chan struct{} // closed when the agent accepted or rejected
	rejected        bool
	streams         int
}

// liveAt reports whether the grant is still known, bound to the device's
// current agent channel and not past the later of its stream deadline plus
// the delivery margin and its acknowledgement plus the stream deadline
// duration that the agent started on receipt.
func (m *Manager) liveAt(g *grant, now time.Time) bool {
	if m.grants[g.id] != g {
		return false
	}
	c, ok := m.Hub.Current(g.device)
	if !ok || c.Generation != g.gen {
		return false
	}
	until := g.streamDeadline.Add(m.Limits.DeliveryMargin)
	if !g.ackAt.IsZero() && g.ackAt.Add(g.deadlineIn).After(until) {
		until = g.ackAt.Add(g.deadlineIn)
	}
	return now.Before(until)
}

// revocableAt applies the grant terms: live and neither revoked nor
// released. Revocable grants count against the limits.
func (m *Manager) revocableAt(g *grant, now time.Time) bool {
	return !g.revoked && m.liveAt(g, now)
}

// admittableAt: revocable, installed and before its expiry plus the
// admission tolerance.
func (m *Manager) admittableAt(g *grant, now time.Time) bool {
	return g.state == grantInstalled && m.revocableAt(g, now) && now.Before(g.expiresAt.Add(m.Limits.AdmissionTolerance))
}

// pendingAuth is an OpenSession or AuthorizeRelay call between its
// registration and its result, so that a revocation in between ends it.
type pendingAuth struct {
	principal auth.Principal
	org       string
	sandbox   string
	device    string
	revoked   bool
}

func (m *Manager) registerPending(p auth.Principal, org, sandbox string) *pendingAuth {
	pa := &pendingAuth{principal: p, org: org, sandbox: sandbox}
	m.mu.Lock()
	m.pending[pa] = struct{}{}
	m.mu.Unlock()
	return pa
}

func (m *Manager) finishPending(pa *pendingAuth) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.pending, pa)
	return pa.revoked
}

// revocation is a session or grant revocation awaiting the agent's ack.
type revocation struct {
	id        string
	device    string
	gen       uint64
	sessionID string
	grantID   string
}

var (
	errRevoked       = connect.NewError(connect.CodePermissionDenied, errors.New("the caller's authorization was revoked"))
	errNoLease       = connect.NewError(connect.CodeUnavailable, errors.New("this instance does not serve the data path now; retry"))
	errNoChannel     = connect.NewError(connect.CodeUnavailable, errors.New("the sandbox's device has no live agent channel"))
	errGrantNotAcked = connect.NewError(connect.CodeUnavailable, errors.New("the device agent did not acknowledge the relay grant"))
)

func retryError(msg string, delay time.Duration) error {
	err := connect.NewError(connect.CodeResourceExhausted, errors.New(msg))
	if d, e := connect.NewErrorDetail(&ownsandv1.RetryDetail{RetryDelay: durationpb.New(delay)}); e == nil {
		err.AddDetail(d)
	}
	return err
}

func sessionEndError(reason ownsandv1.SessionEndReason, msg string) error {
	err := connect.NewError(connect.CodeUnavailable, errors.New(msg))
	if d, e := connect.NewErrorDetail(&ownsandv1.SessionEndDetail{Reason: reason}); e == nil {
		err.AddDetail(d)
	}
	return err
}

// errGrantInvalid is the error of an unknown, non-admittable or revoked grant.
func errGrantInvalid() error {
	return sessionEndError(ownsandv1.SessionEndReason_SESSION_END_REASON_GRANT_INVALID, "the relay grant is not valid; call AuthorizeRelay again")
}

// HandleEnvelope implements agent.Extension.
func (m *Manager) HandleEnvelope(c *agent.Conn, env *ownsandv1.AgentEnvelope) bool {
	switch msg := env.GetMessage().(type) {
	case *ownsandv1.AgentEnvelope_SignalingAnswer:
		m.mu.Lock()
		ch := m.signals[msg.SignalingAnswer.GetSessionId()]
		delete(m.signals, msg.SignalingAnswer.GetSessionId())
		m.mu.Unlock()
		if ch != nil {
			ch <- msg.SignalingAnswer
		}
	case *ownsandv1.AgentEnvelope_RevocationAck:
		m.mu.Lock()
		delete(m.revocations, msg.RevocationAck.GetRevocationId())
		m.mu.Unlock()
	case *ownsandv1.AgentEnvelope_RelayGrantAck:
		m.onGrantAck(c, msg.RelayGrantAck)
	case *ownsandv1.AgentEnvelope_RelayGrantRelease:
		m.mu.Lock()
		if g := m.grants[msg.RelayGrantRelease.GetGrantId()]; g != nil && g.device == c.DeviceID {
			m.removeGrantLocked(g)
		}
		m.mu.Unlock()
		m.Logger.Info("relay_grant_released", slog.String("grant_id", msg.RelayGrantRelease.GetGrantId()), slog.String("device_id", c.DeviceID))
	default:
		return false
	}
	return true
}

func (m *Manager) removeGrantLocked(g *grant) {
	if m.grants[g.id] != g {
		return
	}
	delete(m.grants, g.id)
	if m.byAuth[principalKey(g.principal)+"/"+g.authorizationID] == g {
		delete(m.byAuth, principalKey(g.principal)+"/"+g.authorizationID)
	}
	if g.replaces != nil && g.replaces.successor == g {
		g.replaces.successor = nil
	}
	if g.successor != nil && g.successor.replaces == g {
		g.successor.replaces = nil
	}
	for id, r := range m.revocations {
		if r.grantID == g.id {
			delete(m.revocations, id)
		}
	}
}

func (m *Manager) onGrantAck(c *agent.Conn, ack *ownsandv1.RelayGrantAck) {
	m.mu.Lock()
	defer m.mu.Unlock()
	g := m.grants[ack.GetGrantId()]
	if g == nil || g.device != c.DeviceID || g.gen != c.Generation {
		return
	}
	select {
	case <-g.decided:
		return
	default:
	}
	if ack.GetResult() == ownsandv1.RelayGrantResult_RELAY_GRANT_RESULT_ACCEPTED {
		g.state = grantInstalled
		g.ackAt = m.now()
		if ack.ReplacedGrantId != nil && g.replaces != nil && g.replaces.id == ack.GetReplacedGrantId() {
			m.removeGrantLocked(g.replaces)
			g.replaces = nil
		}
	} else {
		g.rejected = true
		m.removeGrantLocked(g)
	}
	close(g.decided)
}

// ChannelClosed implements agent.Extension: sessions, grants and
// revocations of the channel end with it, and so does its relay connection.
func (m *Manager) ChannelClosed(c *agent.Conn) {
	m.mu.Lock()
	for id, s := range m.sessions {
		if s.device == c.DeviceID && s.gen == c.Generation {
			delete(m.sessions, id)
		}
	}
	for _, g := range m.grants {
		if g.device == c.DeviceID && g.gen == c.Generation {
			m.removeGrantLocked(g)
		}
	}
	for id, r := range m.revocations {
		if r.device == c.DeviceID && r.gen == c.Generation {
			delete(m.revocations, id)
		}
	}
	conn := m.conns[c.DeviceID]
	m.mu.Unlock()
	if conn != nil && conn.gen == c.Generation {
		conn.close("agent channel closed")
	}
}

// ControlEvent implements agent.Extension: it revokes every matching open
// session, revocable grant and pending authorization.
func (m *Manager) ControlEvent(e store.ControlEvent) {
	match := func(p auth.Principal, org, sandbox, device string) bool {
		switch e.Kind {
		case store.ControlEventAPIKeyRevoked:
			return p.Kind == ownsandv1.PrincipalKind_PRINCIPAL_KIND_API_KEY && p.ID == e.APIKeyID
		case store.ControlEventMembershipEnded:
			return p.Kind == ownsandv1.PrincipalKind_PRINCIPAL_KIND_USER && p.ID == e.UserID && org == e.OrganizationID
		case store.ControlEventSandboxDeleteRequested:
			return sandbox == e.SandboxID
		case store.ControlEventOrganizationDeleted:
			return org == e.OrganizationID
		case store.ControlEventDeviceRemoved:
			return device == e.DeviceID
		}
		return false
	}
	now := m.now()
	var sends []*revocation
	m.mu.Lock()
	for pa := range m.pending {
		if match(pa.principal, pa.org, pa.sandbox, pa.device) {
			pa.revoked = true
		}
	}
	for _, s := range m.sessions {
		if !s.revoked && match(s.principal, s.org, s.sandbox, s.device) {
			s.revoked = true
			r := &revocation{id: store.NewID(), device: s.device, gen: s.gen, sessionID: s.id}
			m.revocations[r.id] = r
			sends = append(sends, r)
		}
	}
	for _, g := range m.grants {
		if !g.revoked && m.revocableAt(g, now) && match(g.principal, g.org, g.sandbox, g.device) {
			r := &revocation{id: store.NewID(), device: g.device, gen: g.gen, grantID: g.id}
			g.revoked = true
			m.revocations[r.id] = r
			sends = append(sends, r)
		}
	}
	m.mu.Unlock()
	for _, r := range sends {
		m.sendRevocation(r)
	}
}

func (m *Manager) sendRevocation(r *revocation) {
	c, ok := m.Hub.Current(r.device)
	if !ok || c.Generation != r.gen {
		return
	}
	rev := &ownsandv1.SessionRevocation{RevocationId: r.id}
	if r.sessionID != "" {
		rev.Target = &ownsandv1.SessionRevocation_SessionId{SessionId: r.sessionID}
	} else {
		rev.Target = &ownsandv1.SessionRevocation_GrantId{GrantId: r.grantID}
	}
	c.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_SessionRevocation{SessionRevocation: rev}})
	m.Logger.Info("data_path_revoked", slog.String("revocation_id", r.id), slog.String("device_id", r.device),
		slog.String("session_id", r.sessionID), slog.String("grant_id", r.grantID))
}

// Run removes expired sessions, grants that stopped being revocable and
// their revocations until ctx ends.
func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		m.sweep()
	}
}

func (m *Manager) sweep() {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, s := range m.sessions {
		if !now.Before(s.expiresAt) {
			delete(m.sessions, id)
			for rid, r := range m.revocations {
				if r.sessionID == id {
					delete(m.revocations, rid)
				}
			}
		}
	}
	for _, g := range m.grants {
		switch {
		case !m.liveAt(g, now):
			m.removeGrantLocked(g)
		case g.state == grantPending && !now.Before(g.expiresAt.Add(m.Limits.AdmissionTolerance)):
			m.removeGrantLocked(g)
		case g.revoked && !m.hasRevocationLocked(g.id):
			// The agent acknowledged the revocation.
			m.removeGrantLocked(g)
		}
	}
}

func (m *Manager) hasRevocationLocked(grantID string) bool {
	for _, r := range m.revocations {
		if r.grantID == grantID {
			return true
		}
	}
	return false
}

// countsLocked returns the revocable grants of a device and of a principal
// on it. A pending successor shares the slot of its predecessor.
func (m *Manager) countsLocked(device string, p auth.Principal, now time.Time) (int, int) {
	dev, prin := 0, 0
	for _, g := range m.grants {
		if g.device != device || !m.revocableAt(g, now) {
			continue
		}
		if g.replaces != nil && m.revocableAt(g.replaces, now) {
			continue
		}
		dev++
		if g.principal.Same(p) {
			prin++
		}
	}
	return dev, prin
}

func sameOperation(g *grant, p auth.Principal, sandbox string, kind ownsandv1.RelayOperationKind, opID string, digest []byte) bool {
	return g.principal.Same(p) && g.sandbox == sandbox && g.kind == kind && g.opID == opID && bytes.Equal(g.digest, digest)
}
