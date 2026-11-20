// Package agent wires the device agent: authentication, the agent channel,
// the sandbox manager, the update state machine, device removal, capability
// and state reports, and the data path hooks.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/channel"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/controlplane"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/hostinfo"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/runtime"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/sandbox"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/state"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/update"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

const (
	// capabilityInterval is the longest time between two free disk reports.
	capabilityInterval = 60 * time.Second
	// diskReportDelta is the free disk change that triggers a report.
	diskReportDelta = 1 << 30
)

// ErrRemoved is returned by Run after a confirmed device removal.
var ErrRemoved = errors.New("the device was removed and its local data was deleted")

// ErrRestartForUpdate is returned by Run after an update was installed; the
// service manager starts the new binary.
var ErrRestartForUpdate = errors.New("agent update installed; restarting")

// DataPath is the data path of the agent: peer-to-peer sessions and relay
// grants. All methods must not block.
type DataPath interface {
	Connected(generation uint64, send channel.Sender)
	Disconnected(generation uint64)
	SetIceServers(servers []*ownsandv1.IceServer)
	HandleOffer(generation uint64, offer *ownsandv1.SignalingOffer)
	HandleRelayGrant(generation uint64, grant *ownsandv1.RelayGrant)
	HandleRevocation(generation uint64, rev *ownsandv1.SessionRevocation)
	Close()
}

// Config configures an Agent.
type Config struct {
	Dir      *state.Dir
	Identity *state.Identity
	Version  string
	Runtime  runtime.Runtime
	Logger   *slog.Logger
	// Client overrides the control plane client (tests).
	Client *controlplane.Client
	// ReleaseKeys overrides update.BuiltinReleaseKeys (tests).
	ReleaseKeys []update.ReleaseKey
	// NewDataPath builds the data path. Default: a data path that rejects
	// sessions and grants.
	NewDataPath func(a *Agent) DataPath
	// Install overrides update.InstallBinary (tests).
	Install func(path string) error
	// ChannelTiming overrides the channel timing (tests).
	ChannelTiming *channel.Config
}

// Agent is the running device agent.
type Agent struct {
	cfg Config
	log *slog.Logger
	cp  *controlplane.Client
	Mgr *sandbox.Manager
	upd *update.Updater
	dp  DataPath

	mu          sync.Mutex
	send        channel.Sender
	gen         uint64
	conn        state.ConnectionState
	lastConnect time.Time
	lastErr     string
	limits      *ownsandv1.DeviceLimits
	lastCaps    *ownsandv1.DeviceCapabilities
	lastCapsAt  time.Time

	stop context.CancelCauseFunc
}

// New builds an Agent. Run starts it.
func New(ctx context.Context, cfg Config) (*Agent, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if !cfg.Identity.Enrolled() {
		return nil, state.ErrNotEnrolled
	}
	a := &Agent{cfg: cfg, log: cfg.Logger.With("device_id", cfg.Identity.DeviceID), conn: state.ConnectionDisconnected}
	a.cp = cfg.Client
	if a.cp == nil {
		cp, err := controlplane.New(cfg.Identity.Origin)
		if err != nil {
			return nil, err
		}
		a.cp = cp
	}
	keys := cfg.ReleaseKeys
	if keys == nil {
		keys = update.BuiltinReleaseKeys
	}
	install := cfg.Install
	if install == nil {
		install = update.InstallBinary
	}
	dl, err := cfg.Dir.Sub("updates")
	if err != nil {
		return nil, err
	}
	a.upd = update.New(update.Config{
		CurrentVersion: cfg.Version,
		Platform:       hostinfo.Platform(),
		Keys:           keys,
		DownloadDir:    dl,
		Logger:         a.log,
		Busy:           func() bool { return a.Mgr != nil && a.Mgr.Busy() },
		Install: func(path string) error {
			if err := install(path); err != nil {
				return err
			}
			a.log.Info("agent binary replaced; exiting so the service manager starts the new version")
			if a.stop != nil {
				a.stop(ErrRestartForUpdate)
			}
			return nil
		},
		Report: func(s *ownsandv1.UpdateStatus) {
			a.sendEnvelope(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_UpdateStatus{UpdateStatus: s}})
		},
	})
	mgr, err := sandbox.New(ctx, sandbox.Config{
		Runtime:  cfg.Runtime,
		Dir:      cfg.Dir,
		Logger:   a.log,
		Reporter: reporter{a},
		Gate:     a.upd,
		FreeDisk: func() int64 { return hostinfo.FreeDiskBytes(cfg.Dir.Path) },
	})
	if err != nil {
		return nil, err
	}
	a.Mgr = mgr
	if cfg.NewDataPath != nil {
		a.dp = cfg.NewDataPath(a)
	} else {
		a.dp = rejectingDataPath{a}
	}
	return a, nil
}

// Log returns the agent logger.
func (a *Agent) Log() *slog.Logger { return a.log }

// Identity returns the device identity.
func (a *Agent) Identity() *state.Identity { return a.cfg.Identity }

// Client returns the control plane client.
func (a *Agent) Client() *controlplane.Client { return a.cp }

// Updater returns the update state machine.
func (a *Agent) Updater() *update.Updater { return a.upd }

type reporter struct{ a *Agent }

func (r reporter) SandboxUpdate(rep *ownsandv1.SandboxReport) {
	r.a.sendEnvelope(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_SandboxUpdate{SandboxUpdate: &ownsandv1.SandboxUpdate{Sandbox: rep}}})
}

func (r reporter) SnapshotUpdate(u *ownsandv1.SnapshotUpdate) {
	r.a.sendEnvelope(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_SnapshotUpdate{SnapshotUpdate: u}})
}

// sendEnvelope sends on the current channel; it drops the message when no
// channel is open (updates are resent until acknowledged).
func (a *Agent) sendEnvelope(env *ownsandv1.AgentEnvelope) bool {
	a.mu.Lock()
	send := a.send
	a.mu.Unlock()
	if send == nil {
		return false
	}
	return send(env)
}

// Run runs the agent until ctx ends, the device is removed or an update
// was installed.
func (a *Agent) Run(ctx context.Context) error {
	ctx, stop := context.WithCancelCause(ctx)
	a.stop = stop
	defer stop(nil)
	defer a.dp.Close()

	if err := a.Mgr.Recover(ctx); err != nil {
		return fmt.Errorf("recover sandboxes: %w", err)
	}
	a.log.Info("agent starting", "version", a.cfg.Version, "runtime", a.cfg.Runtime.Name(), "state_epoch", a.Mgr.Epoch())

	var wg sync.WaitGroup
	defer wg.Wait()
	wg.Add(3)
	go func() {
		defer wg.Done()
		a.Mgr.Run(ctx, func(p []*ownsandv1.SandboxReport) {
			for _, rep := range p {
				reporter{a}.SandboxUpdate(rep)
			}
		})
	}()
	go func() {
		defer wg.Done()
		a.periodic(ctx)
	}()

	ccfg := channel.Config{}
	if a.cfg.ChannelTiming != nil {
		ccfg = *a.cfg.ChannelTiming
	}
	ccfg.Client = a.cp.Agent
	ccfg.Authenticate = a.authenticate
	ccfg.Handler = a
	ccfg.Logger = a.log
	ccfg.OnConnecting = func() { a.setConn(state.ConnectionConnecting, "") }
	var runErr error
	go func() {
		defer wg.Done()
		runErr = channel.Run(ctx, ccfg)
		stop(runErr)
	}()
	<-ctx.Done()
	cause := context.Cause(ctx)
	if errors.Is(cause, ErrRemoved) || errors.Is(cause, ErrRestartForUpdate) {
		return cause
	}
	return nil
}

// authenticate returns a new device session credential. It runs the removal
// flow when the device was removed.
func (a *Agent) authenticate(ctx context.Context) (string, error) {
	rem, err := a.cfg.Dir.LoadRemoval()
	if err != nil {
		return "", err
	}
	if rem != nil {
		return "", a.removal(ctx)
	}
	id := a.cfg.Identity
	res, sess, err := a.cp.Authenticate(ctx, id.DeviceID, id.PrivateKey, a.Mgr.Epoch())
	if err != nil {
		a.setConn(state.ConnectionDisconnected, "authentication failed")
		return "", err
	}
	switch res {
	case controlplane.AuthRemoved:
		a.log.Warn("authentication returned the removal notice")
		if err := a.cfg.Dir.SaveRemoval(&state.Removal{NoticeReceivedAt: time.Now()}); err != nil {
			return "", err
		}
		return "", a.removal(ctx)
	case controlplane.AuthUnknownDevice:
		// Without a removal record, the agent keeps its data: the control
		// plane keeps a removal tombstone for up to 1 year, so an unknown
		// device is an error that an operator resolves with reset.
		a.log.Error("the control plane does not know this device; keeping local data (run `ownsand-agent reset` to delete it)")
		a.setConn(state.ConnectionDisconnected, "unknown device")
		return "", errors.New("unknown device")
	}
	a.log.Info("agent authenticated", "credential_expire_time", sess.ExpireTime.UTC().Format(time.RFC3339))
	return sess.Credential, nil
}

// removal deletes every local sandbox, snapshot and image, records the
// clean-up, confirms the removal and deletes the device key. It returns an
// error wrapping channel.ErrStop when it finished.
func (a *Agent) removal(ctx context.Context) error {
	rem, err := a.cfg.Dir.LoadRemoval()
	if err != nil {
		return err
	}
	if rem == nil {
		rem = &state.Removal{NoticeReceivedAt: time.Now()}
	}
	if !rem.CleanedUp {
		a.log.Info("device removal: deleting local sandboxes, snapshots and images")
		if err := a.Mgr.DeleteAll(ctx); err != nil {
			return fmt.Errorf("removal clean-up: %w", err)
		}
		rem.CleanedUp = true
		if err := a.cfg.Dir.SaveRemoval(rem); err != nil {
			return err
		}
	}
	backoff := time.Second
	for {
		err := a.cp.ConfirmRemoval(ctx, a.cfg.Identity.DeviceID, a.cfg.Identity.PrivateKey)
		if err == nil {
			break
		}
		a.log.Warn("confirm removal failed; retrying", "error", err, "retry_in", backoff.String())
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, time.Minute)
	}
	a.log.Info("device removal confirmed; deleting the device key")
	if err := a.cfg.Dir.Reset(); err != nil {
		return fmt.Errorf("delete local state: %w", err)
	}
	a.setConnNoSave(state.ConnectionRemoved, "")
	if a.stop != nil {
		a.stop(ErrRemoved)
	}
	return fmt.Errorf("%w: %w", channel.ErrStop, ErrRemoved)
}

// Connected implements channel.Handler.
func (a *Agent) Connected(gen uint64, send channel.Sender) {
	a.mu.Lock()
	a.send, a.gen = send, gen
	a.lastConnect = time.Now()
	a.mu.Unlock()
	a.setConn(state.ConnectionConnected, "")
	a.dp.Connected(gen, send)
	caps := a.capabilities(context.Background())
	send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_CapabilityReport{CapabilityReport: caps}})
	a.mu.Lock()
	a.lastCaps, a.lastCapsAt = caps, time.Now()
	a.mu.Unlock()
	rep := a.Mgr.StateReport()
	us := a.upd.Status()
	rep.AgentVersion = a.cfg.Version
	rep.UpdateState = us.GetUpdateState()
	rep.BuiltBaseImages = caps.GetBuiltBaseImages()
	send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_StateReport{StateReport: rep}})
	a.log.Info("state report sent", "report_id", rep.GetReportId(), "sandboxes", len(rep.GetSandboxes()), "snapshots", len(rep.GetSnapshots()))
	for _, p := range a.Mgr.PendingUpdates() {
		reporter{a}.SandboxUpdate(p)
	}
	send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_UpdateStatus{UpdateStatus: us}})
}

// Disconnected implements channel.Handler.
func (a *Agent) Disconnected(gen uint64, err error) {
	a.mu.Lock()
	if a.gen == gen {
		a.send = nil
	}
	a.mu.Unlock()
	a.dp.Disconnected(gen)
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	a.setConn(state.ConnectionDisconnected, msg)
}

// Receive implements channel.Handler.
func (a *Agent) Receive(gen uint64, env *ownsandv1.AgentEnvelope) {
	switch m := env.GetMessage().(type) {
	case *ownsandv1.AgentEnvelope_Configuration:
		c := m.Configuration
		if c.GetLimits() != nil {
			a.Mgr.SetLimits(c.GetLimits())
			a.mu.Lock()
			a.limits = c.GetLimits()
			a.mu.Unlock()
		}
		a.dp.SetIceServers(c.GetIceServers())
		a.log.Info("configuration received", "target_version", c.GetTargetVersion(), "revocation_recovery", c.GetRevocationRecovery(), "ice_servers", len(c.GetIceServers()))
		a.upd.SetTarget(context.Background(), c.GetTargetVersion(), c.GetRevocationRecovery(), c.GetReleaseManifestUrl())
	case *ownsandv1.AgentEnvelope_Command:
		a.handleCommand(m.Command)
	case *ownsandv1.AgentEnvelope_StateAck:
		switch t := m.StateAck.GetTarget().(type) {
		case *ownsandv1.StateAck_ReportId:
			a.Mgr.AckReport(t.ReportId)
		case *ownsandv1.StateAck_Sandbox:
			a.Mgr.AckSandbox(t.Sandbox.GetSandboxId(), t.Sandbox.GetRevision())
		}
	case *ownsandv1.AgentEnvelope_SignalingOffer:
		a.dp.HandleOffer(gen, m.SignalingOffer)
	case *ownsandv1.AgentEnvelope_RelayGrant:
		a.dp.HandleRelayGrant(gen, m.RelayGrant)
	case *ownsandv1.AgentEnvelope_SessionRevocation:
		a.dp.HandleRevocation(gen, m.SessionRevocation)
	case *ownsandv1.AgentEnvelope_SignedUrlGrant:
		a.log.Warn("ignoring a signed URL grant: snapshot export and import are not implemented")
	default:
		a.log.Warn("ignoring an unexpected agent channel message", "type", fmt.Sprintf("%T", env.GetMessage()))
	}
}

func (a *Agent) handleCommand(cmd *ownsandv1.AgentCommand) {
	id := cmd.GetCommandId()
	reply := func(res *ownsandv1.AgentCommandResult) {
		a.log.Info("command result", "command_id", id, "error_code", res.GetErrorCode())
		a.sendEnvelope(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_CommandResult{CommandResult: res}})
	}
	a.log.Info("command received", "command_id", id, "type", fmt.Sprintf("%T", cmd.GetCommand()))
	if a.Mgr.Handle(cmd, reply) {
		return
	}
	switch c := cmd.GetCommand().(type) {
	case *ownsandv1.AgentCommand_CleanUp:
		go func() {
			if err := a.cfg.Dir.SaveRemoval(&state.Removal{NoticeReceivedAt: time.Now()}); err != nil {
				reply(sandbox.Result(id, err))
				return
			}
			if err := a.Mgr.DeleteAll(context.Background()); err != nil {
				reply(sandbox.Result(id, err))
				return
			}
			_ = a.cfg.Dir.SaveRemoval(&state.Removal{NoticeReceivedAt: time.Now(), CleanedUp: true})
			reply(sandbox.Result(id, nil))
			go func() {
				ctx := context.Background()
				if err := a.removal(ctx); err != nil && !errors.Is(err, channel.ErrStop) {
					a.log.Error("removal failed", "error", err)
				}
			}()
		}()
	case *ownsandv1.AgentCommand_Update:
		a.upd.SetTarget(context.Background(), c.Update.GetTargetVersion(), c.Update.GetRevocationRecovery(), "")
		reply(sandbox.Result(id, nil))
	default:
		reply(sandbox.Result(id, connect.NewError(connect.CodeUnimplemented, errors.New("unknown command"))))
	}
}

// capabilities builds the capability report.
func (a *Agent) capabilities(ctx context.Context) *ownsandv1.DeviceCapabilities {
	rc := a.cfg.Runtime.Capabilities(ctx)
	return &ownsandv1.DeviceCapabilities{
		HostOs:               hostinfo.HostOS(),
		Arch:                 hostinfo.Arch(),
		AgentVersion:         a.cfg.Version,
		LogicalCpuCount:      hostinfo.LogicalCPUs(),
		MemoryMib:            hostinfo.MemoryMiB(),
		FreeDiskBytes:        hostinfo.FreeDiskBytes(a.cfg.Dir.Path),
		GuestOses:            rc.GuestOSes,
		BuiltBaseImages:      rc.BuiltBaseImages,
		MacosGuestsSupported: rc.MacOSSupported,
		MacosHardwareModels:  rc.MacOSHardwareModel,
		HypervisorChecks:     rc.HypervisorChecks,
	}
}

// periodic reports capabilities, ticks the updater and writes the status
// file.
func (a *Agent) periodic(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		a.upd.Tick()
		a.maybeReportCapabilities(ctx)
		a.saveStatus()
	}
}

func (a *Agent) maybeReportCapabilities(ctx context.Context) {
	caps := a.capabilities(ctx)
	a.mu.Lock()
	last, at, connected := a.lastCaps, a.lastCapsAt, a.send != nil
	a.mu.Unlock()
	if !connected {
		return
	}
	changed := last == nil ||
		!slices.Equal(last.GetBuiltBaseImages(), caps.GetBuiltBaseImages()) ||
		!slices.Equal(last.GetGuestOses(), caps.GetGuestOses()) ||
		!mapsEqual(last.GetHypervisorChecks(), caps.GetHypervisorChecks())
	diskMoved := last != nil && abs(last.GetFreeDiskBytes()-caps.GetFreeDiskBytes()) > diskReportDelta
	if !changed && !diskMoved && time.Since(at) < capabilityInterval {
		return
	}
	if a.sendEnvelope(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_CapabilityReport{CapabilityReport: caps}}) {
		a.mu.Lock()
		a.lastCaps, a.lastCapsAt = caps, time.Now()
		a.mu.Unlock()
		a.log.Debug("capability report sent", "free_disk_bytes", caps.GetFreeDiskBytes())
	}
}

func abs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func (a *Agent) setConn(s state.ConnectionState, errMsg string) {
	a.setConnNoSave(s, errMsg)
	a.saveStatus()
}

func (a *Agent) setConnNoSave(s state.ConnectionState, errMsg string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.conn != s {
		a.log.Info("agent channel state", "state", string(s))
	}
	a.conn = s
	if errMsg != "" {
		a.lastErr = errMsg
	}
}

func (a *Agent) saveStatus() {
	a.mu.Lock()
	if a.conn == state.ConnectionRemoved {
		a.mu.Unlock()
		return
	}
	us := a.upd.Status()
	st := &state.Status{
		UpdatedAt:       time.Now(),
		Connection:      a.conn,
		LastConnectedAt: a.lastConnect,
		LastError:       a.lastErr,
		AgentVersion:    a.cfg.Version,
		UpdateState:     us.GetUpdateState(),
		TargetVersion:   us.GetTargetVersion(),
		Limits:          a.limits,
	}
	caps := a.lastCaps
	a.mu.Unlock()
	if caps != nil {
		st.Capabilities = &state.CapabilitySummary{
			HostOS:         caps.GetHostOs().String(),
			Arch:           caps.GetArch().String(),
			LogicalCPUs:    caps.GetLogicalCpuCount(),
			MemoryMiB:      caps.GetMemoryMib(),
			FreeDiskBytes:  caps.GetFreeDiskBytes(),
			MacOSSupported: caps.GetMacosGuestsSupported(),
		}
		st.HypervisorChecks = caps.GetHypervisorChecks()
		st.BuiltBaseImages = caps.GetBuiltBaseImages()
		st.GuestOSes = caps.GetGuestOses()
	}
	st.SandboxStates = a.Mgr.Counts()
	if err := a.cfg.Dir.SaveStatus(st); err != nil && !errors.Is(err, os.ErrNotExist) {
		a.log.Warn("write status file failed", "error", err)
	}
}

// rejectingDataPath is the data path before the data path layer exists: it
// answers offers with unimplemented, leaves relay grants unacknowledged (the
// control plane then fails AuthorizeRelay with unavailable) and acknowledges
// revocations, which the contract requires also for ended sessions.
type rejectingDataPath struct{ a *Agent }

func (rejectingDataPath) Connected(uint64, channel.Sender)     {}
func (rejectingDataPath) Disconnected(uint64)                  {}
func (rejectingDataPath) SetIceServers([]*ownsandv1.IceServer) {}
func (r rejectingDataPath) HandleOffer(_ uint64, o *ownsandv1.SignalingOffer) {
	r.a.sendEnvelope(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_SignalingAnswer{SignalingAnswer: &ownsandv1.SignalingAnswer{
		SessionId:    o.GetSessionId(),
		ErrorCode:    connect.CodeUnimplemented.String(),
		ErrorMessage: "the peer-to-peer data path is not implemented by this agent",
	}}})
}
func (r rejectingDataPath) HandleRelayGrant(_ uint64, g *ownsandv1.RelayGrant) {
	r.a.log.Warn("ignoring a relay grant: the relay data path is not implemented", "grant_id", g.GetGrantId())
}
func (r rejectingDataPath) HandleRevocation(_ uint64, rev *ownsandv1.SessionRevocation) {
	r.a.sendEnvelope(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_RevocationAck{RevocationAck: &ownsandv1.RevocationAck{RevocationId: rev.GetRevocationId()}}})
}
func (rejectingDataPath) Close() {}
