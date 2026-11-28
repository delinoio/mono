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
	"os"
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
	// Prompt runs one turn and returns the ACP stop reason. It calls sent
	// once the session/prompt request is written (or cannot be), so a
	// later Cancel reaches the agent after the prompt.
	Prompt(ctx context.Context, text string, sent func()) (string, error)
	// Cancel asks the agent to end the current turn. It returns the
	// context error when ctx ends before the request is sent.
	Cancel(ctx context.Context) error
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
	// The bridge owns the read end of stdout. Cmd.StdoutPipe would be
	// closed by Cmd.Wait at the exit of the process and drop the ACP
	// messages that the bridge has not read yet.
	stdout, stdoutWriter, err := os.Pipe()
	if err != nil {
		stdin.Close()
		return nil, err
	}
	cmd.Stdout = stdoutWriter
	// Wait copies stderr until every holder closed it. A tool that keeps
	// the agent's stderr would otherwise delay Wait past the exit of the
	// adapter; Kill handles the rest of the process group.
	cmd.WaitDelay = waitDelay
	err = cmd.Start()
	stdoutWriter.Close()
	if err != nil {
		stdin.Close()
		stdout.Close()
		return nil, err
	}
	return &execProcess{cmd: cmd, stdin: stdin, stdout: stdout, exited: make(chan struct{})}, nil
}

type execProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *os.File
	exited chan struct{}

	killOnce sync.Once
	mu       sync.Mutex
	// stopped is closed when the stop that Kill started is complete; it
	// is nil before Kill.
	stopped chan struct{}
}

func (p *execProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *execProcess) Stdout() io.Reader     { return p.stdout }

// Wait waits for the process. After Kill, it also waits until the rest of
// the process group ended or got SIGKILL.
func (p *execProcess) Wait() Exit {
	err := p.cmd.Wait()
	close(p.exited)
	p.mu.Lock()
	stopped := p.stopped
	p.mu.Unlock()
	if stopped != nil {
		<-stopped
	}
	state := p.cmd.ProcessState
	if state == nil {
		return Exit{Code: -1, Signal: fmt.Sprint(err)}
	}
	return exitOf(state)
}

// killGrace is the time between SIGTERM and SIGKILL of the agent process
// group.
var killGrace = 5 * time.Second

// waitDelay bounds the stderr copy after the exit of the adapter.
const waitDelay = 2 * time.Second

// groupPollInterval is the interval at which Kill checks whether the
// process group still has processes after the leader exited.
const groupPollInterval = 50 * time.Millisecond

// Kill sends SIGTERM to the process group and SIGKILL after killGrace when
// a process of the group still runs. The exit of the adapter alone does
// not end the stop: a tool of the agent can ignore SIGTERM. Windows has no
// SIGTERM for a process group, so the SIGTERM fails there and the SIGKILL
// ends the process.
func (p *execProcess) Kill() {
	p.killOnce.Do(func() {
		stopped := make(chan struct{})
		p.mu.Lock()
		p.stopped = stopped
		p.mu.Unlock()
		if err := procgroup.Signal(p.cmd.Process, syscall.SIGTERM); err != nil {
			_ = procgroup.Signal(p.cmd.Process, syscall.SIGKILL)
			close(stopped)
			return
		}
		go func() {
			defer close(stopped)
			deadline := time.NewTimer(killGrace)
			defer deadline.Stop()
			ticker := time.NewTicker(groupPollInterval)
			defer ticker.Stop()
			for {
				select {
				case <-deadline.C:
					_ = procgroup.Signal(p.cmd.Process, syscall.SIGKILL)
					return
				case <-ticker.C:
					select {
					case <-p.exited:
						if !groupAlive(p.cmd.Process) {
							return
						}
					default:
					}
				}
			}
		}()
	})
}

// drainTimeout bounds the reading of the remaining ACP messages after the
// agent process ended. A child of the adapter that keeps stdout open
// would otherwise keep the reader waiting.
var drainTimeout = 2 * time.Second

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

	// exited is closed when the process ended; done follows after the
	// remaining output.
	exited chan struct{}
	done   chan struct{}
	exit   Exit
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
		exited:       make(chan struct{}),
		done:         make(chan struct{}),
	}
	a.conn = acp.NewConn(process.Stdout(), process.Stdin(), connHandler{a})
	go a.watchConn()
	go func() {
		exit := process.Wait()
		close(a.exited)
		// Handle the messages that the agent wrote before its exit, so
		// agent_exited is the last event.
		select {
		case <-a.conn.Done():
		case <-time.After(drainTimeout):
			a.logger.Warn("the agent output did not end after the agent exit")
			if closer, ok := process.Stdout().(io.Closer); ok {
				_ = closer.Close()
			}
			select {
			case <-a.conn.Done():
			case <-time.After(drainTimeout):
			}
		}
		a.exit = exit
		a.conn.Close()
		close(a.done)
	}()
	return a
}

// watchConn stops the agent when the ACP connection fails while the
// process runs, for example after a message larger than the size limit:
// the stream position is lost, so no later message can be read, and the
// session would accept prompts that never reach the agent.
func (a *ACPAgent) watchConn() {
	<-a.conn.Done()
	err := a.conn.Err()
	if err == nil || errors.Is(err, io.EOF) {
		return
	}
	select {
	case <-a.exited:
		// The bridge closed stdout after the exit (drainTimeout).
		return
	default:
	}
	a.logger.Error("the ACP connection failed; stopping the agent", "error_kind", "ACP_PROTOCOL_ERROR", "error", err)
	a.handler.ProtocolError(fmt.Errorf("agent: the ACP connection failed: %w", err))
	a.Stop()
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
func (a *ACPAgent) Prompt(ctx context.Context, text string, sent func()) (string, error) {
	var resp acp.PromptResponse
	err := a.conn.CallSent(ctx, acp.MethodSessionPrompt, acp.PromptRequest{
		SessionID: a.session(),
		Prompt:    []acp.ContentBlock{{Type: "text", Text: text}},
	}, &resp, sent)
	if err = a.wrapClosed(err); err != nil {
		return "", err
	}
	return resp.StopReason, nil
}

// Cancel implements Agent.
func (a *ACPAgent) Cancel(ctx context.Context) error {
	return a.conn.Notify(ctx, acp.MethodSessionCancel, acp.CancelNotification{SessionID: a.session()})
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
	return a.wrapClosed(a.conn.Call(ctx, method, params, result))
}

// wrapClosed names the ended agent process in a closed-connection error.
func (a *ACPAgent) wrapClosed(err error) error {
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
