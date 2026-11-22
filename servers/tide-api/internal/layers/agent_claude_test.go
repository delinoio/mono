// SPDX-License-Identifier: FSL-1.1-ALv2

package layers_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/servers/tide-api/internal/agent"
	"github.com/delinoio/mono/servers/tide-api/internal/agent/claude"
	"github.com/delinoio/mono/servers/tide-api/internal/agent/claude/claudefake"
	"github.com/delinoio/mono/servers/tide-api/internal/layers"
	"github.com/delinoio/mono/servers/tide-api/internal/ownsandclient/ownsandfake"
)

const (
	claudeEnvKey    = "sk-ant-environment-key-0123456789"
	claudeWorkOrder = "eyJhbGciOiJFUzI1NiJ9.eyJqdGkiOiJvcmRlci0xIn0.c2lnbmF0dXJlLW9mLXRoZS13b3JrLW9yZGVy"
	claudeHookPath  = "/usr/local/bin/tide-api"
)

type claudeWorld struct {
	*agentWorld
	orch          *claudefake.Fake
	integrationID string
	profileID     string
}

func newClaudeWorld(t *testing.T, guest tidev1.GuestOs, expectedSpawn time.Duration) *claudeWorld {
	t.Helper()
	orch := claudefake.New(t)
	w := newAgentWorld(t, layers.Options{Claude: claude.Options{Start: orch.Start, HookPath: claudeHookPath, ExpectedSpawn: expectedSpawn,
		StopTimeout: time.Second}})
	cw := &claudeWorld{agentWorld: w, orch: orch}
	cw.integrationID = w.createIntegration(t, &tidev1.CreateAgentIntegrationRequest{
		Vendor: tidev1.AgentVendor_AGENT_VENDOR_CLAUDE_CODE, Name: "claude",
		Settings:    &tidev1.CreateAgentIntegrationRequest_ClaudeCode{ClaudeCode: &tidev1.ClaudeCodeSettings{}},
		Credentials: &tidev1.CreateAgentIntegrationRequest_ClaudeCodeCredentials{ClaudeCodeCredentials: &tidev1.ClaudeCodeCredentials{EnvironmentKey: claudeEnvKey}},
	})
	cw.profileID = w.createProfile(t, agentProfileProto("claude-agents", cw.integrationID, guest, time.Hour))
	cw.proc(t)
	return cw
}

// proc waits for the one running orchestrator.
func (w *claudeWorld) proc(t *testing.T) *claudefake.Proc {
	t.Helper()
	var p *claudefake.Proc
	eventually(t, "one running orchestrator", func() bool {
		r := w.orch.Running()
		if len(r) != 1 {
			return false
		}
		p = r[0]
		return true
	})
	return p
}

func (w *claudeWorld) full() {
	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) {
		o.CreateError = connect.NewWireError(connect.CodeUnavailable, errors.New("full"))
	})
}

func (w *claudeWorld) storedWorkOrder(t *testing.T, requestID string) []byte {
	t.Helper()
	var c []byte
	if err := w.env.DB.Pool.QueryRow(context.Background(), "SELECT work_order_ciphertext FROM requests WHERE request_id = $1", requestID).Scan(&c); err != nil {
		t.Fatal(err)
	}
	return c
}

// TestClaudeSessionLifecycle: the orchestrator runs with the environment
// key in its environment only and the tide hook in its hooks directory; a
// spawn request stores the request with the encrypted work order; a
// repeated order ID creates nothing; the runner gets only the work order,
// starts `claude self-hosted-runner --capacity 1`, becomes BUSY, and its
// exit ends the request. The environment key never reaches the guest, and
// no secret reaches the log.
func TestClaudeSessionLifecycle(t *testing.T) {
	w := newClaudeWorld(t, tidev1.GuestOs_GUEST_OS_LINUX, 0)
	p := w.proc(t)
	if p.Spec.Path != "claude" || !slices.Equal(p.Spec.Args[:2], []string{"self-hosted-runner", "orchestrator"}) ||
		p.Arg("--health-port") != "0" || p.Arg("--expected-spawn-seconds") != "900" || p.Arg("--hook-timeout") != "30" ||
		p.Arg("--shutdown-drain-sec") != "40" {
		t.Fatalf("orchestrator %q %q", p.Spec.Path, p.Spec.Args)
	}
	if p.Env("SELF_HOSTED_RUNNER_ENVIRONMENT_SECRET") != claudeEnvKey || p.Env("DISABLE_AUTOUPDATER") != "1" {
		t.Fatalf("orchestrator environment %q", p.Spec.Env)
	}
	for _, kv := range p.Spec.Env {
		if strings.HasPrefix(kv, "TIDE_SECRET_KEYS=") || strings.HasPrefix(kv, "DATABASE_URL=") || strings.HasPrefix(kv, "TIDE_TEST_DATABASE_URL=") {
			t.Fatalf("the orchestrator inherits %q", kv)
		}
	}
	hooks := p.Arg("--hooks-dir")
	if target, err := os.Readlink(filepath.Join(hooks, claude.HookName)); err != nil || target != claudeHookPath {
		t.Fatalf("hook link %q %v", target, err)
	}
	if st, err := os.Stat(filepath.Dir(hooks)); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("run directory %v %v", st, err)
	}

	w.full()
	code, stderr := p.Spawn(claudefake.Spawn{OrderID: "order-1", SessionID: "session_01", Reason: "session_created", WorkOrder: claudeWorkOrder})
	if code != 0 || !strings.Contains(stderr, "queued") {
		t.Fatalf("hook exit %d: %s", code, stderr)
	}
	req := w.requestBySource(t, "order-1")
	stored := w.storedWorkOrder(t, req.GetRequestId())
	if len(stored) == 0 || bytes.Contains(stored, []byte(claudeWorkOrder)) {
		t.Fatalf("stored work order %q", stored)
	}
	// A redelivery of the same order ID creates nothing and still exits 0.
	if code, stderr := p.Spawn(claudefake.Spawn{OrderID: "order-1", SessionID: "session_01", WorkOrder: claudeWorkOrder}); code != 0 || !strings.Contains(stderr, "already") {
		t.Fatalf("repeated order: %d %s", code, stderr)
	}
	stays(t, "one request", func() bool { return len(w.requests(t)) == 1 })

	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) { o.CreateError = nil })
	var runner *tidev1.Runner
	eventually(t, "runner starts", func() bool {
		r := w.request(t, req.GetRequestId())
		if r.GetRunnerId() == "" {
			return false
		}
		runner = w.runner(t, r.GetRunnerId())
		return runner.GetState() == tidev1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK || runner.GetState() == tidev1.RunnerState_RUNNER_STATE_BUSY
	})
	sandboxID := runner.GetOwnsandSandboxId()
	dir := agent.RunnerDir(tidev1.GuestOs_GUEST_OS_LINUX, runner.GetRunnerId())
	files := w.fake.Files(w.osOrg, sandboxID)
	if len(files) != 1 || string(files[dir+"/"+agent.CredentialFile]) != claudeWorkOrder {
		t.Fatalf("guest files %q", files)
	}
	script := w.guest.startScript(sandboxID)
	for _, want := range []string{
		"SELF_HOSTED_RUNNER_ENVIRONMENT_SECRET=$(cat '" + dir + "/credential')",
		"exec 'claude' 'self-hosted-runner' '--capacity' '1' '--base-dir' \"$HOME\"'/claude-runner' '--health-port' '0' '--release-idle-session-min' '30' '--retire-at' ",
		">>'" + dir + "/runner.log' 2>&1",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("start script lacks %q:\n%s", want, script)
		}
	}
	if !strings.Contains(w.guest.all(), "curl -fsSL https://claude.ai/install.sh -o") {
		t.Fatal("no install script")
	}
	// The work order went to one runner: tide no longer stores it.
	if c := w.storedWorkOrder(t, req.GetRequestId()); c != nil {
		t.Fatal("the work order is still stored after the runner start")
	}
	w.waitRunner(t, runner.GetRunnerId(), tidev1.RunnerState_RUNNER_STATE_BUSY)
	w.waitRequest(t, req.GetRequestId(), tidev1.RequestState_REQUEST_STATE_RUNNING)
	w.guest.exitProgram(sandboxID, 0)
	r := w.waitRequest(t, req.GetRequestId(), tidev1.RequestState_REQUEST_STATE_ENDED)
	if r.GetEndReason() != tidev1.RequestEndReason_REQUEST_END_REASON_COMPLETED {
		t.Fatalf("end reason %v", r.GetEndReason())
	}
	w.waitRunner(t, runner.GetRunnerId(), tidev1.RunnerState_RUNNER_STATE_DELETED)

	// A new order of the same session (a resumed session) is a new request:
	// tide deduplicates on the order ID, not the session ID.
	if code, _ := p.Spawn(claudefake.Spawn{OrderID: "order-2", SessionID: "session_01", Reason: "resumed", WorkOrder: claudeWorkOrder}); code != 0 {
		t.Fatalf("second order exit %d", code)
	}
	w.requestBySource(t, "order-2")

	all := w.guest.all()
	if strings.Contains(all, claudeEnvKey) || strings.Contains(all, claudeWorkOrder) {
		t.Fatal("an exec command or environment carries a Claude Code secret")
	}
	for _, f := range w.fake.Files(w.osOrg, sandboxID) {
		if bytes.Contains(f, []byte(claudeEnvKey)) {
			t.Fatal("the environment key reached a guest file")
		}
	}
	logs := w.env.Logs.String()
	if strings.Contains(logs, claudeEnvKey) || strings.Contains(logs, claudeWorkOrder) || strings.Contains(logs, "dev@example.com") {
		t.Fatal("a secret or the account e-mail reached the log")
	}
	if !strings.Contains(logs, `"msg":"claude_spawn_request"`) || !strings.Contains(logs, `"result":"duplicate"`) {
		t.Fatal("spawn requests are not logged")
	}
}

// TestClaudeHookExitCodes: the hook exits 2 (blocks the session) only for
// a spawn request that tide can never serve, and 1 (retry) for failures
// that a retry can fix.
func TestClaudeHookExitCodes(t *testing.T) {
	w := newClaudeWorld(t, tidev1.GuestOs_GUEST_OS_MACOS, 0)
	p := w.proc(t)
	for name, tc := range map[string]struct {
		spawn claudefake.Spawn
		code  int
	}{
		"pre-warming request":  {claudefake.Spawn{OrderID: "order-warm", Reason: "prewarm", WorkOrder: claudeWorkOrder}, 2},
		"no order ID":          {claudefake.Spawn{SessionID: "session_02", WorkOrder: claudeWorkOrder}, 2},
		"no work order file":   {claudefake.Spawn{OrderID: "order-nofile", SessionID: "session_02", NoWorkOrderFile: true}, 1},
		"an empty work order":  {claudefake.Spawn{OrderID: "order-empty", SessionID: "session_02"}, 1},
		"an invalid order ID":  {claudefake.Spawn{OrderID: "order with spaces", SessionID: "session_02", WorkOrder: claudeWorkOrder}, 2},
		"a valid spawn result": {claudefake.Spawn{OrderID: "order-ok", SessionID: "session_02", WorkOrder: claudeWorkOrder}, 0},
	} {
		if code, stderr := p.Spawn(tc.spawn); code != tc.code {
			t.Errorf("%s: exit %d (%s), want %d", name, code, stderr, tc.code)
		} else if strings.Contains(stderr, claudeWorkOrder) {
			t.Errorf("%s: the work order reached stderr", name)
		}
	}
	if n := len(w.requests(t)); n != 1 {
		t.Fatalf("%d requests", n)
	}
	// tide-api is not reachable (a stopped process): retryable.
	var stderr bytes.Buffer
	env := map[string]string{"CLAUDE_RUNNER_ORDER_ID": "o", "CLAUDE_RUNNER_SESSION_ID": "s", claude.SocketEnv: filepath.Join(t.TempDir(), "none")}
	file := filepath.Join(t.TempDir(), "wo")
	_ = os.WriteFile(file, []byte(claudeWorkOrder), 0o600)
	env["CLAUDE_RUNNER_WORK_ORDER_FILE"] = file
	if code := claude.RunHook(func(k string) string { return env[k] }, "/x/hooks/spawn-runner", &stderr); code != 1 {
		t.Fatalf("unreachable socket: exit %d", code)
	}
	// The socket variable, when the orchestrator passes it on, wins over
	// the hooks directory.
	env[claude.SocketEnv] = p.Env(claude.SocketEnv)
	env["CLAUDE_RUNNER_ORDER_ID"] = "order-env"
	if code := claude.RunHook(func(k string) string { return env[k] }, "/elsewhere/spawn-runner", &stderr); code != 0 {
		t.Fatalf("socket from the environment: exit %d %s", code, stderr.String())
	}
	if _, err := os.Stat(p.Env(claude.SocketEnv)); err != nil || filepath.Dir(p.Env(claude.SocketEnv)) != filepath.Dir(p.Arg("--hooks-dir")) {
		t.Fatalf("socket %q next to %q: %v", p.Env(claude.SocketEnv), p.Arg("--hooks-dir"), err)
	}
}

// TestClaudeWorkOrderExpiry: a request waits in the queue at most until
// its work order expires (the expected spawn time), even when the
// profile's maximum queue wait is longer.
func TestClaudeWorkOrderExpiry(t *testing.T) {
	w := newClaudeWorld(t, tidev1.GuestOs_GUEST_OS_LINUX, 2*time.Minute)
	w.full()
	p := w.proc(t)
	if code, _ := p.Spawn(claudefake.Spawn{OrderID: "order-exp", SessionID: "session_03", WorkOrder: claudeWorkOrder}); code != 0 {
		t.Fatal(code)
	}
	req := w.requestBySource(t, "order-exp")
	w.env.Clock.Advance(3 * time.Minute)
	r := w.waitRequest(t, req.GetRequestId(), tidev1.RequestState_REQUEST_STATE_EXPIRED)
	if r.GetEndReason() != tidev1.RequestEndReason_REQUEST_END_REASON_MAX_QUEUE_WAIT {
		t.Fatalf("end reason %v", r.GetEndReason())
	}
	if c := w.storedWorkOrder(t, req.GetRequestId()); c != nil {
		t.Fatal("the work order is still stored after the request ended")
	}
}

// TestClaudeStartFailureKeepsWorkOrder: a start failure before the runner
// received the work order (here the installation) returns the request to
// the queue with its stored work order, and the next runner uses it.
func TestClaudeStartFailureKeepsWorkOrder(t *testing.T) {
	w := newClaudeWorld(t, tidev1.GuestOs_GUEST_OS_LINUX, 0)
	w.guest.mu.Lock()
	w.guest.installCode = 12
	w.guest.mu.Unlock()
	p := w.proc(t)
	if code, _ := p.Spawn(claudefake.Spawn{OrderID: "order-retry", SessionID: "session_04", WorkOrder: claudeWorkOrder}); code != 0 {
		t.Fatal(code)
	}
	req := w.requestBySource(t, "order-retry")
	eventually(t, "a failed start", func() bool { return strings.Contains(w.env.Logs.String(), "the Claude Code installer failed") })
	if c := w.storedWorkOrder(t, req.GetRequestId()); c == nil {
		t.Fatal("the work order was dropped after a failure before the runner start")
	}
	w.guest.mu.Lock()
	w.guest.installCode = 0
	w.guest.mu.Unlock()
	eventually(t, "the next runner starts", func() bool {
		r := w.request(t, req.GetRequestId())
		return r.GetState() == tidev1.RequestState_REQUEST_STATE_RUNNING
	})
	if r := w.request(t, req.GetRequestId()); r.GetStartAttempts() < 2 {
		t.Fatalf("start attempts %d", r.GetStartAttempts())
	}
}

// TestClaudeOrchestratorRestart: a crashed orchestrator (or a failed
// start) is restarted with backoff and counted; a credential change
// restarts it with the new key; deleting the last profile stops it with
// SIGTERM.
func TestClaudeOrchestratorRestart(t *testing.T) {
	w := newClaudeWorld(t, tidev1.GuestOs_GUEST_OS_LINUX, 0)
	first := w.proc(t)
	w.orch.FailStarts(1)
	first.Crash()
	eventually(t, "a restarted orchestrator", func() bool { return len(w.orch.Procs()) == 2 && len(w.orch.Running()) == 1 })
	logs := w.env.Logs.String()
	if strings.Count(logs, `"msg":"claude_orchestrator_restart"`) < 2 || strings.Contains(logs, `"msg":"vendor_watch_reconnect"`) {
		t.Fatalf("restarts are not logged as orchestrator restarts")
	}

	second := w.proc(t)
	w.env.Clock.Advance(time.Second)
	if _, err := w.env.User(w.org.Admin).Integration.UpdateAgentIntegration(context.Background(), connect.NewRequest(&tidev1.UpdateAgentIntegrationRequest{
		OrganizationId: w.org.ID, AgentIntegrationId: w.integrationID,
		Credentials: &tidev1.UpdateAgentIntegrationRequest_ClaudeCodeCredentials{ClaudeCodeCredentials: &tidev1.ClaudeCodeCredentials{EnvironmentKey: "sk-ant-new-key"}},
		UpdateMask:  &fieldmaskpb.FieldMask{Paths: []string{"credentials"}}})); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the orchestrator restarts with the new key", func() bool {
		r := w.orch.Running()
		return second.Exited() && len(r) == 1 && r[0].Env("SELF_HOSTED_RUNNER_ENVIRONMENT_SECRET") == "sk-ant-new-key"
	})
	if s := second.Signals(); len(s) != 1 || s[0] != syscall.SIGTERM {
		t.Fatalf("signals %v", s)
	}
	third := w.proc(t)
	if _, err := w.env.User(w.org.Admin).Profile.DeleteRunnerProfile(context.Background(), connect.NewRequest(&tidev1.DeleteRunnerProfileRequest{
		OrganizationId: w.org.ID, RunnerProfileId: w.profileID})); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the orchestrator stops without a profile", func() bool { return third.Exited() && len(w.orch.Running()) == 0 })
	if _, err := os.Stat(filepath.Dir(third.Arg("--hooks-dir"))); !os.IsNotExist(err) {
		t.Fatalf("the run directory stays: %v", err)
	}
}

// TestClaudeOrganizationDeletion: deleting the organization stops the
// orchestrator and ends the queued request.
func TestClaudeOrganizationDeletion(t *testing.T) {
	w := newClaudeWorld(t, tidev1.GuestOs_GUEST_OS_LINUX, 0)
	w.full()
	p := w.proc(t)
	if code, _ := p.Spawn(claudefake.Spawn{OrderID: "order-org", SessionID: "session_05", WorkOrder: claudeWorkOrder}); code != 0 {
		t.Fatal(code)
	}
	w.requestBySource(t, "order-org")
	if _, err := w.env.User(w.org.Owner).Organization.DeleteOrganization(context.Background(), connect.NewRequest(&tidev1.DeleteOrganizationRequest{OrganizationId: w.org.ID})); err != nil {
		t.Fatal(err)
	}
	if !p.Exited() {
		t.Fatal("the orchestrator runs after the organization deletion")
	}
	stays(t, "no orchestrator", func() bool { return len(w.orch.Running()) == 0 })
}

// TestClaudeRealProcesses runs the production path with real processes: a
// shell script in place of claude starts the tide-api binary through the
// spawn-runner link in its hooks directory with only the documented hook
// variables, and the hook stores the request over the socket and exits 0.
func TestClaudeRealProcesses(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the tide-api binary")
	}
	bin := filepath.Join(t.TempDir(), "tide-api")
	if out, err := exec.Command("go", "build", "-o", bin, "github.com/delinoio/mono/servers/tide-api/cmd/tide-api").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	scratch := t.TempDir()
	script := filepath.Join(scratch, "claude")
	if err := os.WriteFile(script, []byte(`#!/bin/sh
hooks=
while [ $# -gt 0 ]; do
  if [ "$1" = --hooks-dir ]; then hooks=$2; fi
  shift
done
printf '%s' '`+claudeWorkOrder+`' > '`+scratch+`/wo'
env -i PATH=/usr/bin:/bin CLAUDE_RUNNER_ORDER_ID=order-real CLAUDE_RUNNER_SESSION_ID=session_real \
  CLAUDE_RUNNER_WORK_ORDER_FILE='`+scratch+`/wo' "$hooks/spawn-runner" 2>'`+scratch+`/stderr'
echo $? > '`+scratch+`/code'
trap 'exit 0' TERM
while :; do sleep 0.05; done
`), 0o755); err != nil {
		t.Fatal(err)
	}
	orchStart := claude.ExecStart
	w := newAgentWorld(t, layers.Options{Claude: claude.Options{ClaudePath: script, HookPath: bin, Start: orchStart, StopTimeout: 5 * time.Second}})
	id := w.createIntegration(t, &tidev1.CreateAgentIntegrationRequest{
		Vendor: tidev1.AgentVendor_AGENT_VENDOR_CLAUDE_CODE, Name: "claude",
		Settings:    &tidev1.CreateAgentIntegrationRequest_ClaudeCode{ClaudeCode: &tidev1.ClaudeCodeSettings{}},
		Credentials: &tidev1.CreateAgentIntegrationRequest_ClaudeCodeCredentials{ClaudeCodeCredentials: &tidev1.ClaudeCodeCredentials{EnvironmentKey: claudeEnvKey}},
	})
	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) {
		o.CreateError = connect.NewWireError(connect.CodeUnavailable, errors.New("full"))
	})
	w.createProfile(t, agentProfileProto("claude-agents", id, tidev1.GuestOs_GUEST_OS_LINUX, time.Hour))
	eventually(t, "the hook exit code", func() bool {
		b, err := os.ReadFile(filepath.Join(scratch, "code"))
		return err == nil && strings.TrimSpace(string(b)) != ""
	})
	code, _ := os.ReadFile(filepath.Join(scratch, "code"))
	stderr, _ := os.ReadFile(filepath.Join(scratch, "stderr"))
	if strings.TrimSpace(string(code)) != "0" {
		t.Fatalf("hook exit %s: %s", code, stderr)
	}
	w.requestBySource(t, "order-real")
}
