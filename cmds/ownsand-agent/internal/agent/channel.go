// SPDX-License-Identifier: FSL-1.1-ALv2

package agent

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/cpclient"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/guestclient"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/sysinfo"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/version"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// sendQueue bounds the envelopes waiting for the channel writer.
const sendQueue = 1024

// conn is one agent channel connection.
type conn struct {
	gen   uint64
	queue chan *ownsandv1.AgentEnvelope
	done  chan struct{}
	// cancel cancels the stream context of the connection. close calls it so
	// that a closed connection also aborts a stream whose writer is blocked
	// in a send and whose receiver is blocked in a receive.
	cancel context.CancelFunc
	logger *slog.Logger
	once   sync.Once
	mu     sync.Mutex
	acked  time.Time
	sentHB uint64
}

func (c *conn) Generation() uint64    { return c.gen }
func (c *conn) Done() <-chan struct{} { return c.done }

// Send queues env. It never blocks: a full queue closes the channel and
// cancels its stream, even when the writer is blocked in a send, so a stalled
// control plane leads to a reconnect and a full state report.
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
		c.logger.Warn("agent channel send queue full", "generation", c.gen, "queue_size", cap(c.queue))
		c.close()
		return false
	}
}

// close ends the connection once: it signals done and cancels the stream.
func (c *conn) close() {
	c.once.Do(func() {
		close(c.done)
		if c.cancel != nil {
			c.cancel()
		}
	})
}

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
	c := &conn{gen: a.generation, queue: make(chan *ownsandv1.AgentEnvelope, sendQueue), done: make(chan struct{}), cancel: cancel, logger: a.logger, acked: time.Now()}
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
			a.dispatch(cctx, ctx, c, env)
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
	caps.MacOSHardwareModels = a.macOSHardwareModels(caps.MacOSHardwareModels)
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

// macOSHardwareModels adds the hardware models of the local macOS
// snapshots that the runtime supports to the models of its built image, so
// that the scheduler can place sandboxes from those snapshots here.
func (a *Agent) macOSHardwareModels(models [][]byte) [][]byte {
	checker, ok := a.rt.(vmruntime.MacOSHardwareModelChecker)
	if !ok {
		return models
	}
	seen := map[string]bool{}
	for _, m := range models {
		seen[string(m)] = true
	}
	for _, m := range a.Sandboxes.MacOSHardwareModels() {
		if !seen[string(m)] && checker.MacOSHardwareModelSupported(m) {
			seen[string(m)] = true
			models = append(models, m)
		}
	}
	return models
}

// dispatch handles one envelope of the control plane. chanCtx ends with the
// channel connection that delivered env; agentCtx ends only when the agent
// stops, installs an update or removes the device.
func (a *Agent) dispatch(chanCtx, agentCtx context.Context, c *conn, env *ownsandv1.AgentEnvelope) {
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
		// An update or withdrawal command takes its order here, in receive
		// order, because the commands run in separate goroutines.
		var order uint64
		if isUpdateCommand(m.Command) {
			order = a.updater.NextOrder()
		}
		go a.handleCommand(chanCtx, agentCtx, m.Command, order)
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
				answer := a.dataPath.Offer(chanCtx, c, offer)
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
