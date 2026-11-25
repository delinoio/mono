// SPDX-License-Identifier: FSL-1.1-ALv2

// Package dataplane serves the data path of the agent: peer-to-peer
// WebRTC sessions that the control plane signals (pion, answerer,
// non-trickle) and the relay connection (RelayAgentService.Connect) with
// relay grant admission, credit windows and stream lifetimes. Both
// transports hand request frames to the operation dispatcher of package
// ops. Every session and relay grant is bound to the agent channel that
// signaled or sent it. See docs/cmds-ownsand-agent-foundation.md (data
// path) and docs/protos-ownsand-api-contract.md (data path framing).
package dataplane

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/agent"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/ops"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// Dispatcher serves operation requests.
type Dispatcher interface {
	Handle(ctx context.Context, req ops.Request, sink ops.Sink) ops.Outcome
	// ExecEnded reports whether an accepted exec has ended.
	ExecEnded(execID string) bool
}

// SandboxStates reports sandbox states.
type SandboxStates interface {
	State(sandboxID string) (ownsandv1.SandboxState, bool)
}

// Authenticator gets a new device session credential for each relay
// connection.
type Authenticator interface {
	NewDeviceSession(ctx context.Context) (string, error)
}

// Options configure the data plane.
type Options struct {
	Dispatcher Dispatcher
	Sandboxes  SandboxStates
	Auth       Authenticator
	// HTTP and APIURL reach the control plane origin for the relay
	// connection.
	HTTP   *http.Client
	APIURL string
	Logger *slog.Logger
	// Session and relay timing; zero values use the contract values.
	Timing Timing
	// IncludeLoopback adds loopback ICE candidates and InterfaceFilter
	// limits the gathered interfaces (tests only).
	IncludeLoopback bool
	InterfaceFilter func(string) bool
	// Limits of the relay; zero values use DefaultRelayLimits.
	Limits RelayLimits
	// Limits of the peer-to-peer sessions; zero values use
	// DefaultSessionLimits.
	SessionLimits SessionLimits
}

// Timing holds the session and relay durations.
type Timing struct {
	SessionIdle        time.Duration
	KeepaliveInterval  time.Duration
	ReceiveTimeout     time.Duration
	ProbeIdle          time.Duration
	ProbeTimeout       time.Duration
	ReconnectMax       time.Duration
	ConnectionIdle     time.Duration
	GrantIdle          time.Duration
	AdmissionTolerance time.Duration
	// FlushDeadline bounds how long a relay connection of a lost agent
	// channel may stay open while it flushes its queued session-end frames
	// and closes its request side (see rconn.closeAfterFlush).
	FlushDeadline time.Duration
	// ReconnectReset is the time an open relay connection must have lived
	// before the reconnect back-off restarts at its first step after the
	// connection ended with an error.
	ReconnectReset time.Duration
}

// DefaultTiming returns the contract values.
func DefaultTiming() Timing {
	return Timing{
		SessionIdle:        15 * time.Minute,
		KeepaliveInterval:  30 * time.Second,
		ReceiveTimeout:     90 * time.Second,
		ProbeIdle:          35 * time.Second,
		ProbeTimeout:       5 * time.Second,
		ReconnectMax:       5 * time.Second,
		ConnectionIdle:     60 * time.Second,
		GrantIdle:          60 * time.Second,
		AdmissionTolerance: 30 * time.Second,
		FlushDeadline:      5 * time.Second,
		ReconnectReset:     60 * time.Second,
	}
}

func (t Timing) withDefaults() Timing {
	d := DefaultTiming()
	set := func(v *time.Duration, def time.Duration) {
		if *v <= 0 {
			*v = def
		}
	}
	set(&t.SessionIdle, d.SessionIdle)
	set(&t.KeepaliveInterval, d.KeepaliveInterval)
	set(&t.ReceiveTimeout, d.ReceiveTimeout)
	set(&t.ProbeIdle, d.ProbeIdle)
	set(&t.ProbeTimeout, d.ProbeTimeout)
	set(&t.ReconnectMax, d.ReconnectMax)
	set(&t.ConnectionIdle, d.ConnectionIdle)
	set(&t.GrantIdle, d.GrantIdle)
	set(&t.AdmissionTolerance, d.AdmissionTolerance)
	set(&t.FlushDeadline, d.FlushDeadline)
	set(&t.ReconnectReset, d.ReconnectReset)
	return t
}

// DataPlane implements agent.DataPath.
type DataPlane struct {
	opts   Options
	timing Timing
	logger *slog.Logger
	// limits are the resolved peer-to-peer session limits; the rejection
	// logs rate-limit the warning of each kind.
	limits          SessionLimits
	sessionRejected limitRejectLog
	streamRejected  limitRejectLog

	newPeerConnection func([]string) (*webrtc.PeerConnection, error)

	mu              sync.Mutex
	stunURLs        []string
	pendingSessions map[string]*pendingSession
	sessions        map[string]*session
	// revoked holds the IDs of sessions that a revocation named before they
	// reserved a slot (see tombstones.go).
	revoked tombstones
	relay   *relay
	closed  bool
}

var _ agent.DataPath = (*DataPlane)(nil)

// New returns a data plane.
func New(opts Options) *DataPlane {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	d := &DataPlane{
		opts: opts, timing: opts.Timing.withDefaults(), logger: opts.Logger, limits: opts.SessionLimits.withDefaults(),
		pendingSessions: make(map[string]*pendingSession), sessions: make(map[string]*session),
	}
	d.newPeerConnection = d.createPeerConnection
	d.relay = newRelay(d)
	return d
}

// ChannelOpened binds a new agent channel.
func (d *DataPlane) ChannelOpened(ch agent.Channel) {
	d.relay.channelOpened(ch)
}

// ChannelLost ends every session and relay grant of the channel.
func (d *DataPlane) ChannelLost(ch agent.Channel) {
	gen := ch.Generation()
	d.mu.Lock()
	for _, p := range d.pendingSessions {
		if p.gen == gen {
			p.end(ownsandv1.SessionEndReason_SESSION_END_REASON_AGENT_CHANNEL_LOST)
		}
	}
	var ended []*session
	for _, s := range d.sessions {
		if s.gen == gen {
			ended = append(ended, s)
		}
	}
	d.mu.Unlock()
	for _, s := range ended {
		s.end(ownsandv1.SessionEndReason_SESSION_END_REASON_AGENT_CHANNEL_LOST)
	}
	d.relay.channelLost(ch)
}

// Configure applies the STUN URLs of a configuration message.
func (d *DataPlane) Configure(stunURLs []string) {
	d.mu.Lock()
	d.stunURLs = append([]string(nil), stunURLs...)
	d.mu.Unlock()
}

// Revoke ends the session or the relay grant that the revocation names.
func (d *DataPlane) Revoke(_ agent.Channel, rev *ownsandv1.SessionRevocation) {
	switch t := rev.GetTarget().(type) {
	case *ownsandv1.SessionRevocation_SessionId:
		d.mu.Lock()
		s := d.sessions[t.SessionId]
		p := d.pendingSessions[t.SessionId]
		if p != nil {
			p.end(ownsandv1.SessionEndReason_SESSION_END_REASON_REVOKED)
		} else if s == nil && t.SessionId != "" {
			// Remember the revocation so that Offer refuses this ID.
			d.revoked.add(t.SessionId, time.Now())
		}
		d.mu.Unlock()
		if s != nil {
			s.end(ownsandv1.SessionEndReason_SESSION_END_REASON_REVOKED)
		} else if p == nil && t.SessionId != "" {
			d.logger.Info("revocation of a session without a reservation; remembered until its offer or expiry", "session_id", t.SessionId)
		}
	case *ownsandv1.SessionRevocation_RelayGrantId:
		d.relay.revoke(t.RelayGrantId)
	}
}

// Grant installs a relay grant.
func (d *DataPlane) Grant(ch agent.Channel, g *ownsandv1.RelayGrant) *ownsandv1.RelayGrantAck {
	return d.relay.grant(ch, g)
}

// Close ends every session and the relay connection.
func (d *DataPlane) Close() {
	d.mu.Lock()
	d.closed = true
	for _, p := range d.pendingSessions {
		p.end(ownsandv1.SessionEndReason_SESSION_END_REASON_AGENT_CHANNEL_LOST)
	}
	sessions := make([]*session, 0, len(d.sessions))
	for _, s := range d.sessions {
		sessions = append(sessions, s)
	}
	d.mu.Unlock()
	for _, s := range sessions {
		s.end(ownsandv1.SessionEndReason_SESSION_END_REASON_AGENT_CHANNEL_LOST)
	}
	d.relay.close()
}
