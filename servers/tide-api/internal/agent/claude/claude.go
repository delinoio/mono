// SPDX-License-Identifier: FSL-1.1-ALv2

// Package claude is the CLAUDE_CODE vendor of the AGENT_SESSION workload.
// For each CLAUDE_CODE integration, the lease holder runs
// `claude self-hosted-runner orchestrator` as a child process with the
// environment key in the orchestrator's environment only and a hooks
// directory whose spawn-runner hook is the tide-api binary in hook mode
// (hook.go). The hook sends each spawn request to this process over a Unix
// domain socket; tide stores the request with its encrypted single-use
// work order in one transaction and answers with the exit code of the
// spawn-runner contract. A runner gets only the work order and starts
// `claude self-hosted-runner --capacity 1` with it. The owning contract is
// the Claude Code section of docs/servers-tide-api-foundation.md, which
// records the command lines, the exit codes and the conflicts with the
// Claude Code documentation:
//
//   - https://code.claude.com/docs/en/self-hosted-environments-configuration#on-demand-runners
//   - https://code.claude.com/docs/en/self-hosted-environments-reference
package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"

	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/servers/tide-api/internal/agent"
	"github.com/delinoio/mono/servers/tide-api/internal/integration"
	"github.com/delinoio/mono/servers/tide-api/internal/secrets"
	"github.com/delinoio/mono/servers/tide-api/internal/work"
)

// MinVersion is the oldest Claude Code that has the `self-hosted-runner`
// subcommand.
const MinVersion = "2.1.224"

// Orchestrator settings (recorded decisions; see the contract).
const (
	// DefaultExpectedSpawn is --expected-spawn-seconds: the server-side
	// lease of a spawn request, with which its work order expires. tide's
	// time from the spawn request to the runner registration includes the
	// queue wait, the sandbox creation and the claude installation.
	DefaultExpectedSpawn = 15 * time.Minute
	// HookTimeout is --hook-timeout. The hook only stores the request, so
	// it needs far less; HookTimeout + 5 s stays below the expected spawn
	// time, as the orchestrator requires.
	HookTimeout = 30 * time.Second
	// ShutdownDrain is --shutdown-drain-sec: on SIGTERM the orchestrator
	// stops polling and lets running hooks finish. The documentation asks
	// for at least --hook-timeout + 10 s.
	ShutdownDrain = 40 * time.Second
	// HookConcurrency is --hook-concurrency (the documented default).
	HookConcurrency = 4
	// IdleRelease is the runner's --release-idle-session-min in minutes:
	// the runner releases its session after 30 idle minutes and exits 0;
	// the session resumes with a new spawn request on its next message.
	IdleRelease = 30
	// retireMargin: the runner retires (--retire-at) this long before the
	// sandbox's max_ttl, so it releases its session before OwnSand deletes
	// the sandbox.
	retireMargin = 5 * time.Minute
)

// runnerBaseDir is the runner's --base-dir under the guest home. The
// default /workspace needs a writable root directory.
const runnerBaseDir = "/claude-runner"

const (
	hookAcceptBackoffMin = 5 * time.Millisecond
	hookAcceptBackoffMax = time.Second
)

// Process is a started child process.
type Process interface {
	Wait() error
	Signal(sig os.Signal) error
	Kill() error
}

// ProcessSpec describes the orchestrator process. Env is the whole
// environment of the process.
type ProcessSpec struct {
	Path   string
	Args   []string
	Env    []string
	Stdout io.Writer
	Stderr io.Writer
}

// Starter starts a process. Tests replace it with a fake orchestrator.
type Starter func(spec ProcessSpec) (Process, error)

// Options configures the vendor.
type Options struct {
	// ClaudePath is the claude binary; empty selects "claude" on PATH (the
	// container image has /usr/local/bin/claude).
	ClaudePath string
	// HookPath is the tide-api binary that the hooks directory links as
	// spawn-runner; empty selects os.Executable().
	HookPath string
	// RunDir is the parent of the per-orchestrator run directories (the
	// hooks directory and the socket); empty selects os.TempDir(). The
	// socket path must stay below 104 bytes.
	RunDir string
	// Start starts the orchestrator; nil starts a child process.
	Start Starter
	// ExpectedSpawn is --expected-spawn-seconds; zero selects
	// DefaultExpectedSpawn.
	ExpectedSpawn time.Duration
	// StopTimeout bounds the wait for the orchestrator after SIGTERM, then
	// it is killed; zero selects ShutdownDrain + 15 s (the documented stop
	// timeout).
	StopTimeout time.Duration
}

// Vendor is the Claude Code vendor.
type Vendor struct {
	hub  *agent.Hub
	opts Options
	log  *slog.Logger
}

var (
	_ agent.Vendor    = (*Vendor)(nil)
	_ agent.Restarter = (*Vendor)(nil)
)

// Register adds the Claude Code vendor to the Hub.
func Register(hub *agent.Hub, opts Options) *Vendor {
	if opts.ClaudePath == "" {
		opts.ClaudePath = "claude"
	}
	if opts.RunDir == "" {
		opts.RunDir = os.TempDir()
	}
	if opts.ExpectedSpawn == 0 {
		opts.ExpectedSpawn = DefaultExpectedSpawn
	}
	if opts.StopTimeout == 0 {
		opts.StopTimeout = ShutdownDrain + 15*time.Second
	}
	if opts.Start == nil {
		opts.Start = ExecStart
	}
	v := &Vendor{hub: hub, opts: opts, log: hub.Service().Logger}
	hub.AddVendor(v)
	return v
}

func (*Vendor) Vendor() tidev1.AgentVendor { return tidev1.AgentVendor_AGENT_VENDOR_CLAUDE_CODE }
func (*Vendor) Source() work.Source        { return work.SourceClaudeCode }

// OrchestratorArgs returns the orchestrator's arguments. The health
// listener is off (--health-port 0): several orchestrators share the
// container, and the default port 8080 is the port of tide-api.
func (v *Vendor) OrchestratorArgs(hooksDir string) []string {
	return []string{"self-hosted-runner", "orchestrator",
		"--hooks-dir", hooksDir,
		"--health-port", "0",
		"--hook-timeout", strconv.Itoa(int(HookTimeout / time.Second)),
		"--hook-concurrency", strconv.Itoa(HookConcurrency),
		"--expected-spawn-seconds", strconv.Itoa(int(v.opts.ExpectedSpawn / time.Second)),
		"--shutdown-drain-sec", strconv.Itoa(int(ShutdownDrain / time.Second)),
	}
}

// passEnv are the variables of tide-api's environment that the
// orchestrator gets. tide-api's own secrets (DATABASE_URL,
// TIDE_SECRET_KEYS, the GitHub App key) never reach it.
var passEnv = []string{"PATH", "HOME", "TMPDIR", "LANG", "TZ", "HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "https_proxy", "http_proxy", "no_proxy", "SSL_CERT_FILE", "SSL_CERT_DIR"}

// orchestratorEnv returns the orchestrator's environment: the passed
// variables, the environment key (SELF_HOSTED_RUNNER_ENVIRONMENT_SECRET
// carries the value), the hook socket and DISABLE_AUTOUPDATER, because the
// image pins the claude version.
func orchestratorEnv(key, socket string) []string {
	var env []string
	for _, k := range passEnv {
		if val, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+val)
		}
	}
	return append(env, "SELF_HOSTED_RUNNER_ENVIRONMENT_SECRET="+key, SocketEnv+"="+socket, "DISABLE_AUTOUPDATER=1")
}

// Watch runs one orchestrator of the integration until ctx ends (it then
// stops the orchestrator with SIGTERM, waits for its drain and returns
// ctx.Err()) or until the orchestrator exits (an error: the Hub restarts
// it with backoff and calls Restarted).
func (v *Vendor) Watch(ctx context.Context, w *agent.Watch) error {
	key, err := w.Credential(ctx, tidev1.AgentCredential_AGENT_CREDENTIAL_ENVIRONMENT_KEY)
	if err != nil {
		return fmt.Errorf("read the environment key: %w", err)
	}
	hookPath := v.opts.HookPath
	if hookPath == "" {
		if hookPath, err = os.Executable(); err != nil {
			return fmt.Errorf("find the tide-api binary for the hook: %w", err)
		}
	}
	dir, err := os.MkdirTemp(v.opts.RunDir, "tc")
	if err != nil {
		return fmt.Errorf("create the orchestrator directory: %w", err)
	}
	defer os.RemoveAll(dir)
	hooks := filepath.Join(dir, "hooks")
	if err := os.Mkdir(hooks, 0o700); err != nil {
		return fmt.Errorf("create the hooks directory: %w", err)
	}
	if err := os.Symlink(hookPath, filepath.Join(hooks, HookName)); err != nil {
		return fmt.Errorf("create the spawn-runner hook: %w", err)
	}
	socket := filepath.Join(dir, socketName)
	if len(socket) >= 104 {
		return fmt.Errorf("the hook socket path %q is too long; set a shorter TMPDIR", socket)
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		return fmt.Errorf("listen on the hook socket: %w", err)
	}
	if err := os.Chmod(socket, 0o600); err != nil {
		_ = ln.Close()
		return fmt.Errorf("restrict the hook socket: %w", err)
	}
	// Keep accepting during the orchestrator drain. When the drain ends,
	// cancel any retry wait and close the listener to unblock AcceptUnix.
	serveCtx, stopServing := context.WithCancel(context.WithoutCancel(ctx))
	var serving sync.WaitGroup
	serving.Add(1)
	go func() {
		defer serving.Done()
		v.serve(serveCtx, w, ln)
	}()
	defer func() {
		stopServing()
		_ = ln.Close()
		serving.Wait()
	}()

	out := &logWriter{log: w.Logger, ctx: ctx}
	defer out.flush()
	p, err := v.opts.Start(ProcessSpec{Path: v.opts.ClaudePath, Args: v.OrchestratorArgs(hooks), Env: orchestratorEnv(key, socket), Stdout: out, Stderr: out})
	if err != nil {
		return fmt.Errorf("start the orchestrator: %w", err)
	}
	w.Logger.InfoContext(ctx, "claude_orchestrator_started", slog.Int("expected_spawn_seconds", int(v.opts.ExpectedSpawn/time.Second)))
	exited := make(chan error, 1)
	go func() { exited <- p.Wait() }()
	select {
	case err := <-exited:
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil {
			err = errors.New("exit code 0")
		}
		return fmt.Errorf("the orchestrator exited: %w", err)
	case <-ctx.Done():
	}
	// One SIGTERM to the orchestrator alone: it stops polling and lets the
	// running hooks finish within --shutdown-drain-sec; the hook socket
	// stays open until it exits.
	_ = p.Signal(syscall.SIGTERM)
	t := time.NewTimer(v.opts.StopTimeout)
	defer t.Stop()
	select {
	case <-exited:
	case <-t.C:
		_ = p.Kill()
		<-exited
	}
	w.Logger.InfoContext(context.WithoutCancel(ctx), "claude_orchestrator_stopped")
	return ctx.Err()
}

// Restarted counts a restart of a crashed orchestrator.
func (v *Vendor) Restarted(ctx context.Context, log *slog.Logger, backoff time.Duration, err error) {
	v.hub.Engine().Metrics().ClaudeOrchestratorRestart()
	log.WarnContext(ctx, "claude_orchestrator_restart", slog.Duration("backoff", backoff), slog.Any("error", err))
}

type hookListener interface {
	AcceptUnix() (*net.UnixConn, error)
}

// serve answers hook connections until the listener closes. A hook that
// connects while the orchestrator drains is still served, with a context
// that outlives the watch.
func (v *Vendor) serve(ctx context.Context, w *agent.Watch, ln hookListener) {
	var wg sync.WaitGroup
	defer wg.Wait()
	backoff := hookAcceptBackoffMin
	for ctx.Err() == nil {
		conn, err := ln.AcceptUnix()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
				return
			}
			w.Logger.ErrorContext(ctx, "claude_hook_accept_failed", slog.Any("error", err), slog.Duration("retry_in", backoff))
			timer := time.NewTimer(backoff)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return
			}
			backoff = min(2*backoff, hookAcceptBackoffMax)
			continue
		}
		backoff = hookAcceptBackoffMin
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer conn.Close()
			hctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), hookDeadline)
			defer cancel()
			_ = conn.SetDeadline(time.Now().Add(hookDeadline))
			if err := checkPeer(conn); err != nil {
				w.Logger.WarnContext(hctx, "claude_hook_rejected", slog.Any("error", err))
				return
			}
			line, err := bufio.NewReaderSize(io.LimitReader(conn, maxHookMessage), 4096).ReadBytes('\n')
			var req hookRequest
			if err == nil {
				err = json.Unmarshal(line, &req)
			}
			reply := hookReply{Result: HookRetry, Message: "tide-api could not read the spawn request"}
			if err == nil {
				reply = v.spawn(hctx, w, &req)
			}
			b, _ := json.Marshal(reply)
			_, _ = conn.Write(append(b, '\n'))
		}()
	}
}

var (
	orderIDPattern = regexp.MustCompile(`^[\x21-\x7e]{1,256}$`)
	reasonPattern  = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)
)

// clean keeps a vendor value for the log when it matches the pattern.
func clean(p *regexp.Regexp, s string) string {
	if p.MatchString(s) {
		return s
	}
	if s == "" {
		return ""
	}
	return "invalid"
}

// spawn stores one spawn request: the request and its claim row (vendor
// request ID = the order ID, scope = the session ID) and the encrypted
// work order, in one transaction. A repeated order ID creates nothing.
func (v *Vendor) spawn(ctx context.Context, w *agent.Watch, req *hookRequest) hookReply {
	attrs := []slog.Attr{slog.String("order_id", clean(orderIDPattern, req.OrderID)), slog.String("session_id", clean(orderIDPattern, req.SessionID)),
		slog.String("spawn_reason", clean(reasonPattern, req.SpawnReason)), slog.String("attempt", clean(reasonPattern, req.Attempt))}
	reply := func(r HookResult, requestID, msg string) hookReply {
		level := slog.LevelInfo
		if r == HookRetry || r == HookReject {
			level = slog.LevelWarn
		}
		w.Logger.LogAttrs(ctx, level, "claude_spawn_request", append(attrs, slog.String("result", string(r)), slog.String("request_id", requestID))...)
		return hookReply{Result: r, Message: msg}
	}
	switch {
	case !orderIDPattern.MatchString(req.OrderID):
		return reply(HookReject, "", "the spawn request has no valid CLAUDE_RUNNER_ORDER_ID")
	case req.SessionID == "":
		// Pre-warming requests (--min-idle) have no session; tide runs its
		// orchestrators with --min-idle 0 and serves only session requests.
		return reply(HookReject, "", "tide serves only session spawn requests; pre-warming runners are not supported")
	case !orderIDPattern.MatchString(req.SessionID):
		return reply(HookReject, "", "the spawn request has no valid CLAUDE_RUNNER_SESSION_ID")
	case req.WorkOrder == "" || len(req.WorkOrder) > maxWorkOrderBytes:
		return reply(HookRetry, "", "the spawn request has no usable work order")
	}
	svc := v.hub.Service()
	var r *work.Request
	var created bool
	err := svc.DB.InTx(ctx, func(tx pgx.Tx) error {
		var err error
		r, created, err = v.hub.CreateDirectTx(ctx, tx, v, w.In, agent.DirectDemand{
			VendorRequestID: req.OrderID, Scope: req.SessionID, Marker: clean(reasonPattern, req.SpawnReason),
			// The work order expires with the spawn request's lease, so
			// the request cannot wait longer in the queue.
			MaxQueueWait: v.opts.ExpectedSpawn,
			Store: func(ctx context.Context, tx pgx.Tx, r *work.Request) error {
				sealed, err := svc.Secrets.Seal([]byte(req.WorkOrder), workOrderAD(r.ID))
				if err != nil {
					return err
				}
				_, err = tx.Exec(ctx, "UPDATE requests SET work_order_key_id = $2, work_order_ciphertext = $3 WHERE request_id = $1", r.ID, sealed.KeyID, sealed.Ciphertext)
				return err
			},
		})
		return err
	})
	switch {
	case errors.Is(err, agent.ErrNoProfile):
		return reply(HookRetry, "", "no tide runner profile serves this Claude Code integration yet")
	case err != nil:
		w.Logger.WarnContext(ctx, "claude_spawn_store_failed", slog.Any("error", err))
		return reply(HookRetry, "", "tide could not store the spawn request")
	case !created:
		return reply(HookDuplicate, r.ID, "tide already has this spawn request")
	}
	svc.WorkChanged(w.In.OrganizationID)
	return reply(HookStored, r.ID, "tide queued the spawn request")
}

// workOrderAD binds a stored work order to its request.
func workOrderAD(requestID string) []byte {
	return secrets.AssociatedData("claude_work_order", requestID)
}

// CheckStarted (recorded decision): Anthropic documents no API through
// which the orchestrator sees that a runner registered, and the work order
// binds the runner to its one session, so the runner program that started
// with the work order has taken its session: every claim counts.
func (v *Vendor) CheckStarted(_ context.Context, _ integration.Integration, claims []*agent.Claim) ([]string, error) {
	out := make([]string, len(claims))
	for i, c := range claims {
		out[i] = c.RequestID
	}
	return out, nil
}

// Release: Claude Code has no claim to give back. A spawn request that tide
// does not serve expires with its lease, and Anthropic sends a new spawn
// request with a new order ID.
func (v *Vendor) Release(context.Context, integration.Integration, *agent.Claim, *work.Request) (bool, error) {
	return true, nil
}

// Exit codes of InstallScript.
var installCodes = map[int64]string{
	10: "the guest has no curl, which the Claude Code installer needs",
	11: "the guest has no bash, which the Claude Code installer needs",
	12: "the Claude Code installer failed",
	13: "claude " + MinVersion + " or later is missing after the installation",
	14: "Claude Code runners need git on PATH in the guest image",
	15: "Claude Code runners support Linux and macOS guests only",
}

// InstallScript returns the guest script of Prepare: the git check, then
// the official installer when `claude` is missing or older than
// MinVersion. The installer installs the latest version (the contract
// does not pin claude in guests). The script runs the documented
// `curl -fsSL https://claude.ai/install.sh | bash` in two steps, download
// then run, because a POSIX shell pipeline reports only the exit status of
// bash, which hides a failed download.
func InstallScript(guest tidev1.GuestOs) string {
	if agent.IsWindows(guest) {
		return "exit 15\n"
	}
	return agent.ScriptPrelude(guest) + `command -v git >/dev/null 2>&1 || exit 14
ok() {
  v=$(claude --version 2>/dev/null | sed -n 's/^\([0-9][0-9]*\.[0-9][0-9]*\.[0-9][0-9]*\).*/\1/p' | head -n 1)
  [ -n "$v" ] || return 1
  low=$(printf '%s\n%s\n' "$v" ` + MinVersion + ` | sort -t . -k 1,1n -k 2,2n -k 3,3n | head -n 1)
  [ "$low" = ` + MinVersion + ` ]
}
if ok; then exit 0; fi
command -v curl >/dev/null 2>&1 || exit 10
command -v bash >/dev/null 2>&1 || exit 11
tmp=$(mktemp) || exit 12
curl -fsSL https://claude.ai/install.sh -o "$tmp" || { rm -f "$tmp"; exit 12; }
bash "$tmp" || { rm -f "$tmp"; exit 12; }
rm -f "$tmp"
ok || exit 13
`
}

// Prepare checks git and installs claude when it is missing or too old.
func (v *Vendor) Prepare(ctx context.Context, g *work.Guest, _ integration.Integration) error {
	return agent.RunStep(ctx, g, "install Claude Code", InstallScript(g.Runner.GuestOS), agent.InstallTimeout, installCodes)
}

// errWorkOrderUsed reports a request whose work order a runner received
// before.
var errWorkOrderUsed = errors.New("the work order of the request was handed to a runner before")

// Start hands the work order to the runner and starts it. The work order
// is single-use, and the Claude Code contract allows at most one workload
// per order ID, so tide gives it to at most one runner program:
//
//  1. A request whose work order expired (its queue time plus the expected
//     spawn time passed) ends with EXPIRED and MAX_QUEUE_WAIT; Anthropic
//     sends a new spawn request.
//  2. The work order is written to the runner's credential file. A failure
//     until here keeps the stored work order, and the engine returns the
//     request to the queue.
//  3. The stored work order is deleted, then the runner program starts. A
//     failure from here on ends the request with FAILED and
//     RUNNER_START_FAILED instead of a retry.
func (v *Vendor) Start(ctx context.Context, g *work.Guest, in integration.Integration, c *agent.Claim) (work.StartResult, error) {
	svc := v.hub.Service()
	req := g.Request
	end := func(state tidev1.RequestState, reason tidev1.RequestEndReason, err error) (work.StartResult, error) {
		if _, eerr := v.hub.Engine().EndRequest(context.WithoutCancel(ctx), req.OrganizationID, req.ID, work.RequestEnd{State: state, Reason: reason}); eerr != nil {
			v.log.WarnContext(ctx, "request_update_failed", slog.String("request_id", req.ID), slog.Any("error", eerr))
		}
		return work.StartResult{}, err
	}
	if !svc.NowUTC().Before(req.QueueTime.Add(v.opts.ExpectedSpawn)) {
		return end(tidev1.RequestState_REQUEST_STATE_EXPIRED, tidev1.RequestEndReason_REQUEST_END_REASON_MAX_QUEUE_WAIT,
			errors.New("the work order expired before the runner could start"))
	}
	var sealed secrets.Sealed
	var keyID *string
	err := svc.DB.Pool.QueryRow(ctx, "SELECT work_order_key_id, work_order_ciphertext FROM requests WHERE request_id = $1", req.ID).Scan(&keyID, &sealed.Ciphertext)
	if err != nil {
		return work.StartResult{}, fmt.Errorf("read the work order: %w", err)
	}
	if keyID == nil || len(sealed.Ciphertext) == 0 {
		return end(tidev1.RequestState_REQUEST_STATE_FAILED, tidev1.RequestEndReason_REQUEST_END_REASON_RUNNER_START_FAILED, errWorkOrderUsed)
	}
	sealed.KeyID = *keyID
	plain, err := svc.Secrets.Open(sealed, workOrderAD(req.ID))
	if err != nil {
		return work.StartResult{}, fmt.Errorf("decrypt the work order: %w", err)
	}
	dir, err := agent.WriteCredential(ctx, g, string(plain))
	if err != nil {
		return work.StartResult{}, err
	}
	tag, err := svc.DB.Pool.Exec(ctx, `UPDATE requests SET work_order_key_id = NULL, work_order_ciphertext = NULL
		WHERE request_id = $1 AND work_order_ciphertext IS NOT NULL`, req.ID)
	if err != nil {
		return work.StartResult{}, fmt.Errorf("delete the stored work order: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return end(tidev1.RequestState_REQUEST_STATE_FAILED, tidev1.RequestEndReason_REQUEST_END_REASON_RUNNER_START_FAILED, errWorkOrderUsed)
	}
	res, err := agent.StartProgram(ctx, g, Program(dir, g, svc.NowUTC()))
	if err != nil {
		return end(tidev1.RequestState_REQUEST_STATE_FAILED, tidev1.RequestEndReason_REQUEST_END_REASON_RUNNER_START_FAILED, err)
	}
	v.log.InfoContext(ctx, "claude_runner_started", slog.String("organization_id", req.OrganizationID), slog.String("request_id", req.ID),
		slog.String("runner_id", g.Runner.ID), slog.String("order_id", clean(orderIDPattern, c.VendorRequestID)))
	return res, nil
}

// Program returns the runner program: one session (--capacity 1) with the
// work order in SELF_HOSTED_RUNNER_ENVIRONMENT_SECRET, the base directory
// in the guest home, no health listener, an idle release after IdleRelease
// minutes (the runner then exits 0), and retirement retireMargin before the
// deadline from the stored sandbox TTL when that retirement is still possible.
func Program(dir string, g *work.Guest, now time.Time) agent.Program {
	args := []string{"claude", "self-hosted-runner", "--capacity", "1", "--base-dir", agent.HomeArg + runnerBaseDir, "--health-port", "0",
		"--release-idle-session-min", strconv.Itoa(IdleRelease)}
	if g.SandboxMaxTTL > 0 {
		if retire := g.Runner.CreateTime.Add(g.SandboxMaxTTL - retireMargin); retire.After(now.Add(time.Minute)) {
			args = append(args, "--retire-at", strconv.FormatInt(retire.Unix(), 10))
		}
	}
	return agent.Program{Dir: dir, CredentialEnv: "SELF_HOSTED_RUNNER_ENVIRONMENT_SECRET", Args: args}
}

// ExecStart starts the orchestrator as a child process (the default
// Starter).
func ExecStart(spec ProcessSpec) (Process, error) {
	cmd := exec.Command(spec.Path, spec.Args...)
	cmd.Env = spec.Env
	cmd.Stdout, cmd.Stderr = spec.Stdout, spec.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &execProcess{cmd: cmd}, nil
}

type execProcess struct{ cmd *exec.Cmd }

func (p *execProcess) Wait() error                { return p.cmd.Wait() }
func (p *execProcess) Signal(sig os.Signal) error { return p.cmd.Process.Signal(sig) }
func (p *execProcess) Kill() error                { return p.cmd.Process.Kill() }

// logWriter logs the orchestrator's output line by line as
// claude_orchestrator_output. The orchestrator keeps secrets out of its
// output, and the hook writes none; e-mail addresses and JWT-like values
// are masked anyway.
type logWriter struct {
	log *slog.Logger
	ctx context.Context
	mu  sync.Mutex
	buf []byte
}

const maxLogLine = 2048

var (
	emailPattern = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
	jwtPattern   = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]*`)
)

func (l *logWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf = append(l.buf, p...)
	for {
		i := strings.IndexByte(string(l.buf), '\n')
		if i < 0 {
			if len(l.buf) > maxLogLine {
				l.emit(l.buf)
				l.buf = l.buf[:0]
			}
			return len(p), nil
		}
		l.emit(l.buf[:i])
		l.buf = l.buf[i+1:]
	}
}

func (l *logWriter) flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.buf) > 0 {
		l.emit(l.buf)
		l.buf = nil
	}
}

func (l *logWriter) emit(line []byte) {
	s := strings.TrimRight(string(line), "\r")
	if len(s) > maxLogLine {
		s = s[:maxLogLine]
	}
	s = jwtPattern.ReplaceAllString(emailPattern.ReplaceAllString(s, "<email>"), "<token>")
	if strings.TrimSpace(s) == "" {
		return
	}
	l.log.InfoContext(context.WithoutCancel(l.ctx), "claude_orchestrator_output", slog.String("line", s))
}
