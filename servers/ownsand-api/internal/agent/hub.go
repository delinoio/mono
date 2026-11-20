// SPDX-License-Identifier: FSL-1.1-ALv2

// Package agent serves the device agent channel (AgentService.Channel) and
// keeps the live channels of this instance: generations, heartbeats, the
// 45-second offline rule, state reconciliation, commands with IDs, and the
// consumer of the cross-instance control events. Contract:
// docs/servers-ownsand-api-foundation.md#agent-channel.
package agent

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/store"
)

// Timing of the agent channel.
const (
	HeartbeatTimeout = 45 * time.Second
	SweepInterval    = 5 * time.Second
	EventInterval    = 500 * time.Millisecond
	// sendBuffer bounds the queued outbound envelopes of one channel; a
	// channel whose queue is full is closed, because the agent stopped
	// reading.
	sendBuffer = 256
)

// Lease reports whether this instance holds the single-instance lease.
type Lease interface{ Held() bool }

// Extension receives the agent channel events that the data path layer
// handles (signaling answers, revocation acknowledgements and relay grant
// acknowledgements and releases).
type Extension interface {
	// HandleEnvelope handles a message; it reports false for messages it
	// does not know.
	HandleEnvelope(c *Conn, env *ownsandv1.AgentEnvelope) bool
	// ChannelClosed runs after a channel closed.
	ChannelClosed(c *Conn)
	// ControlEvent runs for each control event on the lease holder.
	ControlEvent(e store.ControlEvent)
}

// SignedURLs issues the signed part URLs of exports and imports.
type SignedURLs interface {
	Grant(ctx context.Context, deviceID string, req *ownsandv1.SignedUrlRequest) *ownsandv1.SignedUrlGrant
}

// Hub holds the live agent channels of this instance.
type Hub struct {
	Store      *store.Store
	Logger     *slog.Logger
	Lease      Lease
	IceServers func() []*ownsandv1.IceServer
	SignedURLs SignedURLs
	Extension  Extension
	// ReleaseManifestURL is sent in the configuration message.
	ReleaseManifestURL string
	// HeartbeatTimeout replaces the 45-second channel loss rule in tests.
	HeartbeatTimeout time.Duration

	// ready is set while this instance holds the lease and the control event
	// consumer has recorded its starting position; channels are accepted only
	// then, so no event that concerns a channel of this instance is skipped.
	ready atomic.Bool

	mu       sync.Mutex
	channels map[string]*Conn
	gen      uint64
	waiters  map[string]*waiter
	// changes is closed and replaced whenever a sandbox changes, so that
	// handlers that wait for an agent result can wake up.
	changes chan struct{}
}

type waiter struct {
	ch         chan *ownsandv1.AgentCommandResult
	deviceID   string
	generation uint64
	kind       commandKind
	sandboxID  string
	snapshotID string
}

type commandKind int

const (
	commandOther commandKind = iota
	commandCreateSandbox
	commandCreateSnapshot
	commandExportSnapshot
)

// Conn is one agent channel.
type Conn struct {
	DeviceID       string
	OrganizationID string
	Generation     uint64

	out    chan *ownsandv1.AgentEnvelope
	done   chan struct{}
	once   sync.Once
	online atomic.Bool
	reason atomic.Value // string
}

// Done is closed when the channel closes.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Online reports whether the reconnection state report was applied.
func (c *Conn) Online() bool { return c.online.Load() }

// Send queues an envelope. It reports false when the channel is closed or its
// queue is full (the channel is then closed).
func (c *Conn) Send(env *ownsandv1.AgentEnvelope) bool {
	select {
	case <-c.done:
		return false
	default:
	}
	select {
	case c.out <- env:
		return true
	case <-c.done:
		return false
	default:
		c.Close("send queue full")
		return false
	}
}

// Close closes the channel.
func (c *Conn) Close(reason string) {
	c.once.Do(func() {
		c.reason.Store(reason)
		close(c.done)
	})
}

// NewHub returns a hub.
func NewHub(st *store.Store, logger *slog.Logger, lease Lease) *Hub {
	return &Hub{Store: st, Logger: logger, Lease: lease, channels: map[string]*Conn{}, waiters: map[string]*waiter{}, changes: make(chan struct{})}
}

// Ready reports whether the hub accepts agent channels.
func (h *Hub) Ready() bool { return h.ready.Load() && h.Lease != nil && h.Lease.Held() }

// Live returns the current channel of a device when its state report was
// applied.
func (h *Hub) Live(deviceID string) (*Conn, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c, ok := h.channels[deviceID]
	if !ok || !c.Online() {
		return nil, false
	}
	return c, true
}

// IsLive reports whether the device has a current, online channel here.
func (h *Hub) IsLive(deviceID string) bool {
	_, ok := h.Live(deviceID)
	return ok
}

// Current returns the current channel of a device, online or not.
func (h *Hub) Current(deviceID string) (*Conn, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c, ok := h.channels[deviceID]
	return c, ok
}

// Changes returns a channel that is closed at the next sandbox change.
func (h *Hub) Changes() <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.changes
}

func (h *Hub) notifyChange() {
	h.mu.Lock()
	close(h.changes)
	h.changes = make(chan struct{})
	h.mu.Unlock()
}

func (h *Hub) register(deviceID, organizationID string) (*Conn, *Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.gen++
	c := &Conn{DeviceID: deviceID, OrganizationID: organizationID, Generation: h.gen, out: make(chan *ownsandv1.AgentEnvelope, sendBuffer), done: make(chan struct{})}
	old := h.channels[deviceID]
	h.channels[deviceID] = c
	return c, old
}

// unregister removes c when it is still current and reports whether it was.
func (h *Hub) unregister(c *Conn) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.channels[c.DeviceID] != c {
		return false
	}
	delete(h.channels, c.DeviceID)
	for id, w := range h.waiters {
		if w.deviceID == c.DeviceID && w.generation == c.Generation {
			close(w.ch)
			delete(h.waiters, id)
		}
	}
	return true
}

// CloseAll closes every channel (at a lease loss or a drain).
func (h *Hub) CloseAll(reason string) {
	h.mu.Lock()
	conns := make([]*Conn, 0, len(h.channels))
	for _, c := range h.channels {
		conns = append(conns, c)
	}
	h.mu.Unlock()
	for _, c := range conns {
		c.Close(reason)
	}
}

// CloseDevice closes the channel of a device.
func (h *Hub) CloseDevice(deviceID, reason string) {
	if c, ok := h.Current(deviceID); ok {
		c.Close(reason)
	}
}

// Errors of the channel and of commands.
var (
	ErrNoLease   = connect.NewError(connect.CodeUnavailable, errors.New("this instance does not accept agent channels now; retry"))
	ErrNoChannel = connect.NewError(connect.CodeUnavailable, errors.New("the device has no live agent channel"))
)

// Command sends a command with a new command ID to the device's live
// channel. With wait > 0 it waits up to wait for the result and returns nil
// without an error when the wait ends first.
func (h *Hub) Command(ctx context.Context, deviceID string, cmd *ownsandv1.AgentCommand, wait time.Duration) (*ownsandv1.AgentCommandResult, error) {
	c, ok := h.Live(deviceID)
	if !ok {
		return nil, ErrNoChannel
	}
	cmd.CommandId = store.NewID()
	w := &waiter{ch: make(chan *ownsandv1.AgentCommandResult, 1), deviceID: deviceID, generation: c.Generation}
	switch x := cmd.GetCommand().(type) {
	case *ownsandv1.AgentCommand_CreateSandbox:
		w.kind, w.sandboxID = commandCreateSandbox, x.CreateSandbox.GetSandboxId()
	case *ownsandv1.AgentCommand_CreateSnapshot:
		w.kind, w.sandboxID, w.snapshotID = commandCreateSnapshot, x.CreateSnapshot.GetSandboxId(), x.CreateSnapshot.GetSnapshotId()
	case *ownsandv1.AgentCommand_ExportSnapshot:
		w.kind, w.snapshotID = commandExportSnapshot, x.ExportSnapshot.GetSnapshotId()
	}
	if w.kind == commandCreateSandbox {
		// The agent checks a create against the stricter of the command's
		// limits and its last configuration. A limits change reaches the
		// agent through the control event poll, which can arrive after a
		// create that the scheduler placed with the new limits, so the
		// configuration goes first on the ordered channel.
		h.SendConfiguration(ctx, c)
	}
	h.mu.Lock()
	h.waiters[cmd.CommandId] = w
	h.mu.Unlock()
	if !c.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_Command{Command: cmd}}) {
		h.mu.Lock()
		delete(h.waiters, cmd.CommandId)
		h.mu.Unlock()
		return nil, ErrNoChannel
	}
	h.Logger.Info("agent_command_sent", slog.String("device_id", deviceID), slog.String("command_id", cmd.CommandId), slog.String("command", commandName(cmd)))
	if wait <= 0 {
		return nil, nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case res, ok := <-w.ch:
		if !ok {
			return nil, ErrNoChannel
		}
		return res, nil
	case <-timer.C:
		return nil, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func commandName(cmd *ownsandv1.AgentCommand) string {
	if cmd.GetCommand() == nil {
		return "none"
	}
	name := string(cmd.ProtoReflect().WhichOneof(cmd.ProtoReflect().Descriptor().Oneofs().ByName("command")).Name())
	return strings.ToLower(name)
}

// SendDelete sends a delete command without waiting.
func (h *Hub) SendDelete(deviceID, sandboxID string, reason ownsandv1.SandboxEndReason) {
	_, err := h.Command(context.Background(), deviceID, &ownsandv1.AgentCommand{Command: &ownsandv1.AgentCommand_DeleteSandbox{
		DeleteSandbox: &ownsandv1.DeleteSandboxCommand{SandboxId: sandboxID, Reason: reason},
	}}, 0)
	if err != nil {
		h.Logger.Debug("agent_delete_not_sent", slog.String("device_id", deviceID), slog.String("sandbox_id", sandboxID))
	}
}

// Configuration builds the configuration message of a device.
func (h *Hub) Configuration(ctx context.Context, deviceID string) (*ownsandv1.AgentConfiguration, error) {
	d, err := h.Store.GetDeviceByID(ctx, deviceID)
	if err != nil {
		return nil, err
	}
	pol, err := h.Store.GetAgentVersionPolicy(ctx, d.OrganizationID)
	if err != nil {
		return nil, err
	}
	revoked := map[string]bool{}
	for _, v := range []string{d.AgentVersion, pol.PinnedVersion} {
		if v == "" {
			continue
		}
		r, err := h.Store.VersionRevoked(ctx, v)
		if err != nil {
			return nil, err
		}
		revoked[v] = r
	}
	target, recovery := TargetVersion(pol, d, revoked)
	cfg := &ownsandv1.AgentConfiguration{
		Limits:        &ownsandv1.DeviceLimits{VcpuCount: d.Limits.VCPUCount, MemoryMib: d.Limits.MemoryMiB, DiskGib: d.Limits.DiskGiB},
		TargetVersion: target, RevocationRecovery: recovery, ReleaseManifestUrl: h.ReleaseManifestURL,
	}
	if h.IceServers != nil {
		cfg.IceServers = h.IceServers()
	}
	return cfg, nil
}

// PlatformKey returns "<host_os>/<arch>" in lowercase, for example "linux/amd64".
func PlatformKey(os ownsandv1.HostOs, arch ownsandv1.Arch) string {
	o := strings.ToLower(strings.TrimPrefix(os.String(), "HOST_OS_"))
	a := strings.ToLower(strings.TrimPrefix(arch.String(), "ARCH_"))
	return o + "/" + a
}

// TargetVersion applies the organization policy. When the running or pinned
// version is revoked, the default target wins regardless of HOLD or PIN and
// is marked as a revocation recovery. HOLD targets the running version.
func TargetVersion(pol store.AgentVersionPolicy, d store.Device, revoked map[string]bool) (string, bool) {
	def := pol.DefaultVersions[PlatformKey(d.Capabilities.GetHostOs(), d.Capabilities.GetArch())]
	if revoked[d.AgentVersion] || (pol.Mode == ownsandv1.AgentVersionPolicyMode_AGENT_VERSION_POLICY_MODE_PIN && revoked[pol.PinnedVersion]) {
		return def, def != ""
	}
	switch pol.Mode {
	case ownsandv1.AgentVersionPolicyMode_AGENT_VERSION_POLICY_MODE_PIN:
		return pol.PinnedVersion, false
	case ownsandv1.AgentVersionPolicyMode_AGENT_VERSION_POLICY_MODE_HOLD:
		return d.AgentVersion, false
	default:
		return def, false
	}
}

// SendConfiguration sends the current configuration to a device's channel.
func (h *Hub) SendConfiguration(ctx context.Context, c *Conn) {
	cfg, err := h.Configuration(ctx, c.DeviceID)
	if err != nil {
		h.Logger.Warn("agent_configuration_failed", slog.String("device_id", c.DeviceID), slog.Any("error", err))
		return
	}
	c.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_Configuration{Configuration: cfg}})
}
