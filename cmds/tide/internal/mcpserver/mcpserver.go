// SPDX-License-Identifier: Apache-2.0

// Package mcpserver implements the tide mcp MCP server: the dev session
// functions of the tide CLI as MCP tools for one coding agent, over
// standard input and output. The owning contract is the "MCP server"
// section of docs/cmds-tide-cli-foundation.md.
//
// The server never returns token, API key or --pass-env values in tool
// results, and its logs never contain them, command arguments,
// environment values, command output or file contents.
package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/delinoio/mono/go/tide"
	"github.com/delinoio/mono/go/uuidv7"
	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
)

// MaxWait is the longest wait of one tool call. OpenAI Codex stops an MCP
// tool call after 60 seconds by default, so 50 seconds leaves time for the
// response.
const MaxWait = 50 * time.Second

// Output windowing of exec results: the first 8 KiB and the last 24 KiB of
// the new output of each stream.
const (
	headBytes = 8 << 10
	tailBytes = 24 << 10
)

// confirmationGrace bounds the extra wait for the first exec or attach
// response before the tool can report the command as running.
const confirmationGrace = 10 * time.Second

// Config configures a Server.
type Config struct {
	// Client returns the SDK client. It returns an error when no credential
	// is usable; every tool then returns that error.
	Client func() (*tide.Client, error)
	// OrganizationID is the organization of every tool.
	OrganizationID string
	// PassEnv names local environment variables that every exec receives.
	PassEnv []string
	// LookupEnv reads the server's own environment at exec time.
	LookupEnv func(string) (string, bool)
	// WorkDir is the server's working directory, the default sync path and
	// the base of relative local paths.
	WorkDir string
	// Version is the server version.
	Version string
	// Logger receives the server logs (standard error).
	Logger *slog.Logger
	// maxWait overrides MaxWait in tests.
	maxWait time.Duration
}

// Server is the MCP server.
type Server struct {
	cfg       Config
	mcp       *mcp.Server
	ctx       context.Context
	cancel    context.CancelFunc
	mu        sync.Mutex
	transfers map[string]*transfer
}

// New returns a server.
func New(cfg Config) *Server {
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.maxWait == 0 {
		cfg.maxWait = MaxWait
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{cfg: cfg, ctx: ctx, cancel: cancel, transfers: map[string]*transfer{}}
	s.mcp = mcp.NewServer(&mcp.Implementation{Name: "tide", Title: "tide dev sessions", Version: cfg.Version}, &mcp.ServerOptions{
		Instructions: "Dev sessions are sandboxes with another guest OS (Linux, macOS or Windows) on the team's tide device pool. " +
			"Call list_profiles, create_session and wait_session until the state is RUNNING, then sync the local working tree, " +
			"run builds and tests with exec, read more output with exec_output, and copy results back with download. " +
			"Tool calls wait at most 50 seconds; call wait_session, exec_output or transfer_status again while work continues. " +
			"Delete the session with delete_session when you are done.",
		Logger: cfg.Logger,
	})
	s.register()
	return s
}

// Run serves MCP on t until the client disconnects or ctx ends. Background
// transfers stop when Run returns.
func (s *Server) Run(ctx context.Context, t mcp.Transport) error {
	defer s.cancel()
	return s.mcp.Run(ctx, t)
}

// Connect connects the server to one transport without blocking (tests).
func (s *Server) Connect(ctx context.Context, t mcp.Transport) (*mcp.ServerSession, error) {
	return s.mcp.Connect(ctx, t, nil)
}

// Close stops the background transfers.
func (s *Server) Close() { s.cancel() }

// toolError formats an error for the agent: the Connect code and the
// message.
func toolError(err error) error {
	if err == nil {
		return nil
	}
	var ce *connect.Error
	if errors.As(err, &ce) {
		return fmt.Errorf("%s: %s", ce.Code(), ce.Message())
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s: %s", connect.CodeDeadlineExceeded, err)
	}
	return fmt.Errorf("%s: %s", connect.CodeUnknown, err)
}

func invalid(format string, args ...any) error {
	return toolError(connect.NewError(connect.CodeInvalidArgument, fmt.Errorf(format, args...)))
}

// waitDuration validates a wait_seconds argument: 0 to 50, default 50.
func (s *Server) waitDuration(v *int) (time.Duration, error) {
	if v == nil {
		return s.cfg.maxWait, nil
	}
	if *v < 0 || *v > int(MaxWait/time.Second) {
		return 0, invalid("wait_seconds must be between 0 and %d", int(MaxWait/time.Second))
	}
	return min(time.Duration(*v)*time.Second, s.cfg.maxWait), nil
}

func (s *Server) client() (*tide.Client, error) {
	c, err := s.cfg.Client()
	if err != nil {
		return nil, toolError(connect.NewError(connect.CodeUnauthenticated, err))
	}
	return c, nil
}

func (s *Server) localPath(p string) string {
	if p == "" {
		return s.cfg.WorkDir
	}
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(s.cfg.WorkDir, p)
}

func enumText(e interface{ String() string }, prefix string) string {
	return strings.ToLower(strings.TrimPrefix(e.String(), prefix))
}

func tsText(t interface{ AsTime() time.Time }, set bool) string {
	if !set {
		return ""
	}
	return t.AsTime().UTC().Format(time.RFC3339)
}

// Results.

// Profile is one DEV_SESSION runner profile.
type Profile struct {
	Name          string `json:"name"`
	GuestOS       string `json:"guest_os"`
	VCPUCount     int32  `json:"vcpu_count"`
	MemoryMiB     int64  `json:"memory_mib"`
	DiskGiB       int64  `json:"disk_gib"`
	WorkspacePath string `json:"workspace_path"`
	IdleTTL       string `json:"idle_ttl"`
	MaxTTL        string `json:"max_ttl"`
	MaxQueueWait  string `json:"max_queue_wait"`
}

// Session is one dev session.
type Session struct {
	SessionID     string `json:"session_id"`
	Profile       string `json:"profile"`
	GuestOS       string `json:"guest_os"`
	State         string `json:"state"`
	EndReason     string `json:"end_reason,omitempty"`
	Label         string `json:"label,omitempty"`
	WorkspacePath string `json:"workspace_path"`
	QueuedAt      string `json:"queued_at,omitempty"`
	StartedAt     string `json:"started_at,omitempty"`
	EndedAt       string `json:"ended_at,omitempty"`
}

func sessionOf(d *tidev1.DevSession) Session {
	s := Session{
		SessionID: d.GetDevSessionId(), Profile: d.GetRunnerProfileName(),
		GuestOS: enumText(d.GetGuestOs(), "GUEST_OS_"), State: enumText(d.GetState(), "REQUEST_STATE_"),
		Label: d.GetLabel(), WorkspacePath: d.GetWorkspacePath(),
		QueuedAt: tsText(d.GetQueuedAt(), d.GetQueuedAt() != nil), StartedAt: tsText(d.GetStartedAt(), d.GetStartedAt() != nil),
		EndedAt: tsText(d.GetEndedAt(), d.GetEndedAt() != nil),
	}
	if d.GetEndReason() != tidev1.RequestEndReason_REQUEST_END_REASON_UNSPECIFIED {
		s.EndReason = enumText(d.GetEndReason(), "REQUEST_END_REASON_")
	}
	return s
}

func durText(d interface{ AsDuration() time.Duration }, set bool) string {
	if !set {
		return ""
	}
	return d.AsDuration().String()
}

// Tool arguments and results.

type noArgs struct{}

type listProfilesResult struct {
	Profiles []Profile `json:"profiles"`
}

type createSessionArgs struct {
	Profile string `json:"profile" jsonschema:"the name of a dev session profile from list_profiles"`
	Label   string `json:"label,omitempty" jsonschema:"optional label for display, at most 100 characters"`
}

type sessionArgs struct {
	SessionID string `json:"session_id" jsonschema:"the dev session ID"`
}

type waitSessionArgs struct {
	SessionID   string `json:"session_id" jsonschema:"the dev session ID"`
	WaitSeconds *int   `json:"wait_seconds,omitempty" jsonschema:"seconds to wait, 0 to 50, default 50"`
}

type sessionResult struct {
	Session Session `json:"session"`
}

type listSessionsArgs struct {
	IncludeEnded bool `json:"include_ended,omitempty" jsonschema:"also list ended dev sessions"`
}

type listSessionsResult struct {
	Sessions []Session `json:"sessions"`
}

func (s *Server) register() {
	mcp.AddTool(s.mcp, &mcp.Tool{Name: "list_profiles", Description: "List the dev session profiles: name, guest OS, resources, workspace path, TTLs and maximum queue wait."}, s.listProfiles)
	mcp.AddTool(s.mcp, &mcp.Tool{Name: "create_session", Description: "Create a dev session of a profile. Returns at once with the session ID and state; a session without free capacity is QUEUED. Call wait_session until it is RUNNING."}, s.createSession)
	mcp.AddTool(s.mcp, &mcp.Tool{Name: "wait_session", Description: "Wait until a dev session is RUNNING or ended, or the wait passes, and return its state. Call it again while the state is QUEUED or STARTING."}, s.waitSession)
	mcp.AddTool(s.mcp, &mcp.Tool{Name: "list_sessions", Description: "List your own dev sessions with ID, profile, guest OS, state, label and times."}, s.listSessions)
	mcp.AddTool(s.mcp, &mcp.Tool{Name: "get_session", Description: "Read one dev session."}, s.getSession)
	mcp.AddTool(s.mcp, &mcp.Tool{Name: "delete_session", Description: "Delete a dev session (cancel it when queued) and return its final state."}, s.deleteSession)
	mcp.AddTool(s.mcp, &mcp.Tool{Name: "exec", Description: "Run a command (an argument array, no shell unless you call one) in a running dev session. " +
		"When it ends within the wait: the exit code or signal, timed_out and the output. Otherwise: the exec_id, running true, the output so far and the next offsets for exec_output; the command keeps running. " +
		"Each stream returns at most the first 8 KiB and the last 24 KiB of new output, with the omitted byte range. Standard input is not supported."}, s.exec)
	mcp.AddTool(s.mcp, &mcp.Tool{Name: "exec_output", Description: "Read the output of an exec from the given offsets and its result when it ended within the wait. The guest keeps the last 8 MiB of each stream until 10 minutes after the command ends."}, s.execOutput)
	mcp.AddTool(s.mcp, &mcp.Tool{Name: "sync", Description: "Copy the local Git working tree (tracked and untracked files that are not ignored) into the workspace of a running dev session, incrementally. " +
		"When it ends within the wait: the uploaded, deleted and skipped file counts, the bytes sent and the warnings. Otherwise: a transfer_id for transfer_status."}, s.sync)
	mcp.AddTool(s.mcp, &mcp.Tool{Name: "download", Description: "Copy a guest file or directory to a local path; an existing local path is replaced. " +
		"When it ends within the wait: the local path, the file count and the bytes. Otherwise: a transfer_id for transfer_status."}, s.download)
	mcp.AddTool(s.mcp, &mcp.Tool{Name: "transfer_status", Description: "Return the progress or the result of a sync or download that this server process runs. A transfer stops when the server process exits; a later sync sends the same files again."}, s.transferStatus)
}

func (s *Server) listProfiles(ctx context.Context, _ *mcp.CallToolRequest, _ noArgs) (*mcp.CallToolResult, listProfilesResult, error) {
	c, err := s.client()
	if err != nil {
		return nil, listProfilesResult{}, err
	}
	items, err := tide.Collect(c.AllDevSessionProfiles(ctx, &tidev1.ListDevSessionProfilesRequest{OrganizationId: s.cfg.OrganizationID, PageSize: 200}))
	if err != nil {
		return nil, listProfilesResult{}, toolError(err)
	}
	out := listProfilesResult{Profiles: []Profile{}}
	for _, p := range items {
		out.Profiles = append(out.Profiles, Profile{
			Name: p.GetName(), GuestOS: enumText(p.GetGuestOs(), "GUEST_OS_"),
			VCPUCount: p.GetResources().GetVcpuCount(), MemoryMiB: p.GetResources().GetMemoryMib(), DiskGiB: p.GetResources().GetDiskGib(),
			WorkspacePath: p.GetWorkspacePath(),
			IdleTTL:       durText(p.GetIdleTtl(), p.GetIdleTtl() != nil), MaxTTL: durText(p.GetMaxTtl(), p.GetMaxTtl() != nil),
			MaxQueueWait: durText(p.GetMaxQueueWait(), p.GetMaxQueueWait() != nil),
		})
	}
	return nil, out, nil
}

func (s *Server) createSession(ctx context.Context, _ *mcp.CallToolRequest, in createSessionArgs) (*mcp.CallToolResult, sessionResult, error) {
	if in.Profile == "" {
		return nil, sessionResult{}, invalid("profile is required")
	}
	c, err := s.client()
	if err != nil {
		return nil, sessionResult{}, err
	}
	var profileID string
	for p, err := range c.AllDevSessionProfiles(ctx, &tidev1.ListDevSessionProfilesRequest{OrganizationId: s.cfg.OrganizationID, PageSize: 200}) {
		if err != nil {
			return nil, sessionResult{}, toolError(err)
		}
		if p.GetName() == in.Profile {
			profileID = p.GetRunnerProfileId()
			break
		}
	}
	if profileID == "" {
		return nil, sessionResult{}, toolError(connect.NewError(connect.CodeNotFound, fmt.Errorf("no dev session profile is named %q; list_profiles lists them", in.Profile)))
	}
	res, err := c.DevSessions().CreateDevSession(ctx, connect.NewRequest(&tidev1.CreateDevSessionRequest{OrganizationId: s.cfg.OrganizationID, RunnerProfileId: profileID, Label: in.Label}))
	if err != nil {
		return nil, sessionResult{}, toolError(err)
	}
	s.cfg.Logger.InfoContext(ctx, "tide mcp created a dev session", "dev_session_id", res.Msg.GetDevSession().GetDevSessionId())
	return nil, sessionResult{Session: sessionOf(res.Msg.GetDevSession())}, nil
}

func (s *Server) waitSession(ctx context.Context, _ *mcp.CallToolRequest, in waitSessionArgs) (*mcp.CallToolResult, sessionResult, error) {
	wait, err := s.waitDuration(in.WaitSeconds)
	if err != nil {
		return nil, sessionResult{}, err
	}
	c, err := s.client()
	if err != nil {
		return nil, sessionResult{}, err
	}
	wctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	d, err := c.WaitDevSession(wctx, tide.WaitDevSessionParams{OrganizationID: s.cfg.OrganizationID, DevSessionID: in.SessionID})
	if err != nil && !(errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil) {
		return nil, sessionResult{}, toolError(err)
	}
	if d == nil {
		// The wait ended before the first read (wait_seconds 0).
		res, gerr := c.DevSessions().GetDevSession(ctx, connect.NewRequest(&tidev1.GetDevSessionRequest{OrganizationId: s.cfg.OrganizationID, DevSessionId: in.SessionID}))
		if gerr != nil {
			return nil, sessionResult{}, toolError(gerr)
		}
		d = res.Msg.GetDevSession()
	}
	return nil, sessionResult{Session: sessionOf(d)}, nil
}

func (s *Server) listSessions(ctx context.Context, _ *mcp.CallToolRequest, in listSessionsArgs) (*mcp.CallToolResult, listSessionsResult, error) {
	c, err := s.client()
	if err != nil {
		return nil, listSessionsResult{}, err
	}
	items, err := tide.Collect(c.AllDevSessions(ctx, &tidev1.ListDevSessionsRequest{
		OrganizationId: s.cfg.OrganizationID, PageSize: 200, IncludeEnded: in.IncludeEnded,
		Owner: &tidev1.ListDevSessionsRequest_OwnedByCaller{OwnedByCaller: true},
	}))
	if err != nil {
		return nil, listSessionsResult{}, toolError(err)
	}
	out := listSessionsResult{Sessions: []Session{}}
	for _, d := range items {
		out.Sessions = append(out.Sessions, sessionOf(d))
	}
	return nil, out, nil
}

func (s *Server) getSession(ctx context.Context, _ *mcp.CallToolRequest, in sessionArgs) (*mcp.CallToolResult, sessionResult, error) {
	c, err := s.client()
	if err != nil {
		return nil, sessionResult{}, err
	}
	res, err := c.DevSessions().GetDevSession(ctx, connect.NewRequest(&tidev1.GetDevSessionRequest{OrganizationId: s.cfg.OrganizationID, DevSessionId: in.SessionID}))
	if err != nil {
		return nil, sessionResult{}, toolError(err)
	}
	return nil, sessionResult{Session: sessionOf(res.Msg.GetDevSession())}, nil
}

func (s *Server) deleteSession(ctx context.Context, _ *mcp.CallToolRequest, in sessionArgs) (*mcp.CallToolResult, sessionResult, error) {
	c, err := s.client()
	if err != nil {
		return nil, sessionResult{}, err
	}
	res, err := c.DevSessions().DeleteDevSession(ctx, connect.NewRequest(&tidev1.DeleteDevSessionRequest{OrganizationId: s.cfg.OrganizationID, DevSessionId: in.SessionID}))
	if err != nil {
		return nil, sessionResult{}, toolError(err)
	}
	s.cfg.Logger.InfoContext(ctx, "tide mcp deleted a dev session", "dev_session_id", in.SessionID)
	return nil, sessionResult{Session: sessionOf(res.Msg.GetDevSession())}, nil
}

// Exec.

const maxExecTimeoutSeconds = math.MaxInt64 / int64(time.Second)

type execArgs struct {
	SessionID      string            `json:"session_id" jsonschema:"the dev session ID"`
	Command        []string          `json:"command" jsonschema:"the argument array; the first element is the program"`
	Cwd            string            `json:"cwd,omitempty" jsonschema:"working directory: an absolute guest path or a path relative to the workspace path"`
	Env            map[string]string `json:"env,omitempty" jsonschema:"environment variables for the command"`
	TimeoutSeconds int64             `json:"timeout_seconds,omitempty" jsonschema:"seconds before stopping the command, 0 to 9223372036 inclusive, 0 means no timeout"`
	WaitSeconds    *int              `json:"wait_seconds,omitempty" jsonschema:"seconds to wait for the command to end, 0 to 50, default 50"`
}

type execOutputArgs struct {
	SessionID    string `json:"session_id" jsonschema:"the dev session ID"`
	ExecID       string `json:"exec_id" jsonschema:"the exec_id of an exec result"`
	StdoutOffset int64  `json:"stdout_offset" jsonschema:"the next stdout byte offset (next_stdout_offset of the last result)"`
	StderrOffset int64  `json:"stderr_offset" jsonschema:"the next stderr byte offset (next_stderr_offset of the last result)"`
	WaitSeconds  *int   `json:"wait_seconds,omitempty" jsonschema:"seconds to wait for more output or the end, 0 to 50, default 50"`
}

// Output is the new output of one stream.
type Output struct {
	// Text is the returned output, decoded as UTF-8 with invalid bytes
	// replaced.
	Text string `json:"text"`
	// From and To are the byte range of the new output in the stream.
	From int64 `json:"from"`
	To   int64 `json:"to"`
	// Omitted is the byte range that Text leaves out, if any. Read it with
	// exec_output while the guest keeps it.
	Omitted *tide.ByteRange `json:"omitted,omitempty"`
}

// Truncated reports output that the guest no longer keeps.
type Truncated struct {
	MissingStdout *tide.ByteRange `json:"missing_stdout,omitempty"`
	MissingStderr *tide.ByteRange `json:"missing_stderr,omitempty"`
}

type execResult struct {
	ExecID           string      `json:"exec_id"`
	Running          bool        `json:"running"`
	ExitCode         *int64      `json:"exit_code,omitempty"`
	Signal           *int32      `json:"signal,omitempty"`
	TimedOut         bool        `json:"timed_out,omitempty"`
	FailureReason    string      `json:"failure_reason,omitempty"`
	FailureMessage   string      `json:"failure_message,omitempty"`
	SandboxEnded     string      `json:"sandbox_ended,omitempty"`
	Stdout           Output      `json:"stdout"`
	Stderr           Output      `json:"stderr"`
	NextStdoutOffset int64       `json:"next_stdout_offset"`
	NextStderrOffset int64       `json:"next_stderr_offset"`
	OutputTruncated  []Truncated `json:"output_truncated,omitempty"`
	// MissingEnv names --pass-env variables that were not set; their
	// values are never returned.
	MissingEnv []string `json:"missing_env,omitempty"`
}

// window keeps the first headBytes and the last tailBytes of a stream.
type window struct {
	from  int64
	next  int64
	head  []byte
	tail  []byte
	total int64
}

func (w *window) add(data []byte) {
	w.total += int64(len(data))
	if room := headBytes - len(w.head); room > 0 {
		n := min(room, len(data))
		w.head = append(w.head, data[:n]...)
		data = data[n:]
	}
	w.tail = append(w.tail, data...)
	if len(w.tail) > tailBytes {
		w.tail = append([]byte(nil), w.tail[len(w.tail)-tailBytes:]...)
	}
}

func (w *window) output() Output {
	o := Output{From: w.from, To: w.next}
	kept := int64(len(w.head) + len(w.tail))
	text := string(w.head) + string(w.tail)
	if w.total > kept {
		o.Omitted = &tide.ByteRange{From: w.from + int64(len(w.head)), To: w.next - int64(len(w.tail))}
		text = string(w.head) + "\n[... output omitted ...]\n" + string(w.tail)
	}
	o.Text = strings.ToValidUTF8(text, "�")
	return o
}

// collector gathers the events of one tool call.
type collector struct {
	mu        sync.Mutex
	execID    string
	out, err  window
	truncated []Truncated
	confirmed chan struct{}
	once      sync.Once
}

func newCollector(so, se int64) *collector {
	return &collector{out: window{from: so, next: so}, err: window{from: se, next: se}, confirmed: make(chan struct{})}
}

// hasConfirmation reports whether the SDK delivered a callback. For an
// attach, the SDK delivers output or truncation and skips keepalives.
func (c *collector) hasConfirmation() bool {
	select {
	case <-c.confirmed:
		return true
	default:
		return false
	}
}

func (c *collector) on(ev tide.ExecEvent) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.execID = ev.ExecID
	c.once.Do(func() { close(c.confirmed) })
	switch ev.Kind {
	case tide.ExecEventStdout:
		c.out.add(ev.Data)
		c.out.next = ev.Offset + int64(len(ev.Data))
	case tide.ExecEventStderr:
		c.err.add(ev.Data)
		c.err.next = ev.Offset + int64(len(ev.Data))
	case tide.ExecEventOutputTruncated:
		t := Truncated{}
		if !ev.MissingStdout.Empty() {
			r := ev.MissingStdout
			t.MissingStdout = &r
			c.out.next = max(c.out.next, r.To)
		}
		if !ev.MissingStderr.Empty() {
			r := ev.MissingStderr
			t.MissingStderr = &r
			c.err.next = max(c.err.next, r.To)
		}
		c.truncated = append(c.truncated, t)
	}
	return nil
}

func (c *collector) result(execID string, res *tide.ExecResult) execResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := execResult{
		ExecID: execID, Stdout: c.out.output(), Stderr: c.err.output(),
		NextStdoutOffset: c.out.next, NextStderrOffset: c.err.next, OutputTruncated: c.truncated,
	}
	if res == nil {
		r.Running = true
		return r
	}
	switch res.Kind {
	case tide.ExecResultExited:
		r.ExitCode, r.Signal, r.TimedOut = res.ExitCode, res.Signal, res.TimedOut
	case tide.ExecResultFailed:
		r.FailureReason = enumText(res.FailureReason, "EXEC_FAILURE_REASON_")
		r.FailureMessage = res.FailureMessage
	case tide.ExecResultSandboxEnded:
		r.SandboxEnded = res.SandboxEndReason
	}
	return r
}

// errExecUnconfirmed is returned by runWithWait when the wait and grace
// period end before a callback or a definite outcome.
var errExecUnconfirmed = errors.New("exec unconfirmed")

type execOutcome struct {
	res *tide.ExecResult
	err error
}

// runWithWait waits for fn to return. After wait, it allows up to
// confirmationGrace for the first callback or outcome, within MaxWait
// overall. It then cancels the stream and lets the command keep running
// in the guest. Without confirmation, it returns errExecUnconfirmed.
func (s *Server) runWithWait(ctx context.Context, wait time.Duration, col *collector, fn func(context.Context) (*tide.ExecResult, error)) (*tide.ExecResult, error) {
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan execOutcome, 1)
	go func() {
		res, err := fn(sctx)
		done <- execOutcome{res, err}
	}()
	start := time.Now()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case o := <-done:
		return o.res, o.err
	case <-timer.C:
	}
	grace := min(confirmationGrace, max(s.cfg.maxWait-time.Since(start), 0))
	select {
	case o := <-done:
		return o.res, o.err
	case <-col.confirmed:
	case <-time.After(grace):
	}
	cancel()
	o := <-done
	if o.err == nil {
		return o.res, nil
	}
	var se *tide.ExecStreamError
	if errors.As(o.err, &se) && errors.Is(se.Err, context.Canceled) && ctx.Err() == nil {
		if !col.hasConfirmation() {
			return nil, errExecUnconfirmed
		}
		return nil, nil // still running
	}
	return nil, o.err
}

func (s *Server) exec(ctx context.Context, _ *mcp.CallToolRequest, in execArgs) (*mcp.CallToolResult, execResult, error) {
	wait, err := s.waitDuration(in.WaitSeconds)
	if err != nil {
		return nil, execResult{}, err
	}
	if len(in.Command) == 0 {
		return nil, execResult{}, invalid("command must have at least one element")
	}
	if in.TimeoutSeconds < 0 {
		return nil, execResult{}, invalid("timeout_seconds must not be negative")
	}
	if in.TimeoutSeconds > maxExecTimeoutSeconds {
		return nil, execResult{}, invalid("timeout_seconds must be between 0 and %d", maxExecTimeoutSeconds)
	}
	c, err := s.client()
	if err != nil {
		return nil, execResult{}, err
	}
	env := map[string]string{}
	for k, v := range in.Env {
		env[k] = v
	}
	// --pass-env values come from the server's own environment at exec
	// time and win over the tool arguments. They never appear in results
	// or logs.
	var missing []string
	for _, name := range s.cfg.PassEnv {
		if v, ok := s.cfg.LookupEnv(name); ok {
			env[name] = v
		} else {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	execID, err := uuidv7.NewString()
	if err != nil {
		return nil, execResult{}, toolError(err)
	}
	col := newCollector(0, 0)
	s.cfg.Logger.InfoContext(ctx, "tide mcp exec", "dev_session_id", in.SessionID, "exec_id", execID)
	res, err := s.runWithWait(ctx, wait, col, func(sctx context.Context) (*tide.ExecResult, error) {
		return c.Exec(sctx, tide.ExecParams{
			OrganizationID: s.cfg.OrganizationID, DevSessionID: in.SessionID, ExecID: execID,
			Command: in.Command, Env: env, WorkingDirectory: in.Cwd,
			Timeout: time.Duration(in.TimeoutSeconds) * time.Second, OnEvent: col.on,
		})
	})
	if errors.Is(err, errExecUnconfirmed) {
		s.cfg.Logger.WarnContext(ctx, "tide mcp exec start unconfirmed", "dev_session_id", in.SessionID, "exec_id", execID)
		return nil, execResult{}, toolError(connect.NewError(connect.CodeDeadlineExceeded, fmt.Errorf(
			"exec %s did not confirm its start before the wait passed, so it may not run; exec_output with this exec_id returns not_found if it did not start", execID)))
	}
	if err != nil {
		return nil, execResult{}, toolError(err)
	}
	out := col.result(execID, res)
	out.MissingEnv = missing
	return nil, out, nil
}

func (s *Server) execOutput(ctx context.Context, _ *mcp.CallToolRequest, in execOutputArgs) (*mcp.CallToolResult, execResult, error) {
	wait, err := s.waitDuration(in.WaitSeconds)
	if err != nil {
		return nil, execResult{}, err
	}
	if in.ExecID == "" || in.StdoutOffset < 0 || in.StderrOffset < 0 {
		return nil, execResult{}, invalid("exec_id is required and the offsets must not be negative")
	}
	c, err := s.client()
	if err != nil {
		return nil, execResult{}, err
	}
	col := newCollector(in.StdoutOffset, in.StderrOffset)
	res, err := s.runWithWait(ctx, wait, col, func(sctx context.Context) (*tide.ExecResult, error) {
		return c.AttachExec(sctx, tide.AttachParams{
			OrganizationID: s.cfg.OrganizationID, DevSessionID: in.SessionID, ExecID: in.ExecID,
			StdoutOffset: in.StdoutOffset, StderrOffset: in.StderrOffset, OnEvent: col.on,
		})
	})
	if errors.Is(err, errExecUnconfirmed) {
		return nil, execResult{}, toolError(connect.NewError(connect.CodeDeadlineExceeded, fmt.Errorf(
			"exec_output did not confirm exec %s before the wait expired. Retry with the same exec_id, stdout_offset %d, and stderr_offset %d.",
			in.ExecID, in.StdoutOffset, in.StderrOffset)))
	}
	if err != nil {
		return nil, execResult{}, toolError(err)
	}
	return nil, col.result(in.ExecID, res), nil
}

// Transfers.

type syncArgs struct {
	SessionID   string `json:"session_id" jsonschema:"the dev session ID"`
	LocalPath   string `json:"local_path,omitempty" jsonschema:"local directory in a Git working tree; default: the server's working directory"`
	Force       bool   `json:"force,omitempty" jsonschema:"remove a sync lock that a stopped sync left"`
	WaitSeconds *int   `json:"wait_seconds,omitempty" jsonschema:"seconds to wait for the end, 0 to 50, default 50"`
}

type downloadArgs struct {
	SessionID   string `json:"session_id" jsonschema:"the dev session ID"`
	GuestPath   string `json:"guest_path" jsonschema:"guest file or directory: absolute, or relative to the workspace path"`
	LocalPath   string `json:"local_path" jsonschema:"local destination; relative paths start at the server's working directory"`
	WaitSeconds *int   `json:"wait_seconds,omitempty" jsonschema:"seconds to wait for the end, 0 to 50, default 50"`
}

type transferStatusArgs struct {
	TransferID  string `json:"transfer_id" jsonschema:"the transfer_id of a sync or download result"`
	WaitSeconds *int   `json:"wait_seconds,omitempty" jsonschema:"seconds to wait for the end, 0 to 50, default 50"`
}

// Progress is the progress of a running transfer.
type Progress struct {
	Phase      string `json:"phase"`
	BytesDone  int64  `json:"bytes_done"`
	BytesTotal int64  `json:"bytes_total,omitempty"`
}

type transferResult struct {
	TransferID string               `json:"transfer_id"`
	Kind       string               `json:"kind"`
	Running    bool                 `json:"running"`
	Progress   *Progress            `json:"progress,omitempty"`
	Sync       *tide.SyncResult     `json:"sync,omitempty"`
	Download   *tide.DownloadResult `json:"download,omitempty"`
}

// transfer is one sync or download that runs in the server process.
type transfer struct {
	id       string
	kind     string
	done     chan struct{}
	mu       sync.Mutex
	progress tide.TransferProgress
	sync     *tide.SyncResult
	download *tide.DownloadResult
	err      error
}

func (t *transfer) onProgress(p tide.TransferProgress) {
	t.mu.Lock()
	t.progress = p
	t.mu.Unlock()
}

func (s *Server) startTransfer(kind string, run func(ctx context.Context, t *transfer)) (*transfer, error) {
	id, err := uuidv7.NewString()
	if err != nil {
		return nil, err
	}
	t := &transfer{id: id, kind: kind, done: make(chan struct{})}
	s.mu.Lock()
	s.transfers[id] = t
	s.mu.Unlock()
	go func() {
		defer close(t.done)
		// The transfer runs on the server context, not the tool call, so
		// it continues after the call returns.
		run(s.ctx, t)
	}()
	return t, nil
}

// awaitTransfer waits up to wait and returns the result or the progress.
func (s *Server) awaitTransfer(ctx context.Context, t *transfer, wait time.Duration) (transferResult, error) {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-t.done:
	case <-timer.C:
	case <-ctx.Done():
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	r := transferResult{TransferID: t.id, Kind: t.kind}
	select {
	case <-t.done:
		if t.err != nil {
			return r, toolError(t.err)
		}
		r.Sync, r.Download = t.sync, t.download
	default:
		r.Running = true
		r.Progress = &Progress{Phase: t.progress.Phase.String(), BytesDone: t.progress.BytesDone, BytesTotal: t.progress.BytesTotal}
		if t.progress.Phase == 0 {
			r.Progress.Phase = tide.TransferPreparing.String()
		}
	}
	return r, nil
}

func (s *Server) sync(ctx context.Context, _ *mcp.CallToolRequest, in syncArgs) (*mcp.CallToolResult, transferResult, error) {
	wait, err := s.waitDuration(in.WaitSeconds)
	if err != nil {
		return nil, transferResult{}, err
	}
	c, err := s.client()
	if err != nil {
		return nil, transferResult{}, err
	}
	dir := s.localPath(in.LocalPath)
	t, err := s.startTransfer("sync", func(tctx context.Context, t *transfer) {
		res, err := c.Sync(tctx, tide.SyncParams{OrganizationID: s.cfg.OrganizationID, DevSessionID: in.SessionID, LocalDir: dir, Force: in.Force, Progress: t.onProgress})
		t.mu.Lock()
		t.sync, t.err = res, err
		t.mu.Unlock()
		s.cfg.Logger.Info("tide mcp sync ended", "transfer_id", t.id, "dev_session_id", in.SessionID, "code", codeText(err))
	})
	if err != nil {
		return nil, transferResult{}, toolError(err)
	}
	r, err := s.awaitTransfer(ctx, t, wait)
	return nil, r, err
}

func (s *Server) download(ctx context.Context, _ *mcp.CallToolRequest, in downloadArgs) (*mcp.CallToolResult, transferResult, error) {
	wait, err := s.waitDuration(in.WaitSeconds)
	if err != nil {
		return nil, transferResult{}, err
	}
	if in.GuestPath == "" || in.LocalPath == "" {
		return nil, transferResult{}, invalid("guest_path and local_path are required")
	}
	c, err := s.client()
	if err != nil {
		return nil, transferResult{}, err
	}
	local := s.localPath(in.LocalPath)
	t, err := s.startTransfer("download", func(tctx context.Context, t *transfer) {
		res, err := c.Download(tctx, tide.DownloadParams{OrganizationID: s.cfg.OrganizationID, DevSessionID: in.SessionID, GuestPath: in.GuestPath, LocalPath: local, Progress: t.onProgress})
		t.mu.Lock()
		t.download, t.err = res, err
		t.mu.Unlock()
		s.cfg.Logger.Info("tide mcp download ended", "transfer_id", t.id, "dev_session_id", in.SessionID, "code", codeText(err))
	})
	if err != nil {
		return nil, transferResult{}, toolError(err)
	}
	r, err := s.awaitTransfer(ctx, t, wait)
	return nil, r, err
}

func (s *Server) transferStatus(ctx context.Context, _ *mcp.CallToolRequest, in transferStatusArgs) (*mcp.CallToolResult, transferResult, error) {
	wait, err := s.waitDuration(in.WaitSeconds)
	if err != nil {
		return nil, transferResult{}, err
	}
	s.mu.Lock()
	t := s.transfers[in.TransferID]
	s.mu.Unlock()
	if t == nil {
		return nil, transferResult{}, toolError(connect.NewError(connect.CodeNotFound, errors.New("no transfer with this transfer_id runs in this server process; transfers stop when the server exits, so sync again")))
	}
	r, err := s.awaitTransfer(ctx, t, wait)
	return nil, r, err
}

func codeText(err error) string {
	if err == nil {
		return "ok"
	}
	return connect.CodeOf(err).String()
}
