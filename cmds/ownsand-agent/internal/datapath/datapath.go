package datapath

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/channel"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// Config configures the data path.
type Config struct {
	Sandboxes SandboxAccess
	Logger    *slog.Logger
	// Origin is the control plane origin of the relay connection.
	Origin string
	// Authenticate returns a new osd_ device session credential for each
	// relay connection.
	Authenticate func(ctx context.Context) (string, error)
	// Limits override DefaultRelayLimits (tests).
	Limits *RelayLimits
	// Now overrides the monotonic clock of relay grants (tests).
	Now func() time.Time
	// IncludeLoopback restricts ICE to loopback candidates (tests only).
	IncludeLoopback bool
}

// DataPath implements the agent's data path hooks.
type DataPath struct {
	log      *slog.Logger
	disp     *Dispatcher
	grants   *grantTable
	relay    *relayManager
	sessions *sessionTable
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup

	mu   sync.Mutex
	gen  uint64
	send channel.Sender
}

// New starts the data path.
func New(cfg Config) (*DataPath, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	lim := DefaultRelayLimits()
	if cfg.Limits != nil {
		lim = *cfg.Limits
	}
	ctx, cancel := context.WithCancel(context.Background())
	disp := NewDispatcher(ctx, cfg.Sandboxes, cfg.Logger, nil)
	grants := newGrantTable(cfg.Logger, cfg.Now)
	relay, err := newRelayManager(cfg.Logger, lim, disp, grants, cfg.Origin, cfg.Authenticate)
	if err != nil {
		cancel()
		return nil, err
	}
	d := &DataPath{
		log: cfg.Logger, disp: disp, grants: grants, relay: relay,
		sessions: newSessionTable(cfg.Logger, disp, cfg.Sandboxes, cfg.IncludeLoopback),
		ctx:      ctx, cancel: cancel,
	}
	d.wg.Add(2)
	go func() {
		defer d.wg.Done()
		relay.run(ctx)
	}()
	go func() {
		defer d.wg.Done()
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				grants.sweep()
				disp.Sweep()
			}
		}
	}()
	return d, nil
}

func (d *DataPath) sendEnv(env *ownsandv1.AgentEnvelope) bool {
	d.mu.Lock()
	send := d.send
	d.mu.Unlock()
	return send != nil && send(env)
}

// Connected binds grants and sessions to a new agent channel.
func (d *DataPath) Connected(gen uint64, send channel.Sender) {
	d.mu.Lock()
	d.gen, d.send = gen, send
	d.mu.Unlock()
	d.grants.setChannel(gen, send)
}

// Disconnected ends every session and relay grant of the channel with
// AGENT_CHANNEL_LOST and closes the relay connection.
func (d *DataPath) Disconnected(gen uint64) {
	d.mu.Lock()
	if d.gen == gen {
		d.send = nil
	}
	d.mu.Unlock()
	d.sessions.channelLost(gen)
	d.grants.channelLost(gen)
	d.relay.closeChannel()
}

// SetIceServers stores the STUN servers of the configuration message.
func (d *DataPath) SetIceServers(servers []*ownsandv1.IceServer) { d.sessions.setIceServers(servers) }

// HandleOffer answers an OpenSession offer.
func (d *DataPath) HandleOffer(gen uint64, offer *ownsandv1.SignalingOffer) {
	go func() {
		ans := d.sessions.answer(gen, offer)
		d.sendEnv(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_SignalingAnswer{SignalingAnswer: ans}})
	}()
}

// HandleRelayGrant installs a grant, acknowledges it and makes sure a relay
// connection exists.
func (d *DataPath) HandleRelayGrant(gen uint64, g *ownsandv1.RelayGrant) {
	ack := d.grants.install(gen, g)
	if ack.GetResult() == ownsandv1.RelayGrantResult_RELAY_GRANT_RESULT_UNSPECIFIED {
		return
	}
	d.sendEnv(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_RelayGrantAck{RelayGrantAck: ack}})
	if ack.GetResult() == ownsandv1.RelayGrantResult_RELAY_GRANT_RESULT_ACCEPTED {
		d.relay.probe()
	}
}

// HandleRevocation ends the session or the relay grant that the revocation
// names and acknowledges it, also when the target already ended.
func (d *DataPath) HandleRevocation(_ uint64, rev *ownsandv1.SessionRevocation) {
	switch t := rev.GetTarget().(type) {
	case *ownsandv1.SessionRevocation_SessionId:
		d.sessions.end(t.SessionId, ownsandv1.SessionEndReason_SESSION_END_REASON_REVOKED)
	case *ownsandv1.SessionRevocation_GrantId:
		d.grants.revoke(t.GrantId)
	}
	d.sendEnv(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_RevocationAck{RevocationAck: &ownsandv1.RevocationAck{RevocationId: rev.GetRevocationId()}}})
}

// Close stops the data path.
func (d *DataPath) Close() {
	d.sessions.closeAll()
	d.relay.closeChannel()
	d.cancel()
	d.wg.Wait()
}
