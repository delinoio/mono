// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/cpclient"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/guestclient"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/sysinfo"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/version"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// sendQueue bounds the envelopes waiting for the channel writer.
const sendQueue = 1024

// conn is one agent channel connection.
type conn struct {
	gen    uint64
	queue  chan *ownsandv1.AgentEnvelope
	done   chan struct{}
	once   sync.Once
	mu     sync.Mutex
	acked  time.Time
	sentHB uint64
}

func (c *conn) Generation() uint64    { return c.gen }
func (c *conn) Done() <-chan struct{} { return c.done }

// Send queues env. It never blocks: a full queue closes the channel, so a
// stalled control plane leads to a reconnect and a full state report.
func (c *conn) Send(env *ownsandv1.AgentEnvelope) bool {
	select {
	case <-c.done:
		return false
	default:
	}
	select {
	case c.queue <- env:
		return true
	default:
		c.close()
		return false
	}
}

func (c *conn) close() { c.once.Do(func() { close(c.done) }) }

// runChannel authenticates, opens one agent channel and serves it until it
// closes.
func (a *Agent) runChannel(ctx context.Context) error {
	st, err := a.st.LoadAgent()
	if err != nil {
		return err
	}
	res, err := a.cp.Authenticate(ctx, a.DeviceID(), a.key, st.StateEpoch)
	if errors.Is(err, cpclient.ErrUnknownDevice) {
		// The control plane keeps a tombstone for every removed device for
		// up to a year, so an unknown device without a recorded removal
		// notice keeps its data; an operator runs `ownsand-agent reset`.
		a.logger.Error("authentication failed: the control plane does not know this device; keeping local data")
		a.setConnected(false)
		return err
	}
	if err != nil {
		a.logger.Warn("authentication failed", "error", err)
		return err
	}
	if res.Removal != nil {
		a.logger.Info("authentication returned a removal notice")
		return a.removeDevice(ctx)
	}
	a.logger.Info("authenticated", "device_id", a.DeviceID(), "credential_expires_in_s", int64(res.Session.ExpiresIn.Seconds()))

	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream := a.cp.Agent.Channel(cctx)
	cpclient.SetBearer(stream.RequestHeader(), res.Session.Credential)
	guestclient.CloseOnDone(cctx, stream)

	a.mu.Lock()
	a.generation++
	c := &conn{gen: a.generation, queue: make(chan *ownsandv1.AgentEnvelope, sendQueue), done: make(chan struct{}), acked: time.Now()}
	a.conn = c
	a.mu.Unlock()
	defer func() {
		c.close()
		a.mu.Lock()
		if a.conn == c {
			a.conn = nil
		}
		a.mu.Unlock()
		a.dataPath.ChannelLost(c)
		a.setConnected(false)
		a.logger.Info("agent channel disconnected", "generation", c.gen)
	}()

	writerErr := make(chan error, 1)
	go func() {
		for {
			select {
			case <-c.done:
				writerErr <- errors.New("agent channel closed")
				cancel()
				return
			case env := <-c.queue:
				if err := stream.Send(&ownsandv1.ChannelRequest{Envelope: env}); err != nil {
					writerErr <- err
					c.close()
					cancel()
					return
				}
			}
		}
	}()

	c.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_CapabilityReport{CapabilityReport: a.capabilityReport(ctx)}})
	a.mu.Lock()
	a.reportSeq++
	seq := a.reportSeq
	a.mu.Unlock()
	c.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_StateReport{StateReport: a.Sandboxes.StateReport(seq, a.rt.Capabilities(ctx).BaseImages)}})
	a.dataPath.ChannelOpened(c)
	a.setConnected(true)
	a.logger.Info("agent channel connected", "generation", c.gen)

	go a.heartbeats(cctx, c, cancel)

	for {
		resp, err := stream.Receive()
		if err != nil {
			select {
			case werr := <-writerErr:
				return werr
			default:
			}
			if cctx.Err() != nil && ctx.Err() == nil {
				return errors.New("agent channel lost")
			}
			return err
		}
		if env := resp.GetEnvelope(); env != nil {
			a.dispatch(cctx, c, env)
		}
	}
}

// heartbeats sends a heartbeat every interval, reports capabilities when
// the free disk needs it, and closes the channel when no acknowledgement
// arrived on it within the acknowledgement timeout.
func (a *Agent) heartbeats(ctx context.Context, c *conn, cancel context.CancelFunc) {
	t := time.NewTicker(a.opts.HeartbeatInterval)
	defer t.Stop()
	lastCap := time.Now()
	lastFree, _ := sysinfo.FreeDiskBytes(a.st.Dir())
	var seq uint64
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.done:
			return
		case <-t.C:
		}
		c.mu.Lock()
		acked := c.acked
		c.mu.Unlock()
		if time.Since(acked) > a.opts.AckTimeout {
			a.logger.Warn("no heartbeat acknowledgement; treating the agent channel as lost", "generation", c.gen)
			c.close()
			cancel()
			return
		}
		seq++
		state, target := a.updater.State()
		c.mu.Lock()
		c.sentHB = seq
		c.mu.Unlock()
		c.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_Heartbeat{Heartbeat: &ownsandv1.Heartbeat{
			Sequence: seq, UpdateState: state, UpdateTargetVersion: target,
		}}})
		free, _ := sysinfo.FreeDiskBytes(a.st.Dir())
		if time.Since(lastCap) >= CapabilityInterval || abs(free-lastFree) > capabilityDiskDelta {
			lastCap, lastFree = time.Now(), free
			c.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_CapabilityReport{CapabilityReport: a.capabilityReport(ctx)}})
		}
	}
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func (a *Agent) capabilityReport(ctx context.Context) *ownsandv1.CapabilityReport {
	caps := a.rt.Capabilities(ctx)
	free, _ := sysinfo.FreeDiskBytes(a.st.Dir())
	return &ownsandv1.CapabilityReport{
		HostOs:       sysinfo.HostOS(),
		Arch:         sysinfo.Arch(),
		AgentVersion: version.Version,
		Capabilities: &ownsandv1.DeviceCapabilities{
			LogicalCpuCount:      sysinfo.LogicalCPUs(),
			MemoryMib:            sysinfo.MemoryMiB(),
			FreeDiskBytes:        free,
			GuestOses:            caps.GuestOSes,
			BuiltBaseImages:      caps.BaseImages,
			MacosGuestsSupported: caps.MacOSGuestsSupported,
			MacosHardwareModels:  caps.MacOSHardwareModels,
			HypervisorChecks:     caps.Checks,
		},
	}
}

// dispatch handles one envelope of the control plane.
func (a *Agent) dispatch(ctx context.Context, c *conn, env *ownsandv1.AgentEnvelope) {
	switch m := env.GetMessage().(type) {
	case *ownsandv1.AgentEnvelope_HeartbeatAck:
		c.mu.Lock()
		if m.HeartbeatAck.GetSequence() <= c.sentHB && m.HeartbeatAck.GetSequence() > 0 {
			c.acked = time.Now()
		}
		c.mu.Unlock()
	case *ownsandv1.AgentEnvelope_StateAck:
		switch ack := m.StateAck.GetAck().(type) {
		case *ownsandv1.StateAck_StateReport:
			a.Sandboxes.AckStateReport(ack.StateReport)
		case *ownsandv1.StateAck_SandboxUpdate:
			a.Sandboxes.AckSandboxUpdate(ack.SandboxUpdate)
		}
	case *ownsandv1.AgentEnvelope_Command:
		go a.handleCommand(ctx, m.Command)
	case *ownsandv1.AgentEnvelope_Configuration:
		cfg := m.Configuration
		a.Sandboxes.SetLimits(cfg.GetLimits())
		a.dataPath.Configure(cfg.GetStunUrls())
		if _, err := a.st.UpdateAgent(func(s *store.AgentState) error { s.STUNURLs = cfg.GetStunUrls(); return nil }); err != nil {
			a.logger.Warn("persist configuration failed", "error", err)
		}
		a.logger.Info("configuration received", "stun_url_count", len(cfg.GetStunUrls()), "vcpu_limit", cfg.GetLimits().GetVcpuCount(), "memory_mib_limit", cfg.GetLimits().GetMemoryMib(), "disk_gib_limit", cfg.GetLimits().GetDiskGib())
	case *ownsandv1.AgentEnvelope_Signaling:
		if offer := m.Signaling.GetOffer(); offer != nil {
			go func() {
				answer := a.dataPath.Offer(ctx, c, offer)
				c.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_Signaling{Signaling: &ownsandv1.SignalingMessage{
					Message: &ownsandv1.SignalingMessage_Answer{Answer: answer},
				}}})
			}()
		}
	case *ownsandv1.AgentEnvelope_SessionRevocation:
		a.dataPath.Revoke(c, m.SessionRevocation)
		// Acknowledged also when the session or grant already ended.
		c.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_RevocationAck{RevocationAck: &ownsandv1.RevocationAck{RevocationId: m.SessionRevocation.GetRevocationId()}}})
	case *ownsandv1.AgentEnvelope_SignedUrlGrant:
		a.deliverURLs(m.SignedUrlGrant)
	case *ownsandv1.AgentEnvelope_RelayGrant:
		if ack := a.dataPath.Grant(c, m.RelayGrant); ack != nil {
			c.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_RelayGrantAck{RelayGrantAck: ack}})
		}
	default:
		a.logger.Warn("ignoring an unexpected agent channel message")
	}
}
