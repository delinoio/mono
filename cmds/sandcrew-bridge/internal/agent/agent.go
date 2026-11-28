// SPDX-License-Identifier: Apache-2.0

// Package agent runs the vendor agent and gives the bridge one interface for
// it. The ACP implementation speaks ACP with the adapter over the stdio of a
// child process. A vendor without ACP gets a HeadlessAdapter that runs the
// vendor's headless CLI and converts its output to ACP-shaped session
// updates, so the bridge and the control plane know only one protocol. The
// owning contract is docs/cmds-sandcrew-bridge-foundation.md#vendors.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/delinoio/mono/cmds/sandcrew-bridge/internal/acp"
	"github.com/delinoio/mono/internal/procgroup"
)

// Handler receives what the agent sends during a session.
type Handler interface {
	// SessionUpdate receives the `update` object of each ACP
	// `session/update`, in order.
	SessionUpdate(update json.RawMessage)
	// RequestPermission answers an ACP `session/request_permission`. It
	// blocks until the bridge or the user answers or ctx ends.
	RequestPermission(ctx context.Context, request acp.RequestPermissionRequest) (acp.PermissionOutcome, error)
	// ProtocolError reports a malformed message or an unexpected request
	// of the agent. The session continues.
	ProtocolError(err error)
}

// Info is what the agent reports when its session starts.
type Info struct {
	SessionID     string
	LoadSession   bool
	ResumeSession bool
	AgentName     string
	AgentVersion  string
	Modes         *acp.SessionModeState
	ConfigOptions []json.RawMessage
	// WrappedHeadless is true for a HeadlessAdapter.
	WrappedHeadless bool
}

// Exit is how the agent process ended.
type Exit struct {
	// Code is the exit code, or -1 when a signal ended the process.
	Code int
	// Signal names the signal that ended the process, or is empty.
	Signal string
}

// Agent is one running agent with one session.
type Agent interface {
	// Start initializes the agent and creates its session with cwd as the
	// working directory and no MCP servers.
	Start(ctx context.Context, cwd string) (Info, error)
	// Prompt runs one turn and returns the ACP stop reason.
	Prompt(ctx context.Context, text string) (string, error)
	// Cancel asks the agent to end the current turn.
	Cancel() error
	// SetMode switches the agent mode.
	SetMode(ctx context.Context, modeID string) error
	// SetConfigOption sets a configuration option and returns the new
	// option list. value is a value ID, or a bool for boolean options.
	SetConfigOption(ctx context.Context, configID string, value any) ([]json.RawMessage, error)
	// Done is closed when the agent process ended.
	Done() <-chan struct{}
	// Exit returns how the process ended, after Done is closed.
	Exit() Exit
	// Stop ends the agent process.
	Stop()
}

// HeadlessAdapter is the interface of a bridge adapter that wraps the
// headless CLI mode of a vendor without ACP (for example
// `claude -p --output-format stream-json`). It implements Agent: Start
// starts nothing until the first prompt, Prompt runs one CLI process per
// turn with the vendor's resume flag and sends each converted output record
// to Handler.SessionUpdate in ACP form, and Info.WrappedHeadless is true.
// The MVP vendors both use ACP, so the bridge has no implementation yet;
// the contract records the candidate commands.
type HeadlessAdapter interface {
	Agent
	// Command returns the CLI and arguments of one turn.
	Command(prompt string, resumeID string) (name string, args []string)
}

// Process is the agent process that the ACP implementation talks to.
type Process interface {
	Stdin() io.WriteCloser
	Stdout() io.Reader
	// Wait waits for the process and returns how it ended.
	Wait() Exit
	// Kill ends the process and its children.
	Kill()
}

// ExecProcess starts name with args as a child process in its own process
// group.
func ExecProcess(name string, args []string, dir string, env []string, stderr io.Writer) (Process, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stderr = stderr
	procgroup.Set(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &execProcess{cmd: cmd, stdin: stdin, stdout: stdout, exited: make(chan struct{})}, nil
}

type execProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.Reader
	exited chan struct{}
}

func (p *execProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *execProcess) Stdout() io.Reader     { return p.stdout }

func (p *execProcess) Wait() Exit {
	err := p.cmd.Wait()
	close(p.exited)
	state := p.cmd.ProcessState
	if state == nil {
		return Exit{Code: -1, Signal: fmt.Sprint(err)}
	}
	return exitOf(state)
}

// killGrace is the time between SIGTERM and SIGKILL of the agent process
// group.
const killGrace = 5 * time.Second

// Kill sends SIGTERM to the process group and SIGKILL after killGrace when
// the process still runs. Windows has no SIGTERM for a process group, so the
// SIGTERM fails there and the SIGKILL ends the process.
func (p *execProcess) Kill() {
	if err := procgroup.Signal(p.cmd.Process, syscall.SIGTERM); err != nil {
		_ = procgroup.Signal(p.cmd.Process, syscall.SIGKILL)
		return
	}
	go func() {
		select {
		case <-p.exited:
		case <-time.After(killGrace):
			_ = procgroup.Signal(p.cmd.Process, syscall.SIGKILL)
		}
	}()
}

// ACPAgent is the ACP implementation of Agent.
type ACPAgent struct {
	process Process
	conn    *acp.Conn
	handler Handler
	logger  *slog.Logger
	info    acp.Implementation
	// AuthMethodID is called with `authenticate` after `initialize` when
	// the agent offers it.
	authMethodID string

	mu        sync.Mutex
	sessionID string

	done chan struct{}
	exit Exit
}

// ACPOptions configure an ACPAgent.
type ACPOptions struct {
	Handler      Handler
	Logger       *slog.Logger
	ClientInfo   acp.Implementation
	AuthMethodID string
}

// NewACPAgent connects to process.
func NewACPAgent(process Process, opts ACPOptions) *ACPAgent {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	a := &ACPAgent{
		process:      process,
		handler:      opts.Handler,
		logger:       opts.Logger,
		info:         opts.ClientInfo,
		authMethodID: opts.AuthMethodID,
		done:         make(chan struct{}),
	}
	a.conn = acp.NewConn(process.Stdout(), process.Stdin(), connHandler{a})
	go func() {
		a.exit = process.Wait()
		// Wait returns after the stdout copy ended; fail pending calls.
		a.conn.Close()
		close(a.done)
	}()
	return a
}

// Start implements Agent.
func (a *ACPAgent) Start(ctx context.Context, cwd string) (Info, error) {
	var initResp acp.InitializeResponse
	clientInfo := a.info
	if err := a.call(ctx, acp.MethodInitialize, acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersion,
		ClientCapabilities: acp.ClientCapabilities{
			FS:       acp.FileSystemCapability{},
			Terminal: false,
		},
		ClientInfo: &clientInfo,
	}, &initResp); err != nil {
		return Info{}, fmt.Errorf("agent: initialize: %w", err)
	}
	if initResp.ProtocolVersion != acp.ProtocolVersion {
		return Info{}, fmt.Errorf("agent: initialize: unsupported ACP protocol version %d", initResp.ProtocolVersion)
	}
	if a.authMethodID != "" {
		offered := false
		for _, method := range initResp.AuthMethods {
			offered = offered || method.ID == a.authMethodID
		}
		if !offered {
			return Info{}, fmt.Errorf("agent: the agent does not offer the %s authentication method", a.authMethodID)
		}
		if err := a.call(ctx, acp.MethodAuthenticate, acp.AuthenticateRequest{MethodID: a.authMethodID}, nil); err != nil {
			return Info{}, fmt.Errorf("agent: authenticate: %w", err)
		}
	}
	var newResp acp.NewSessionResponse
	if err := a.call(ctx, acp.MethodSessionNew, acp.NewSessionRequest{CWD: cwd, MCPServers: []any{}}, &newResp); err != nil {
		return Info{}, fmt.Errorf("agent: session/new: %w", err)
	}
	if newResp.SessionID == "" {
		return Info{}, errors.New("agent: session/new returned no session ID")
	}
	a.mu.Lock()
	a.sessionID = newResp.SessionID
	a.mu.Unlock()
	info := Info{
		SessionID:     newResp.SessionID,
		LoadSession:   initResp.AgentCapabilities.LoadSession,
		ResumeSession: initResp.AgentCapabilities.SessionCapabilities.SupportsResume(),
		Modes:         newResp.Modes,
		ConfigOptions: newResp.ConfigOptions,
	}
	if initResp.AgentInfo != nil {
		info.AgentName = initResp.AgentInfo.Name
		info.AgentVersion = initResp.AgentInfo.Version
	}
	return info, nil
}

// Prompt implements Agent.
func (a *ACPAgent) Prompt(ctx context.Context, text string) (string, error) {
	var resp acp.PromptResponse
	if err := a.call(ctx, acp.MethodSessionPrompt, acp.PromptRequest{
		SessionID: a.session(),
		Prompt:    []acp.ContentBlock{{Type: "text", Text: text}},
	}, &resp); err != nil {
		return "", err
	}
	return resp.StopReason, nil
}

// Cancel implements Agent.
func (a *ACPAgent) Cancel() error {
	return a.conn.Notify(acp.MethodSessionCancel, acp.CancelNotification{SessionID: a.session()})
}

// SetMode implements Agent.
func (a *ACPAgent) SetMode(ctx context.Context, modeID string) error {
	return a.call(ctx, acp.MethodSessionSetMode, acp.SetModeRequest{SessionID: a.session(), ModeID: modeID}, nil)
}

// SetConfigOption implements Agent.
func (a *ACPAgent) SetConfigOption(ctx context.Context, configID string, value any) ([]json.RawMessage, error) {
	req := acp.SetConfigOptionRequest{SessionID: a.session(), ConfigID: configID, Value: value}
	if _, ok := value.(bool); ok {
		req.Type = "boolean"
	}
	var resp acp.SetConfigOptionResponse
	if err := a.call(ctx, acp.MethodSessionSetConfig, req, &resp); err != nil {
		return nil, err
	}
	return resp.ConfigOptions, nil
}

// Done implements Agent.
func (a *ACPAgent) Done() <-chan struct{} { return a.done }

// Exit implements Agent.
func (a *ACPAgent) Exit() Exit {
	<-a.done
	return a.exit
}

// Stop implements Agent.
func (a *ACPAgent) Stop() {
	_ = a.process.Stdin().Close()
	a.process.Kill()
}

func (a *ACPAgent) session() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sessionID
}

func (a *ACPAgent) call(ctx context.Context, method string, params, result any) error {
	err := a.conn.Call(ctx, method, params, result)
	if errors.Is(err, acp.ErrClosed) {
		select {
		case <-a.done:
			return fmt.Errorf("agent: the agent process ended: %w", err)
		default:
		}
	}
	return err
}

// connHandler adapts the ACP connection to Handler.
type connHandler struct{ a *ACPAgent }

func (h connHandler) HandleNotification(method string, params json.RawMessage) {
	if method != acp.MethodSessionUpdate {
		h.a.logger.Debug("ignoring ACP notification", "method", method)
		return
	}
	var notification acp.SessionNotification
	if err := json.Unmarshal(params, &notification); err != nil || len(notification.Update) == 0 {
		h.a.handler.ProtocolError(fmt.Errorf("agent: malformed session/update"))
		return
	}
	h.a.handler.SessionUpdate(notification.Update)
}

func (h connHandler) HandleRequest(ctx context.Context, method string, params json.RawMessage) (any, error) {
	if method != acp.MethodRequestPermission {
		// The bridge offers no file system or terminal capability.
		h.a.handler.ProtocolError(fmt.Errorf("agent: unsupported client method %s", method))
		return nil, &acp.RPCError{Code: acp.CodeMethodNotFound, Message: "method not found: " + method}
	}
	var request acp.RequestPermissionRequest
	if err := json.Unmarshal(params, &request); err != nil {
		h.a.handler.ProtocolError(fmt.Errorf("agent: malformed session/request_permission"))
		return nil, &acp.RPCError{Code: acp.CodeInvalidParams, Message: "invalid params"}
	}
	outcome, err := h.a.handler.RequestPermission(ctx, request)
	if err != nil {
		return nil, err
	}
	return acp.RequestPermissionResponse{Outcome: outcome}, nil
}

func (h connHandler) HandleMalformed(err error) {
	h.a.handler.ProtocolError(err)
}
