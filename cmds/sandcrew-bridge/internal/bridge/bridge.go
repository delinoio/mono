// SPDX-License-Identifier: Apache-2.0

// Package bridge runs one SandCrew session in the guest: it starts the
// pinned vendor agent, speaks ACP with it, applies the inputs of the inbox
// directory and writes normalized events to the guest event log and stdout.
// The owning contract is docs/cmds-sandcrew-bridge-foundation.md.
package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/delinoio/mono/cmds/sandcrew-bridge/internal/acp"
	"github.com/delinoio/mono/cmds/sandcrew-bridge/internal/agent"
	"github.com/delinoio/mono/cmds/sandcrew-bridge/internal/credwatch"
	"github.com/delinoio/mono/cmds/sandcrew-bridge/internal/eventlog"
	"github.com/delinoio/mono/cmds/sandcrew-bridge/internal/inbox"
	"github.com/delinoio/mono/cmds/sandcrew-bridge/internal/install"
	"github.com/delinoio/mono/cmds/sandcrew-bridge/internal/normalize"
	"github.com/delinoio/mono/cmds/sandcrew-bridge/internal/pins"
	"github.com/delinoio/mono/cmds/sandcrew-bridge/internal/vendor"
	"github.com/delinoio/mono/go/uuidv7"
	sandcrewv1 "github.com/delinoio/mono/protos/delino/sandcrew/v1"
)

// Protocol version of the bridge event and input schema. The protos
// contract owns the major and minor rules.
const (
	ProtocolMajor = 1
	ProtocolMinor = 0
)

// ExitCode is the exit code of the bridge process.
type ExitCode int

// Exit codes.
const (
	// ExitAgentExited: the agent process ended (agent_exited was written).
	ExitAgentExited ExitCode = 0
	// ExitUsage: invalid arguments or an unusable state directory.
	ExitUsage ExitCode = 1
	// ExitStartFailed: the start failed before the first IDLE.
	ExitStartFailed ExitCode = 2
	// ExitEventLogFailed: the event log could not be appended.
	ExitEventLogFailed ExitCode = 3
)

// File names in the state directory.
const (
	StartFileName  = "start.json"
	InboxDirName   = "inbox"
	EventsFileName = "events.jsonl"
	AgentsDirName  = "agents"
)

// Timeouts of agent calls.
const (
	startTimeout = 5 * time.Minute
	callTimeout  = time.Minute
	stopTimeout  = 15 * time.Second
)

// Launch describes the agent process to start.
type Launch struct {
	Name         string
	Args         []string
	Dir          string
	Env          []string
	AuthMethodID string
	Handler      agent.Handler
	Logger       *slog.Logger
	ClientInfo   acp.Implementation
	Stderr       io.Writer
}

// Config configures Run.
type Config struct {
	// StateDir is `<workspace>/.sandcrew`.
	StateDir string
	// Workspace is the working directory of the agent. Defaults to the
	// parent of StateDir.
	Workspace string
	// Environ is the bridge environment. Defaults to os.Environ.
	Environ []string
	Stdout  io.Writer
	// AgentStderr receives the stderr of the agent process. Defaults to
	// io.Discard, because it can contain agent output.
	AgentStderr io.Writer
	Logger      *slog.Logger
	// BridgeVersion is the version of this bridge build.
	BridgeVersion string
	GuestOS       sandcrewv1.GuestOs
	Target        pins.Target
	Pins          *pins.Pins
	// Install installs the agent components. Defaults to an
	// install.Installer in `<state>/agents`.
	Install func(ctx context.Context, vendor sandcrewv1.AgentVendor) (install.Result, error)
	// StartAgent starts the agent process. Defaults to a child process
	// with the ACP implementation.
	StartAgent func(ctx context.Context, launch Launch) (agent.Agent, error)
	// PollInterval is the inbox poll interval when file system
	// notifications are not available. Defaults to inbox.PollInterval.
	PollInterval time.Duration
	// FallbackPollInterval is the inbox poll interval while file system
	// notifications work. Defaults to inbox.FallbackPollInterval.
	FallbackPollInterval time.Duration
	// CredentialInterval is the credential file check interval. Defaults
	// to credwatch.Interval.
	CredentialInterval time.Duration
	// Now returns the event time. Defaults to time.Now.
	Now func() time.Time
	// NewID returns a new UUID v7 for permission requests. Defaults to
	// uuidv7.NewString.
	NewID func() string
	// OpenEventLog opens the event log file for appending. Defaults to the
	// eventlog default (tests inject failures).
	OpenEventLog func(path string) (eventlog.File, error)
}

func (c *Config) defaults() error {
	if c.StateDir == "" {
		return errors.New("bridge: no state directory")
	}
	if c.Workspace == "" {
		c.Workspace = filepath.Dir(c.StateDir)
	}
	if c.Environ == nil {
		c.Environ = os.Environ()
	}
	if c.Stdout == nil {
		c.Stdout = os.Stdout
	}
	if c.AgentStderr == nil {
		c.AgentStderr = io.Discard
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.DiscardHandler)
	}
	if c.Pins == nil {
		p, err := pins.Load()
		if err != nil {
			return err
		}
		c.Pins = p
	}
	if c.Install == nil {
		installer := &install.Installer{
			AgentsDir: filepath.Join(c.StateDir, AgentsDirName),
			Target:    c.Target,
			Pins:      c.Pins,
			Logger:    c.Logger,
		}
		c.Install = installer.Install
	}
	if c.StartAgent == nil {
		c.StartAgent = startProcessAgent
	}
	if c.PollInterval == 0 {
		c.PollInterval = inbox.PollInterval
	}
	if c.FallbackPollInterval == 0 {
		c.FallbackPollInterval = inbox.FallbackPollInterval
	}
	if c.CredentialInterval == 0 {
		c.CredentialInterval = credwatch.Interval
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.NewID == nil {
		c.NewID = func() string {
			id, err := uuidv7.NewString()
			if err != nil {
				panic(err)
			}
			return id
		}
	}
	return nil
}

func startProcessAgent(_ context.Context, launch Launch) (agent.Agent, error) {
	process, err := agent.ExecProcess(launch.Name, launch.Args, launch.Dir, launch.Env, launch.Stderr)
	if err != nil {
		return nil, err
	}
	return agent.NewACPAgent(process, agent.ACPOptions{
		Handler:      launch.Handler,
		Logger:       launch.Logger,
		ClientInfo:   launch.ClientInfo,
		AuthMethodID: launch.AuthMethodID,
	}), nil
}

// pendingPermission is an ASK-mode permission request that waits for an
// answer.
type pendingPermission struct {
	options []acp.PermissionOption
	answer  chan acp.PermissionOutcome
}

type turnResult struct {
	stopReason string
	err        error
}

// Bridge is the state of one bridge process.
type Bridge struct {
	cfg    Config
	logger *slog.Logger
	log    *eventlog.Log
	inbox  *inbox.Inbox
	spec   vendor.Spec
	start  *sandcrewv1.BridgeStartConfig
	agent  agent.Agent

	ctx      context.Context
	turnDone chan turnResult
	fatal    chan struct{}
	failOnce sync.Once

	mu             sync.Mutex
	activity       sandcrewv1.AgentActivity
	turnActive     bool
	queue          []string
	pending        map[string]*pendingPermission
	permissionMode sandcrewv1.PermissionMode
	currentModeID  string
	availableModes []acp.SessionMode
	configOptions  []normalize.ConfigOption
}

// Run runs the bridge until the agent ends, the start fails or the event
// log fails, and returns the exit code. Canceling ctx stops the agent.
func Run(ctx context.Context, cfg Config) ExitCode {
	if err := cfg.defaults(); err != nil {
		cfg.Logger.Error("bridge configuration failed", "error", err)
		return ExitUsage
	}
	b := &Bridge{
		cfg:      cfg,
		logger:   cfg.Logger,
		turnDone: make(chan turnResult, 1),
		fatal:    make(chan struct{}),
		pending:  make(map[string]*pendingPermission),
	}
	return b.run(ctx)
}

func (b *Bridge) run(ctx context.Context) ExitCode {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	b.ctx = ctx

	log, err := eventlog.Open(filepath.Join(b.cfg.StateDir, EventsFileName), eventlog.Options{
		Stdout:   b.cfg.Stdout,
		Logger:   b.logger,
		Now:      b.cfg.Now,
		OpenFile: b.cfg.OpenEventLog,
	})
	if err != nil {
		b.logger.Error("event log open failed", "error", err)
		return ExitEventLogFailed
	}
	b.log = log
	defer log.Close()
	go log.RunSyncer(ctx)

	start, startErr := b.readStart()
	b.start = start
	pinned, _ := b.cfg.Pins.Vendor(start.GetVendor())
	b.logger = b.logger.With("session_id", start.GetSessionId(), "vendor", start.GetVendor().String(), "agent_version", pinned.Agent.Version)
	if !b.emit(&sandcrewv1.BridgeEvent{Event: &sandcrewv1.BridgeEvent_Hello{Hello: &sandcrewv1.BridgeHello{
		BridgeVersion:   b.cfg.BridgeVersion,
		ProtocolVersion: &sandcrewv1.ProtocolVersion{Major: ProtocolMajor, Minor: ProtocolMinor},
		Vendor:          start.GetVendor(),
		AgentVersion:    pinned.Agent.Version,
		AdapterVersion:  pinned.Adapter.Version,
		GuestOs:         b.cfg.GuestOS,
	}}}) {
		return ExitEventLogFailed
	}
	b.mu.Lock()
	b.setActivityLocked(sandcrewv1.AgentActivity_AGENT_ACTIVITY_STARTING)
	b.mu.Unlock()
	if b.failed() {
		return ExitEventLogFailed
	}

	if code, ok := b.startAgent(ctx, startErr); !ok {
		if b.agent != nil {
			b.stopAgent()
		}
		return code
	}
	defer b.stopAgent()

	if b.inbox, err = inbox.New(filepath.Join(b.cfg.StateDir, InboxDirName)); err != nil {
		b.logger.Error("inbox unusable", "error", err)
		b.emitError(sandcrewv1.BridgeErrorKind_BRIDGE_ERROR_KIND_AGENT_START_FAILED, "the inbox directory cannot be created")
		return ExitStartFailed
	}
	b.mu.Lock()
	b.setActivityLocked(sandcrewv1.AgentActivity_AGENT_ACTIVITY_IDLE)
	b.mu.Unlock()
	b.logger.Info("agent session ready")
	return b.loop(ctx)
}

// startAgent runs the start sequence up to the first IDLE. It writes the
// bridge_error of a failure and returns false with the exit code.
func (b *Bridge) startAgent(ctx context.Context, startErr error) (ExitCode, bool) {
	fail := func(kind sandcrewv1.BridgeErrorKind, message string) (ExitCode, bool) {
		b.logger.Error("start failed", "error_kind", kind.String(), "error", message)
		if !b.emitError(kind, message) {
			return ExitEventLogFailed, false
		}
		return ExitStartFailed, false
	}
	if startErr != nil {
		return fail(sandcrewv1.BridgeErrorKind_BRIDGE_ERROR_KIND_AGENT_START_FAILED, startErr.Error())
	}
	spec, ok := vendor.Lookup(b.start.GetVendor())
	if !ok {
		return fail(sandcrewv1.BridgeErrorKind_BRIDGE_ERROR_KIND_AGENT_START_FAILED, fmt.Sprintf("vendor %s is not supported", b.start.GetVendor()))
	}
	b.spec = spec
	credential := vendor.Credential{Kind: b.start.GetCredentialKind(), Value: envValue(b.cfg.Environ, vendor.EnvVendorCredential)}
	if _, err := os.Stat(vendor.CodexAuthFile(b.cfg.StateDir)); err == nil {
		credential.CodexAuthFileExists = true
	}
	launch, err := spec.Prepare(b.cfg.Environ, credential, b.cfg.StateDir)
	if err != nil {
		return fail(sandcrewv1.BridgeErrorKind_BRIDGE_ERROR_KIND_CREDENTIAL_MISSING, err.Error())
	}

	// The credential baseline is read before the agent starts, so a
	// refresh during initialize or session/new is a change.
	var credWatcher *credwatch.Watcher
	var credWatchErr error
	if launch.CredentialFile != "" {
		credWatcher, credWatchErr = credwatch.New(launch.CredentialFile, b.cfg.CredentialInterval)
	}

	installed, err := b.cfg.Install(ctx, b.start.GetVendor())
	if err != nil {
		kind := sandcrewv1.BridgeErrorKind_BRIDGE_ERROR_KIND_AGENT_INSTALL_FAILED
		if installErr, ok := install.IsError(err); ok {
			kind = installErr.Kind
		}
		return fail(kind, err.Error())
	}

	a, err := b.cfg.StartAgent(ctx, Launch{
		Name:         installed.RuntimePath,
		Args:         []string{installed.AdapterEntry},
		Dir:          b.cfg.Workspace,
		Env:          launch.Env,
		AuthMethodID: launch.AuthMethodID,
		Handler:      b,
		Logger:       b.logger,
		ClientInfo:   acp.Implementation{Name: "sandcrew-bridge", Title: "SandCrew", Version: b.cfg.BridgeVersion},
		Stderr:       b.cfg.AgentStderr,
	})
	if err != nil {
		return fail(sandcrewv1.BridgeErrorKind_BRIDGE_ERROR_KIND_AGENT_START_FAILED, fmt.Sprintf("the agent process did not start: %v", err))
	}
	b.agent = a
	startCtx, cancel := context.WithTimeout(ctx, startTimeout)
	info, err := a.Start(startCtx, b.cfg.Workspace)
	cancel()
	if err != nil {
		return fail(sandcrewv1.BridgeErrorKind_BRIDGE_ERROR_KIND_AGENT_START_FAILED, startErrorMessage(err))
	}
	if !b.emit(&sandcrewv1.BridgeEvent{Event: &sandcrewv1.BridgeEvent_AgentReady{AgentReady: &sandcrewv1.AgentReady{
		AgentCapabilities:    &sandcrewv1.AgentCapabilities{LoadSession: info.LoadSession, ResumeSession: info.ResumeSession},
		AgentSessionId:       info.SessionID,
		AgentName:            info.AgentName,
		AgentReportedVersion: info.AgentVersion,
	}}}) {
		return ExitEventLogFailed, false
	}
	b.logger.Info("agent started", "runtime_preinstalled", installed.RuntimePreinstalled, "adapter_preinstalled", installed.AdapterPreinstalled, "load_session", info.LoadSession, "resume_session", info.ResumeSession)

	options, err := normalize.ConfigOptions(info.ConfigOptions)
	if err != nil {
		b.ProtocolError(err)
	}
	b.mu.Lock()
	if info.Modes != nil {
		b.currentModeID = info.Modes.CurrentModeID
		b.availableModes = info.Modes.AvailableModes
	}
	b.configOptions = options
	b.permissionMode = b.start.GetPermissionMode()
	if b.permissionMode == sandcrewv1.PermissionMode_PERMISSION_MODE_UNSPECIFIED {
		b.permissionMode = sandcrewv1.PermissionMode_PERMISSION_MODE_AUTO_APPROVE
	}
	if len(options) > 0 {
		b.emitConfigOptionsLocked()
	}
	b.mu.Unlock()

	// Apply the start configuration: the permission mode first, then the
	// requested agent mode and the configuration options. A rejected value
	// writes OPTION_REJECTED and the session continues with the default.
	b.applyPermissionMode(b.permissionMode, false)
	if modeID := b.start.GetModeId(); modeID != "" {
		b.applyModeID(modeID)
	}
	for _, option := range b.start.GetConfigOptions() {
		b.applyConfigOption(option.GetConfigOptionId(), option.GetValueId())
	}
	if launch.CredentialFile != "" {
		b.watchCredential(ctx, credWatcher, credWatchErr)
	}
	if b.failed() {
		return ExitEventLogFailed, false
	}
	return 0, true
}

func (b *Bridge) readStart() (*sandcrewv1.BridgeStartConfig, error) {
	data, err := os.ReadFile(filepath.Join(b.cfg.StateDir, StartFileName))
	if err != nil {
		return &sandcrewv1.BridgeStartConfig{}, fmt.Errorf("%s cannot be read", StartFileName)
	}
	var start sandcrewv1.BridgeStartConfig
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(data, &start); err != nil {
		return &sandcrewv1.BridgeStartConfig{}, fmt.Errorf("%s does not parse", StartFileName)
	}
	return &start, nil
}

func (b *Bridge) loop(ctx context.Context) ExitCode {
	// Notifications start before the first read of the inbox, so a file
	// that arrives between them is not missed.
	interval := b.cfg.PollInterval
	var notified <-chan struct{}
	if notifier, err := b.inbox.Notify(); err != nil {
		b.logger.Warn("inbox notifications are not available; polling only", "error", err, "poll_interval", interval.String())
	} else {
		defer notifier.Close()
		notified = notifier.C()
		interval = b.cfg.FallbackPollInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	b.processInbox()
	for {
		select {
		case <-b.fatal:
			return ExitEventLogFailed
		case <-ctx.Done():
			b.logger.Info("stopping the agent")
			b.stopAgent()
			select {
			case <-b.agent.Done():
			case <-time.After(stopTimeout):
				b.logger.Warn("the agent did not stop in time")
				return ExitAgentExited
			}
			return b.agentExited()
		case <-b.agent.Done():
			return b.agentExited()
		case result := <-b.turnDone:
			b.finishTurn(result)
		case <-notified:
			b.processInbox()
		case <-ticker.C:
			b.processInbox()
		}
		if b.failed() {
			return ExitEventLogFailed
		}
	}
}

func (b *Bridge) agentExited() ExitCode {
	exit := b.agent.Exit()
	event := &sandcrewv1.AgentExited{}
	if exit.Signal != "" {
		event.Result = &sandcrewv1.AgentExited_Signal{Signal: exit.Signal}
	} else {
		event.Result = &sandcrewv1.AgentExited_ExitCode{ExitCode: int32(exit.Code)}
	}
	b.logger.Info("agent exited", "exit_code", exit.Code, "signal", exit.Signal)
	if !b.emit(&sandcrewv1.BridgeEvent{Event: &sandcrewv1.BridgeEvent_AgentExited{AgentExited: event}}) {
		return ExitEventLogFailed
	}
	return ExitAgentExited
}

func (b *Bridge) stopAgent() {
	if b.agent != nil {
		b.agent.Stop()
	}
}

// emit appends event to the log and stdout. It returns false and stops the
// bridge when the log failed.
func (b *Bridge) emit(event *sandcrewv1.BridgeEvent) bool {
	if err := b.log.Append(event); err != nil {
		b.failOnce.Do(func() {
			b.logger.Error("event log failed; stopping", "error", err)
			close(b.fatal)
			b.stopAgent()
		})
		return false
	}
	b.logger.Debug("event", "event_kind", eventKind(event), "sequence", event.GetSequence())
	return true
}

func (b *Bridge) failed() bool {
	select {
	case <-b.fatal:
		return true
	default:
		return false
	}
}

func (b *Bridge) emitError(kind sandcrewv1.BridgeErrorKind, message string) bool {
	return b.emit(&sandcrewv1.BridgeEvent{Event: &sandcrewv1.BridgeEvent_BridgeError{BridgeError: &sandcrewv1.BridgeError{Kind: kind, Message: message}}})
}

func (b *Bridge) setActivityLocked(activity sandcrewv1.AgentActivity) {
	if b.activity == activity {
		return
	}
	b.activity = activity
	b.emit(&sandcrewv1.BridgeEvent{Event: &sandcrewv1.BridgeEvent_SessionState{SessionState: &sandcrewv1.AgentSessionState{State: activity}}})
}

func (b *Bridge) emitModeChangedLocked() {
	b.emit(&sandcrewv1.BridgeEvent{Event: &sandcrewv1.BridgeEvent_ModeChanged{ModeChanged: &sandcrewv1.ModeChanged{
		CurrentModeId:  b.currentModeID,
		AvailableModes: normalize.Modes(b.availableModes),
		PermissionMode: b.permissionMode,
	}}})
}

func (b *Bridge) emitConfigOptionsLocked() {
	options := make([]*sandcrewv1.ConfigOption, 0, len(b.configOptions))
	for _, option := range b.configOptions {
		options = append(options, option.Option)
	}
	b.emit(&sandcrewv1.BridgeEvent{Event: &sandcrewv1.BridgeEvent_ConfigOptions{ConfigOptions: &sandcrewv1.ConfigOptions{Options: options}}})
}

// processInbox applies every inbox file in input_id order.
func (b *Bridge) processInbox() {
	items, err := b.inbox.List()
	if err != nil {
		b.logger.Warn("inbox list failed", "error", err)
		return
	}
	for _, item := range items {
		if b.failed() {
			return
		}
		if b.log.HasInput(item.InputID) {
			// A repeated write of an input that the log already holds.
			b.logger.Info("deleting a repeated input", "input_id", item.InputID)
			b.remove(item)
			continue
		}
		item = inbox.Read(item)
		if item.Err != nil {
			b.logger.Warn("malformed input", "input_id", item.InputID, "error", item.Err)
			b.reject(item.InputID, sandcrewv1.InputRejectionReason_INPUT_REJECTION_REASON_MALFORMED)
		} else {
			b.apply(item.Input)
		}
		if b.failed() {
			// The acceptance or rejection may be missing from the log. The
			// file stays, so the next bridge process handles the input; one
			// that the log holds is deleted there without a new effect.
			b.logger.Warn("keeping the inbox file after an event log failure", "input_id", item.InputID)
			return
		}
		b.remove(item)
	}
}

func (b *Bridge) remove(item inbox.Item) {
	if err := inbox.Remove(item); err != nil {
		b.logger.Warn("inbox file removal failed", "input_id", item.InputID, "error", err)
	}
}

func (b *Bridge) accept(id string) bool {
	b.logger.Info("input accepted", "input_id", id)
	return b.emit(&sandcrewv1.BridgeEvent{Event: &sandcrewv1.BridgeEvent_InputAccepted{InputAccepted: &sandcrewv1.InputAccepted{InputId: id}}})
}

func (b *Bridge) reject(id string, reason sandcrewv1.InputRejectionReason) {
	b.logger.Info("input rejected", "input_id", id, "reason", reason.String())
	b.emit(&sandcrewv1.BridgeEvent{Event: &sandcrewv1.BridgeEvent_InputRejected{InputRejected: &sandcrewv1.InputRejected{InputId: id, Reason: reason}}})
}

// apply validates one input, writes input_accepted or input_rejected and
// then applies the accepted input. The acceptance is logged before the
// effect, so a restarted bridge never applies an input twice.
func (b *Bridge) apply(input *sandcrewv1.BridgeInput) {
	id := input.GetInputId()
	malformed := sandcrewv1.InputRejectionReason_INPUT_REJECTION_REASON_MALFORMED
	switch in := input.GetInput().(type) {
	case *sandcrewv1.BridgeInput_Prompt:
		if in.Prompt.GetText() == "" {
			b.reject(id, malformed)
			return
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		if !b.accept(id) {
			return
		}
		if b.turnActive {
			b.queue = append(b.queue, in.Prompt.GetText())
			return
		}
		b.startTurnLocked(in.Prompt.GetText())
	case *sandcrewv1.BridgeInput_PermissionAnswer:
		b.answerPermission(id, in.PermissionAnswer)
	case *sandcrewv1.BridgeInput_CancelTurn:
		b.cancelTurn(id)
	case *sandcrewv1.BridgeInput_SetMode:
		switch target := in.SetMode.GetTarget().(type) {
		case *sandcrewv1.SetModeInput_ModeId:
			if target.ModeId == "" {
				b.reject(id, malformed)
				return
			}
			if b.accept(id) {
				b.applyModeID(target.ModeId)
			}
		case *sandcrewv1.SetModeInput_PermissionMode:
			if target.PermissionMode != sandcrewv1.PermissionMode_PERMISSION_MODE_AUTO_APPROVE && target.PermissionMode != sandcrewv1.PermissionMode_PERMISSION_MODE_ASK {
				b.reject(id, malformed)
				return
			}
			if b.accept(id) {
				b.applyPermissionMode(target.PermissionMode, true)
			}
		default:
			b.reject(id, malformed)
		}
	case *sandcrewv1.BridgeInput_SetConfigOption:
		option := in.SetConfigOption
		if option.GetConfigOptionId() == "" || option.GetValueId() == "" {
			b.reject(id, malformed)
			return
		}
		if b.accept(id) {
			b.applyConfigOption(option.GetConfigOptionId(), option.GetValueId())
		}
	default:
		// An input case of a later protocol version.
		b.reject(id, sandcrewv1.InputRejectionReason_INPUT_REJECTION_REASON_UNSUPPORTED)
	}
}

func (b *Bridge) startTurnLocked(text string) {
	b.emit(&sandcrewv1.BridgeEvent{Event: &sandcrewv1.BridgeEvent_UserMessageChunk{UserMessageChunk: &sandcrewv1.ContentChunk{
		Content: &sandcrewv1.ContentBlock{Block: &sandcrewv1.ContentBlock_Text{Text: text}},
	}}})
	b.turnActive = true
	b.setActivityLocked(sandcrewv1.AgentActivity_AGENT_ACTIVITY_WORKING)
	go func() {
		stopReason, err := b.agent.Prompt(b.ctx, text)
		b.turnDone <- turnResult{stopReason: stopReason, err: err}
	}()
}

func (b *Bridge) finishTurn(result turnResult) {
	b.mu.Lock()
	defer b.mu.Unlock()
	stopReason := normalize.StopReason(result.stopReason)
	if result.err != nil && !errors.Is(result.err, acp.ErrClosed) && !errors.Is(result.err, context.Canceled) {
		b.logger.Warn("session/prompt failed", "error_kind", "ACP_PROTOCOL_ERROR", "error", rpcSummary(result.err))
		b.emitError(sandcrewv1.BridgeErrorKind_BRIDGE_ERROR_KIND_ACP_PROTOCOL_ERROR, "session/prompt failed: "+rpcSummary(result.err))
	}
	// A request that is still pending belongs to the ending turn.
	b.cancelPendingLocked()
	b.emit(&sandcrewv1.BridgeEvent{Event: &sandcrewv1.BridgeEvent_TurnEnded{TurnEnded: &sandcrewv1.TurnEnded{StopReason: stopReason}}})
	b.logger.Info("turn ended", "stop_reason", stopReason.String())
	b.turnActive = false
	if len(b.queue) > 0 {
		next := b.queue[0]
		b.queue = b.queue[1:]
		b.startTurnLocked(next)
		return
	}
	b.setActivityLocked(sandcrewv1.AgentActivity_AGENT_ACTIVITY_IDLE)
}

func (b *Bridge) answerPermission(id string, answer *sandcrewv1.PermissionAnswerInput) {
	b.mu.Lock()
	defer b.mu.Unlock()
	pending, ok := b.pending[answer.GetPermissionRequestId()]
	if !ok {
		b.reject(id, sandcrewv1.InputRejectionReason_INPUT_REJECTION_REASON_UNKNOWN_PERMISSION_REQUEST)
		return
	}
	if !slices.ContainsFunc(pending.options, func(o acp.PermissionOption) bool { return o.OptionID == answer.GetOptionId() }) {
		b.reject(id, sandcrewv1.InputRejectionReason_INPUT_REJECTION_REASON_UNKNOWN_OPTION)
		return
	}
	if !b.accept(id) {
		return
	}
	b.resolveLocked(answer.GetPermissionRequestId(), acp.PermissionOutcome{Outcome: acp.OutcomeSelected, OptionID: answer.GetOptionId()}, false)
}

func (b *Bridge) cancelTurn(id string) {
	b.mu.Lock()
	if !b.turnActive {
		b.mu.Unlock()
		b.reject(id, sandcrewv1.InputRejectionReason_INPUT_REJECTION_REASON_NO_ACTIVE_TURN)
		return
	}
	if !b.accept(id) {
		b.mu.Unlock()
		return
	}
	b.queue = nil
	b.cancelPendingLocked()
	b.mu.Unlock()
	if err := b.agent.Cancel(); err != nil {
		b.logger.Warn("session/cancel failed", "error", err)
	}
}

// cancelPendingLocked answers every pending permission request with the
// outcome cancelled.
func (b *Bridge) cancelPendingLocked() {
	ids := make([]string, 0, len(b.pending))
	for requestID := range b.pending {
		ids = append(ids, requestID)
	}
	slices.Sort(ids)
	for _, requestID := range ids {
		b.resolveLocked(requestID, acp.PermissionOutcome{Outcome: acp.OutcomeCancelled}, false)
	}
}

// resolveLocked answers a pending permission request and writes its
// permission_response.
func (b *Bridge) resolveLocked(requestID string, outcome acp.PermissionOutcome, autoApproved bool) {
	pending, ok := b.pending[requestID]
	if !ok {
		return
	}
	delete(b.pending, requestID)
	b.emit(permissionResponse(requestID, outcome, autoApproved))
	pending.answer <- outcome
	if len(b.pending) == 0 && b.activity == sandcrewv1.AgentActivity_AGENT_ACTIVITY_WAITING_FOR_PERMISSION {
		b.setActivityLocked(sandcrewv1.AgentActivity_AGENT_ACTIVITY_WORKING)
	}
}

func permissionResponse(requestID string, outcome acp.PermissionOutcome, autoApproved bool) *sandcrewv1.BridgeEvent {
	response := &sandcrewv1.PermissionResponse{PermissionRequestId: requestID, AutoApproved: autoApproved}
	if outcome.Outcome == acp.OutcomeSelected {
		response.Outcome = sandcrewv1.PermissionOutcome_PERMISSION_OUTCOME_SELECTED
		response.OptionId = outcome.OptionID
	} else {
		response.Outcome = sandcrewv1.PermissionOutcome_PERMISSION_OUTCOME_CANCELLED
	}
	return &sandcrewv1.BridgeEvent{Event: &sandcrewv1.BridgeEvent_PermissionResponse{PermissionResponse: response}}
}

// allowOption returns the first ALLOW_ONCE option, otherwise the first
// ALLOW_ALWAYS option.
func allowOption(options []acp.PermissionOption) (string, bool) {
	for _, kind := range []string{acp.PermissionAllowOnce, acp.PermissionAllowAlways} {
		for _, option := range options {
			if option.Kind == kind {
				return option.OptionID, true
			}
		}
	}
	return "", false
}

// modeOffered reports whether the agent offers mode id, as a session mode
// or as a value of a configuration option of category MODE.
func (b *Bridge) modeOfferedLocked(id string) bool {
	if slices.ContainsFunc(b.availableModes, func(m acp.SessionMode) bool { return m.ID == id }) {
		return true
	}
	if option, ok := b.modeConfigOptionLocked(); ok {
		return slices.ContainsFunc(option.Option.GetValues(), func(v *sandcrewv1.ConfigOptionValue) bool { return v.GetValueId() == id })
	}
	return false
}

func (b *Bridge) modeConfigOptionLocked() (normalize.ConfigOption, bool) {
	for _, option := range b.configOptions {
		if option.Option.GetCategory() == sandcrewv1.ConfigOptionCategory_CONFIG_OPTION_CATEGORY_MODE {
			return option, true
		}
	}
	return normalize.ConfigOption{}, false
}

// setAgentMode switches the agent mode with session/set_mode, or with the
// configuration option of category MODE when the agent has no session
// modes. It must be called without b.mu: the agent can send updates before
// it answers.
func (b *Bridge) setAgentMode(id string) error {
	b.mu.Lock()
	useModes := len(b.availableModes) > 0
	modeOption, hasModeOption := b.modeConfigOptionLocked()
	b.mu.Unlock()
	ctx, cancel := context.WithTimeout(b.ctx, callTimeout)
	defer cancel()
	switch {
	case useModes:
		if err := b.agent.SetMode(ctx, id); err != nil {
			return err
		}
	case hasModeOption:
		raws, err := b.agent.SetConfigOption(ctx, modeOption.Option.GetConfigOptionId(), id)
		if err != nil {
			return err
		}
		b.replaceConfigOptions(raws)
	default:
		return errors.New("the agent offers no modes")
	}
	b.mu.Lock()
	b.currentModeID = id
	b.mu.Unlock()
	return nil
}

func (b *Bridge) applyModeID(id string) {
	if err := b.setAgentMode(id); err != nil {
		b.optionRejected(fmt.Sprintf("the agent rejected mode %q: %s", id, rpcSummary(err)))
		return
	}
	b.mu.Lock()
	b.emitModeChangedLocked()
	b.mu.Unlock()
}

// applyPermissionMode switches between AUTO_APPROVE and ASK: it switches
// the agent to the vendor's auto-approve or asking mode when the agent
// offers it, and in AUTO_APPROVE it answers the pending requests.
//
// When the agent rejects the vendor mode and keepOnReject is true, the
// session keeps its permission mode: after a rejected switch to ASK the
// agent can still run in its auto-approve mode, where it asks nothing. At
// start (keepOnReject false) there is no previous mode; the agent then
// stays in its initial mode, and the bridge answers the requests that the
// agent sends according to the requested mode.
func (b *Bridge) applyPermissionMode(mode sandcrewv1.PermissionMode, keepOnReject bool) {
	target := b.spec.AskModeID
	if mode == sandcrewv1.PermissionMode_PERMISSION_MODE_AUTO_APPROVE {
		target = b.spec.AutoApproveModeID
	}
	b.mu.Lock()
	offered := target != "" && b.modeOfferedLocked(target)
	switchMode := offered && b.currentModeID != target
	b.mu.Unlock()
	if target != "" && !offered {
		b.logger.Info("the agent does not offer the vendor mode; the bridge answers permission requests", "mode_id", target, "permission_mode", mode.String())
	}
	if switchMode {
		if err := b.setAgentMode(target); err != nil {
			b.optionRejected(fmt.Sprintf("the agent rejected mode %q: %s", target, rpcSummary(err)))
			if keepOnReject {
				return
			}
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.permissionMode = mode
	if mode == sandcrewv1.PermissionMode_PERMISSION_MODE_AUTO_APPROVE {
		ids := make([]string, 0, len(b.pending))
		for requestID := range b.pending {
			ids = append(ids, requestID)
		}
		slices.Sort(ids)
		for _, requestID := range ids {
			if optionID, ok := allowOption(b.pending[requestID].options); ok {
				b.resolveLocked(requestID, acp.PermissionOutcome{Outcome: acp.OutcomeSelected, OptionID: optionID}, true)
			}
		}
	}
	b.emitModeChangedLocked()
}

func (b *Bridge) applyConfigOption(configID, valueID string) {
	b.mu.Lock()
	var value any = valueID
	for _, option := range b.configOptions {
		if option.Option.GetConfigOptionId() == configID && option.Boolean {
			switch valueID {
			case normalize.BooleanTrue:
				value = true
			case normalize.BooleanFalse:
				value = false
			}
		}
	}
	b.mu.Unlock()
	ctx, cancel := context.WithTimeout(b.ctx, callTimeout)
	defer cancel()
	raws, err := b.agent.SetConfigOption(ctx, configID, value)
	if err != nil {
		b.optionRejected(fmt.Sprintf("the agent rejected value %q of option %q: %s", valueID, configID, rpcSummary(err)))
		return
	}
	b.replaceConfigOptions(raws)
}

// replaceConfigOptions stores the option list that the agent returned and
// writes config_options.
func (b *Bridge) replaceConfigOptions(raws []json.RawMessage) {
	if raws == nil {
		return
	}
	options, err := normalize.ConfigOptions(raws)
	if err != nil {
		b.ProtocolError(err)
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.configOptions = options
	b.emitConfigOptionsLocked()
}

func (b *Bridge) optionRejected(message string) {
	b.logger.Warn("option rejected", "error_kind", "OPTION_REJECTED", "error", message)
	b.emitError(sandcrewv1.BridgeErrorKind_BRIDGE_ERROR_KIND_OPTION_REJECTED, message)
}

// watchCredential runs watcher, which was created with err before the
// agent started.
func (b *Bridge) watchCredential(ctx context.Context, watcher *credwatch.Watcher, err error) {
	watchFailed := func(err error) {
		b.logger.Warn("credential watch failed", "error_kind", "CREDENTIAL_WATCH_FAILED", "error", err)
		b.emitError(sandcrewv1.BridgeErrorKind_BRIDGE_ERROR_KIND_CREDENTIAL_WATCH_FAILED, "the credential file cannot be watched")
	}
	if err != nil {
		watchFailed(err)
		return
	}
	go watcher.Run(ctx, credwatch.Handlers{
		Change: func(revision uint64) {
			b.logger.Info("credential file changed", "revision", revision)
			if err := b.log.WriteStdoutOnly(&sandcrewv1.BridgeEvent{Event: &sandcrewv1.BridgeEvent_CredentialUpdate{CredentialUpdate: &sandcrewv1.CredentialUpdate{
				Kind:     sandcrewv1.VendorCredentialKind_VENDOR_CREDENTIAL_KIND_CODEX_AUTH_JSON,
				Revision: revision,
			}}}); err != nil {
				b.logger.Warn("credential update write failed", "error", err)
			}
		},
		Error: watchFailed,
		PollingOnly: func(err error) {
			b.logger.Warn("credential file notifications are not available; polling only", "error", err, "check_interval", b.cfg.CredentialInterval.String())
		},
	})
}

// SessionUpdate implements agent.Handler.
func (b *Bridge) SessionUpdate(raw json.RawMessage) {
	update, err := normalize.SessionUpdate(raw)
	if err != nil {
		b.ProtocolError(err)
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	switch {
	case update.Event != nil:
		b.emit(update.Event)
	case update.CurrentModeID != nil:
		b.currentModeID = *update.CurrentModeID
		b.emitModeChangedLocked()
	case update.ConfigOptions != nil:
		b.configOptions = update.ConfigOptions
		b.emitConfigOptionsLocked()
	}
}

// RequestPermission implements agent.Handler. In AUTO_APPROVE it allows
// the request at once and records both events. Otherwise it waits for a
// permission_answer, a cancel or a switch to AUTO_APPROVE.
func (b *Bridge) RequestPermission(ctx context.Context, request acp.RequestPermissionRequest) (acp.PermissionOutcome, error) {
	requestID := b.cfg.NewID()
	toolCall, err := normalize.ToolCallUpdate(request.ToolCall)
	if err != nil {
		b.ProtocolError(err)
	}
	event := &sandcrewv1.BridgeEvent{Event: &sandcrewv1.BridgeEvent_PermissionRequest{PermissionRequest: &sandcrewv1.PermissionRequest{
		PermissionRequestId: requestID,
		ToolCallId:          toolCall.GetToolCallId(),
		Options:             normalize.PermissionOptions(request.Options),
		ToolCall:            toolCall,
	}}}
	b.mu.Lock()
	if b.permissionMode == sandcrewv1.PermissionMode_PERMISSION_MODE_AUTO_APPROVE {
		if optionID, ok := allowOption(request.Options); ok {
			outcome := acp.PermissionOutcome{Outcome: acp.OutcomeSelected, OptionID: optionID}
			b.emit(event)
			b.emit(permissionResponse(requestID, outcome, true))
			b.mu.Unlock()
			b.logger.Info("permission auto-approved", "permission_request_id", requestID)
			return outcome, nil
		}
	}
	pending := &pendingPermission{options: request.Options, answer: make(chan acp.PermissionOutcome, 1)}
	b.pending[requestID] = pending
	b.emit(event)
	b.setActivityLocked(sandcrewv1.AgentActivity_AGENT_ACTIVITY_WAITING_FOR_PERMISSION)
	b.mu.Unlock()
	b.logger.Info("permission request waits for an answer", "permission_request_id", requestID)
	select {
	case outcome := <-pending.answer:
		return outcome, nil
	case <-ctx.Done():
		b.mu.Lock()
		delete(b.pending, requestID)
		b.mu.Unlock()
		return acp.PermissionOutcome{Outcome: acp.OutcomeCancelled}, nil
	}
}

// ProtocolError implements agent.Handler.
func (b *Bridge) ProtocolError(err error) {
	b.logger.Warn("ACP protocol error", "error_kind", "ACP_PROTOCOL_ERROR", "error", err)
	b.emitError(sandcrewv1.BridgeErrorKind_BRIDGE_ERROR_KIND_ACP_PROTOCOL_ERROR, err.Error())
}

// envValue returns the last value of name in environ, like os/exec.
func envValue(environ []string, name string) string {
	value := ""
	for _, entry := range environ {
		if key, v, ok := strings.Cut(entry, "="); ok && key == name {
			value = v
		}
	}
	return value
}

// rpcSummary describes an agent error without its message, which can repeat
// prompt or tool content: a JSON-RPC error code, a timeout or a closed
// connection.
func rpcSummary(err error) string {
	var rpcErr *acp.RPCError
	switch {
	case errors.As(err, &rpcErr):
		return fmt.Sprintf("JSON-RPC error %d", rpcErr.Code)
	case errors.Is(err, context.DeadlineExceeded):
		return "the agent did not answer in time"
	case errors.Is(err, acp.ErrClosed):
		return "the agent connection closed"
	default:
		return "the request failed"
	}
}

// startErrorMessage describes a failed agent start. Before the first prompt
// the agent has seen no user content, so its JSON-RPC error message (for
// example an authentication failure) is included, shortened.
func startErrorMessage(err error) string {
	var rpcErr *acp.RPCError
	if errors.As(err, &rpcErr) {
		message := rpcErr.Message
		if len(message) > 200 {
			message = message[:200]
		}
		return fmt.Sprintf("the agent did not start its session: JSON-RPC error %d: %s", rpcErr.Code, message)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "the agent did not start its session in time"
	}
	return "the agent did not start its session: " + err.Error()
}

func eventKind(event *sandcrewv1.BridgeEvent) string {
	message := event.ProtoReflect()
	field := message.WhichOneof(message.Descriptor().Oneofs().ByName("event"))
	if field == nil {
		return "unknown"
	}
	return string(field.Name())
}
