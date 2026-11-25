// SPDX-License-Identifier: FSL-1.1-ALv2

package layers_test

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/servers/tide-api/internal/agent"
	"github.com/delinoio/mono/servers/tide-api/internal/agent/claude"
	"github.com/delinoio/mono/servers/tide-api/internal/agent/claude/claudefake"
	"github.com/delinoio/mono/servers/tide-api/internal/api"
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
	return newClaudeWorldWithProfile(t, agentProfileProto("claude-agents", "", guest, time.Hour), expectedSpawn)
}

func newClaudeWorldWithProfile(t *testing.T, profile *tidev1.RunnerProfile, expectedSpawn time.Duration) *claudeWorld {
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
	profile.AgentIntegrationId = cw.integrationID
	cw.profileID = w.createProfile(t, profile)
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

func TestClaudeRunnerStoredTTL(t *testing.T) {
	for _, tc := range []struct {
		name       string
		profileTTL time.Duration
		elapsed    time.Duration
		corrupt    bool
	}{
		{name: "raised profile TTL", profileTTL: 8 * time.Hour},
		{name: "lowered profile TTL", profileTTL: 30 * time.Minute},
		{name: "under a minute away", profileTTL: 8 * time.Hour, elapsed: 54*time.Minute + time.Second},
		{name: "corrupt create request", profileTTL: 8 * time.Hour, corrupt: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			profile := agentProfileProto("claude-agents", "", tidev1.GuestOs_GUEST_OS_LINUX, time.Hour)
			profile.WarmCount = 1
			profile.IdleTtl = durationpb.New(15 * time.Minute)
			profile.MaxTtl = durationpb.New(time.Hour)
			w := newClaudeWorldWithProfile(t, profile, 0)
			user := w.env.User(w.org.Admin)
			var warm *tidev1.Runner
			eventually(t, "one warm runner", func() bool {
				resp, err := user.Work.ListRunners(ctx, connect.NewRequest(&tidev1.ListRunnersRequest{
					OrganizationId: w.org.ID, RunnerProfileId: w.profileID,
					States: []tidev1.RunnerState{tidev1.RunnerState_RUNNER_STATE_WARM},
				}))
				if err != nil {
					t.Fatal(err)
				}
				if runners := resp.Msg.GetRunners(); len(runners) == 1 {
					warm = runners[0]
					return true
				}
				return false
			})
			if _, err := user.Profile.UpdateRunnerProfile(ctx, connect.NewRequest(&tidev1.UpdateRunnerProfileRequest{
				OrganizationId: w.org.ID, RunnerProfileId: w.profileID,
				Profile: &tidev1.RunnerProfile{MaxTtl: durationpb.New(tc.profileTTL)}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"max_ttl"}},
			})); err != nil {
				t.Fatal(err)
			}
			if tc.corrupt {
				if _, err := w.env.DB.Pool.Exec(ctx, "UPDATE runners SET create_request = $2 WHERE runner_id = $1", warm.GetRunnerId(), []byte{0xff}); err != nil {
					t.Fatal(err)
				}
			}
			w.env.Clock.Advance(tc.elapsed)
			if code, stderr := w.proc(t).Spawn(claudefake.Spawn{OrderID: "order-ttl", SessionID: "session_ttl", WorkOrder: claudeWorkOrder}); code != 0 {
				t.Fatalf("spawn exited %d: %s", code, stderr)
			}
			req := w.requestBySource(t, "order-ttl")
			req = w.waitRequest(t, req.GetRequestId(), tidev1.RequestState_REQUEST_STATE_RUNNING)
			if req.GetRunnerId() != warm.GetRunnerId() {
				t.Fatalf("request got runner %s, want warm runner %s", req.GetRunnerId(), warm.GetRunnerId())
			}
			script := w.guest.startScript(warm.GetOwnsandSandboxId())
			if script == "" {
				t.Fatal("no runner start script")
			}
			if tc.corrupt || tc.elapsed > 0 {
				if strings.Contains(script, "'--retire-at'") {
					t.Errorf("unexpected retirement argument in %s", script)
				}
			} else {
				retireAt := strconv.FormatInt(warm.GetCreatedAt().AsTime().Add(55*time.Minute).Unix(), 10)
				if !strings.Contains(script, "'--retire-at' '"+retireAt+"'") {
					t.Errorf("want --retire-at %s in %s", retireAt, script)
				}
			}
			if tc.corrupt && !strings.Contains(w.env.Logs.String(), "runner_create_request_load_failed") {
				t.Error("no warning for corrupt create request")
			}
		})
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
	retry := make(chan struct{})
	allowRetry := sync.OnceFunc(func() { close(retry) })
	t.Cleanup(allowRetry)
	var creates atomic.Int32
	w.fake.SetCreateHook(func(ctx context.Context) error {
		if creates.Add(1) == 1 {
			return nil
		}
		// Keep retries from consuming the start budget while the fixture
		// checks the work order left by the failed first attempt.
		select {
		case <-retry:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
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
	allowRetry()
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
	// The watch removes the run directory after the process has exited and
	// the hook socket has closed, so wait for the removal as well.
	runDir := filepath.Dir(third.Arg("--hooks-dir"))
	eventually(t, "the run directory is removed", func() bool {
		_, err := os.Stat(runDir)
		return os.IsNotExist(err)
	})
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

func TestClaudeOrganizationDeletionWaitsForDrainingWatcher(t *testing.T) {
	script := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(script, []byte(`#!/bin/sh
trap '
  : > "$TIDE_SPAWN_SOCKET.term"
  while [ ! -f "$TIDE_SPAWN_SOCKET.release" ]; do sleep 0.02; done
  exit 0
' TERM
: > "$TIDE_SPAWN_SOCKET.ready"
while :; do sleep 0.02; done
`), 0o755); err != nil {
		t.Fatal(err)
	}
	started := make(chan string, 4)
	w := newAgentWorld(t, layers.Options{Claude: claude.Options{
		ClaudePath: script, HookPath: claudeHookPath, StopTimeout: 30 * time.Second,
		Start: func(spec claude.ProcessSpec) (claude.Process, error) {
			p, err := claude.ExecStart(spec)
			if err == nil {
				for _, entry := range spec.Env {
					if socket, ok := strings.CutPrefix(entry, "TIDE_SPAWN_SOCKET="); ok {
						started <- socket
						break
					}
				}
			}
			return p, err
		},
	}})
	otherWorld := &world{env: w.env, fake: w.fake, l: w.l, org: w.env.NewOrg("bob")}
	other := &agentWorld{workWorld: &workWorld{world: otherWorld, osOrg: api.NewID()}}
	w.fake.AddOrg(other.osOrg, ownsandKey('b'))
	if _, err := other.setLink(other.org.Admin, other.osOrg, ownsandKey('b')); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	waitFile := func(path string) {
		t.Helper()
		eventually(t, filepath.Base(path), func() bool {
			_, err := os.Stat(path)
			return err == nil
		})
	}
	start := func(w *agentWorld) (string, string, string) {
		t.Helper()
		integrationID := w.createIntegration(t, &tidev1.CreateAgentIntegrationRequest{
			Vendor: tidev1.AgentVendor_AGENT_VENDOR_CLAUDE_CODE, Name: "claude",
			Settings:    &tidev1.CreateAgentIntegrationRequest_ClaudeCode{ClaudeCode: &tidev1.ClaudeCodeSettings{}},
			Credentials: &tidev1.CreateAgentIntegrationRequest_ClaudeCodeCredentials{ClaudeCodeCredentials: &tidev1.ClaudeCodeCredentials{EnvironmentKey: claudeEnvKey}},
		})
		profileID := w.createProfile(t, agentProfileProto("claude-agents", integrationID, tidev1.GuestOs_GUEST_OS_LINUX, time.Hour))
		var socket string
		select {
		case socket = <-started:
		case <-ctx.Done():
			t.Fatal("orchestrator did not start")
		}
		t.Cleanup(func() { _ = os.WriteFile(socket+".release", nil, 0o600) })
		waitFile(socket + ".ready")
		return integrationID, profileID, socket
	}
	socketOpen := func(socket string) bool {
		conn, err := net.DialTimeout("unix", socket, time.Second)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	}
	deleteOrg := func(w *agentWorld) <-chan error {
		result := make(chan error, 1)
		go func() {
			_, err := w.env.User(w.org.Owner).Organization.DeleteOrganization(ctx,
				connect.NewRequest(&tidev1.DeleteOrganizationRequest{OrganizationId: w.org.ID}))
			result <- err
		}()
		return result
	}
	assertPending := func(result <-chan error) {
		t.Helper()
		select {
		case err := <-result:
			t.Fatalf("DeleteOrganization returned during the orchestrator drain: %v", err)
		default:
		}
	}
	rowsExist := func(w *agentWorld, integrationID string) (bool, bool) {
		t.Helper()
		var org, credential bool
		err := w.env.DB.Pool.QueryRow(ctx, `SELECT
			EXISTS (SELECT 1 FROM organizations WHERE organization_id = $1),
			EXISTS (SELECT 1 FROM agent_integration_credentials WHERE agent_integration_id = $2)`,
			w.org.ID, integrationID).Scan(&org, &credential)
		if err != nil {
			t.Fatal(err)
		}
		return org, credential
	}
	releaseAndVerify := func(w *agentWorld, integrationID, socket string, result <-chan error) {
		t.Helper()
		if err := os.WriteFile(socket+".release", nil, 0o600); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-result:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal("DeleteOrganization did not finish after release")
		}
		if socketOpen(socket) {
			t.Fatal("the hook socket is open after DeleteOrganization returned")
		}
		if org, credential := rowsExist(w, integrationID); org || credential {
			t.Fatalf("after deletion: organization=%t, credentials=%t", org, credential)
		}
	}

	id, profile, socket := start(w)
	otherID, _, otherSocket := start(other)
	if _, err := w.env.User(w.org.Admin).Profile.DeleteRunnerProfile(ctx, connect.NewRequest(&tidev1.DeleteRunnerProfileRequest{
		OrganizationId: w.org.ID, RunnerProfileId: profile,
	})); err != nil {
		t.Fatal(err)
	}
	waitFile(socket + ".term")
	result := deleteOrg(w)
	eventually(t, "the link is marked removing", func() bool {
		var removing bool
		err := w.env.DB.Pool.QueryRow(ctx, "SELECT removing FROM ownsand_links WHERE organization_id = $1", w.org.ID).Scan(&removing)
		assertPending(result)
		return err == nil && removing
	})
	stays(t, "the organization and credentials during the drain", func() bool {
		assertPending(result)
		org, credential := rowsExist(w, id)
		return org && credential && socketOpen(socket)
	})
	t.Log("the owner RPC stayed pending while the link was marked removing, the organization and credential rows existed, and the hook socket accepted connections")

	otherResult := deleteOrg(other)
	waitFile(otherSocket + ".term")
	assertPending(otherResult)
	releaseAndVerify(other, otherID, otherSocket, otherResult)
	assertPending(result)
	if !socketOpen(socket) {
		t.Fatal("the first organization's hook socket closed before its orchestrator was released")
	}
	t.Log("deletion stopped the second organization's active watcher and completed while the first watcher was still draining")

	releaseAndVerify(w, id, socket, result)
	t.Log("after process release both owner RPCs succeeded, their hook sockets were closed, and their organization and credential rows were gone")
}

// TestClaudeRealProcesses runs the production path with real processes: a
// shell script in place of claude starts the tide-api binary through the
// spawn-runner link in its hooks directory with only the documented hook
// variables. The hooks store requests over the socket and exit 0 during
// the normal run and after the orchestrator receives SIGTERM.
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
spawn() {
  env -i PATH=/usr/bin:/bin CLAUDE_RUNNER_ORDER_ID="$1" CLAUDE_RUNNER_SESSION_ID=session_real \
    CLAUDE_RUNNER_WORK_ORDER_FILE='`+scratch+`/wo' "$hooks/spawn-runner" 2>"`+scratch+`/$2.stderr"
  echo $? > "`+scratch+`/$2.code"
}
trap 'spawn order-drain drain; exit 0' TERM
spawn order-real normal
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
	checkHook := func(phase, order string) {
		t.Helper()
		var code string
		eventually(t, "the "+phase+" hook exit code", func() bool {
			b, err := os.ReadFile(filepath.Join(scratch, phase+".code"))
			code = strings.TrimSpace(string(b))
			return err == nil && code != ""
		})
		if code != "0" {
			stderr, _ := os.ReadFile(filepath.Join(scratch, phase+".stderr"))
			t.Fatalf("%s hook exit %s: %s", phase, code, stderr)
		}
		w.requestBySource(t, order)
	}
	checkHook("normal", "order-real")
	// Rotating the credential stops the orchestrator while the profile still
	// serves requests, so a hook invoked during the drain can store its order.
	w.env.Clock.Advance(time.Second)
	if _, err := w.env.User(w.org.Admin).Integration.UpdateAgentIntegration(t.Context(), connect.NewRequest(&tidev1.UpdateAgentIntegrationRequest{
		OrganizationId: w.org.ID, AgentIntegrationId: id,
		Credentials: &tidev1.UpdateAgentIntegrationRequest_ClaudeCodeCredentials{ClaudeCodeCredentials: &tidev1.ClaudeCodeCredentials{EnvironmentKey: "sk-ant-new-key"}},
		UpdateMask:  &fieldmaskpb.FieldMask{Paths: []string{"credentials"}},
	})); err != nil {
		t.Fatal(err)
	}
	checkHook("drain", "order-drain")
}

func TestClaudeInheritedOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix processes and signals")
	}
	for _, mode := range []string{"graceful", "forced", "unexpected"} {
		t.Run(mode, func(t *testing.T) {
			scratch := t.TempDir()
			script := filepath.Join(scratch, "claude")
			if err := os.WriteFile(script, []byte(`#!/bin/sh
cd "${0%/*}" || exit 1
mode=`+mode+`
trap 'touch terminated; exit 0' TERM
if [ -f ready ]; then
  printf '%s\n' "$TIDE_SPAWN_SOCKET" > restarted
else
  # This helper inherits stdout and stderr and outlives the watcher.
  sleep 120 &
  if [ "$mode" = forced ]; then trap '' TERM; fi
  printf '%s\n' "$TIDE_SPAWN_SOCKET" "$$" "$!" > starting
  mv starting ready
  if [ "$mode" = unexpected ]; then
    while [ ! -f crash ]; do sleep 0.05; done
    exit 17
  fi
fi
while :; do sleep 0.05; done
`), 0o755); err != nil {
				t.Fatal(err)
			}
			stopTimeout := 5 * time.Second
			if mode == "forced" {
				stopTimeout = 500 * time.Millisecond
			}
			w := newAgentWorld(t, layers.Options{Claude: claude.Options{ClaudePath: script, Start: claude.ExecStart, StopTimeout: stopTimeout}})
			id := w.createIntegration(t, &tidev1.CreateAgentIntegrationRequest{
				Vendor: tidev1.AgentVendor_AGENT_VENDOR_CLAUDE_CODE, Name: "claude",
				Settings:    &tidev1.CreateAgentIntegrationRequest_ClaudeCode{ClaudeCode: &tidev1.ClaudeCodeSettings{}},
				Credentials: &tidev1.CreateAgentIntegrationRequest_ClaudeCodeCredentials{ClaudeCodeCredentials: &tidev1.ClaudeCodeCredentials{EnvironmentKey: claudeEnvKey}},
			})
			profileID := w.createProfile(t, agentProfileProto("claude-agents", id, tidev1.GuestOs_GUEST_OS_LINUX, time.Hour))
			var first []string
			eventually(t, "the orchestrator and its output helper", func() bool {
				b, err := os.ReadFile(filepath.Join(scratch, "ready"))
				first = strings.Split(strings.TrimSpace(string(b)), "\n")
				return err == nil && len(first) == 3
			})
			helperPID, err := strconv.Atoi(first[2])
			if err != nil {
				t.Fatal(err)
			}
			helper, err := os.FindProcess(helperPID)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = helper.Kill()
				_ = helper.Release()
			})
			leaderPID, err := strconv.Atoi(first[1])
			if err != nil {
				t.Fatal(err)
			}
			leader, err := os.FindProcess(leaderPID)
			if err != nil {
				t.Fatal(err)
			}
			defer leader.Release()

			began := time.Now()
			if mode == "unexpected" {
				if err := os.WriteFile(filepath.Join(scratch, "crash"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
				eventually(t, "a replacement orchestrator with a new socket", func() bool {
					b, err := os.ReadFile(filepath.Join(scratch, "restarted"))
					socket := strings.TrimSpace(string(b))
					if err != nil || socket == first[0] {
						return false
					}
					info, err := os.Stat(socket)
					return err == nil && info.Mode()&os.ModeSocket != 0
				})
				if !strings.Contains(w.env.Logs.String(), "exit status 17") {
					t.Fatal("the restart did not report the orchestrator exit status")
				}
			} else {
				if _, err := w.env.User(w.org.Admin).Profile.DeleteRunnerProfile(t.Context(), connect.NewRequest(&tidev1.DeleteRunnerProfileRequest{
					OrganizationId: w.org.ID, RunnerProfileId: profileID,
				})); err != nil {
					t.Fatal(err)
				}
				eventually(t, "the stopped watcher", func() bool {
					return strings.Contains(w.env.Logs.String(), `"msg":"vendor_watch_stopped"`)
				})
				_, err := os.Stat(filepath.Join(scratch, "terminated"))
				if mode == "graceful" && err != nil {
					t.Fatalf("the leader should finish its SIGTERM handler: %v", err)
				}
			}
			if _, err := os.Stat(filepath.Dir(first[0])); !os.IsNotExist(err) {
				t.Fatalf("the old run directory should be removed: %v", err)
			}
			if err := leader.Signal(syscall.Signal(0)); err == nil {
				t.Fatal("the orchestrator leader remains alive")
			}
			if err := helper.Signal(syscall.Signal(0)); err != nil {
				t.Fatalf("the output helper should still be alive: %v", err)
			}
			t.Logf("watcher finished after %s with its output helper still alive", time.Since(began).Round(time.Millisecond))
		})
	}
}
