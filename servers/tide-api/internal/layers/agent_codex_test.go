// SPDX-License-Identifier: FSL-1.1-ALv2

package layers_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/servers/tide-api/internal/agent"
	"github.com/delinoio/mono/servers/tide-api/internal/agent/codex"
	"github.com/delinoio/mono/servers/tide-api/internal/agent/codex/codexfake"
	"github.com/delinoio/mono/servers/tide-api/internal/layers"
	"github.com/delinoio/mono/servers/tide-api/internal/ownsandclient/ownsandfake"
)

const (
	codexEnvKey = "sk-proj-codex-environment-key-0123"
	codexAPIKey = "sk-proj-codex-application-key-4567"
	// The webhook secret: whsec_ and base64 of 32 bytes.
	codexWebhookSecret = "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw+sKt9nqXC6E="
)

type codexWorld struct {
	*agentWorld
	openai        *codexfake.Fake
	sender        *codexfake.Sender
	integrationID string
	profileID     string
}

// newCodexWorld wires the Codex adapter with the guest OSes of the
// adapter's scripts, which the interim rule does not verify yet.
func newCodexWorld(t *testing.T, guest tidev1.GuestOs) *codexWorld {
	t.Helper()
	fake := codexfake.New(t, codexAPIKey)
	w := newAgentWorld(t, layers.Options{Codex: codex.Options{BaseURL: fake.Server.URL}, CodexGuestOSes: []tidev1.GuestOs{tidev1.GuestOs_GUEST_OS_LINUX, tidev1.GuestOs_GUEST_OS_MACOS}})
	cw := &codexWorld{agentWorld: w, openai: fake}
	cw.integrationID = w.createIntegration(t, &tidev1.CreateAgentIntegrationRequest{
		Vendor: tidev1.AgentVendor_AGENT_VENDOR_OPENAI_CODEX, Name: "codex",
		Settings: &tidev1.CreateAgentIntegrationRequest_OpenaiCodex{OpenaiCodex: &tidev1.OpenAiCodexSettings{}},
		Credentials: &tidev1.CreateAgentIntegrationRequest_OpenaiCodexCredentials{OpenaiCodexCredentials: &tidev1.OpenAiCodexCredentials{
			EnvironmentKey: codexEnvKey, WebhookSecret: codexWebhookSecret, ApiKey: codexAPIKey}},
	})
	cw.profileID = w.createProfile(t, agentProfileProto("codex-agents", cw.integrationID, guest, 5*time.Minute))
	cw.sender = &codexfake.Sender{URL: w.env.Server.URL + "/webhooks/openai/" + cw.integrationID, Secret: codexWebhookSecret, Now: w.env.Clock.Now,
		Client: w.env.Server.Client()}
	return cw
}

func (w *codexWorld) deliver(t *testing.T, id string, body []byte) int {
	t.Helper()
	return w.sender.Send(t, codexfake.Delivery{ID: id, Body: body})
}

// TestCodexSessionLifecycle: an environment_connection webhook makes tide
// read the session with the API key and queue a request keyed by the
// session and environment IDs; the runner gets only the environment key,
// creates the session workspace and starts `codex exec-server`; it becomes
// BUSY when the session no longer asks for a connection, and the executor
// exit ends the request. No webhook secret or API key reaches the guest,
// and no secret or body reaches the log.
func TestCodexSessionLifecycle(t *testing.T) {
	w := newCodexWorld(t, tidev1.GuestOs_GUEST_OS_LINUX)
	w.openai.AddSession("sess_1", "ccarenv_1")
	body := codexfake.ActionRequired("sess_1")
	if code := w.deliver(t, "wh_1", body); code != http.StatusOK {
		t.Fatalf("delivery %d", code)
	}
	req := w.requestBySource(t, "sess_1:ccarenv_1")
	// A repeated delivery ID changes nothing and makes no session read.
	reads := w.openai.Reads()
	if code := w.deliver(t, "wh_1", body); code != http.StatusOK || w.openai.Reads() != reads {
		t.Fatalf("duplicate %d, reads %d -> %d", code, reads, w.openai.Reads())
	}
	// A new delivery of the same connection request while the request is
	// open creates no second request.
	if code := w.deliver(t, "wh_2", body); code != http.StatusOK {
		t.Fatalf("second delivery %d", code)
	}
	stays(t, "one request", func() bool { return len(w.requests(t)) == 1 })

	var runner *tidev1.Runner
	eventually(t, "runner waits for work", func() bool {
		r := w.request(t, req.GetRequestId())
		if r.GetRunnerId() == "" {
			return false
		}
		runner = w.runner(t, r.GetRunnerId())
		return runner.GetState() == tidev1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK
	})
	sandboxID := runner.GetOwnsandSandboxId()
	dir := agent.RunnerDir(tidev1.GuestOs_GUEST_OS_LINUX, runner.GetRunnerId())
	files := w.fake.Files(w.osOrg, sandboxID)
	if len(files) != 1 || string(files[dir+"/"+agent.CredentialFile]) != codexEnvKey {
		t.Fatalf("guest files %q", files)
	}
	script := w.guest.startScript(sandboxID)
	for _, want := range []string{
		"CODEX_API_KEY=$(cat '" + dir + "/credential')",
		"exec 'codex' 'exec-server' '--remote' 'https://api.openai.test/v1/agents/api' '--environment-id' 'ccarenv_1' >>'" + dir + "/runner.log' 2>&1",
		`export PATH="$HOME/.local/bin:$HOME/.local/share/tide/node-v` + codex.NodeVersion + `/bin:$PATH"`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("start script lacks %q:\n%s", want, script)
		}
	}
	all := w.guest.all()
	for _, want := range []string{"mkdir -p '/tmp/codex-workspace'", `npm install -g "@openai/codex@` + codex.CodexVersion + `"`,
		"https://nodejs.org/dist/v" + codex.NodeVersion + "/$file"} {
		if !strings.Contains(all, want) {
			t.Fatalf("guest execs lack %q", want)
		}
	}
	for _, secret := range []string{codexEnvKey, codexAPIKey, codexWebhookSecret} {
		if strings.Contains(all, secret) {
			t.Fatal("an exec command or environment carries a Codex credential")
		}
	}
	for _, f := range w.fake.Files(w.osOrg, sandboxID) {
		if bytes.Contains(f, []byte(codexAPIKey)) || bytes.Contains(f, []byte(codexWebhookSecret)) {
			t.Fatal("a tide-only credential reached a guest file")
		}
	}
	stays(t, "STARTING while the session waits for the executor", func() bool {
		return w.request(t, req.GetRequestId()).GetState() == tidev1.RequestState_REQUEST_STATE_STARTING
	})
	w.openai.Update("sess_1", func(s *codexfake.Session) { s.NeedsConnection, s.Status = false, "in_progress" })
	w.waitRunner(t, runner.GetRunnerId(), tidev1.RunnerState_RUNNER_STATE_BUSY)
	w.waitRequest(t, req.GetRequestId(), tidev1.RequestState_REQUEST_STATE_RUNNING)
	w.guest.exitProgram(sandboxID, 0)
	r := w.waitRequest(t, req.GetRequestId(), tidev1.RequestState_REQUEST_STATE_ENDED)
	if r.GetEndReason() != tidev1.RequestEndReason_REQUEST_END_REASON_COMPLETED {
		t.Fatalf("end reason %v", r.GetEndReason())
	}
	w.waitRunner(t, runner.GetRunnerId(), tidev1.RunnerState_RUNNER_STATE_DELETED)

	// A later connection request of the same session is new demand.
	w.openai.Update("sess_1", func(s *codexfake.Session) { s.NeedsConnection, s.Status = true, "requires_action" })
	if code := w.deliver(t, "wh_3", body); code != http.StatusOK {
		t.Fatalf("third delivery %d", code)
	}
	w.requestBySource(t, "sess_1:ccarenv_1#2")

	logs := w.env.Logs.String()
	for _, secret := range []string{codexEnvKey, codexAPIKey, codexWebhookSecret, `"required_action"`} {
		if strings.Contains(logs, secret) {
			t.Fatalf("the log contains %q", secret)
		}
	}
	if !strings.Contains(logs, `"msg":"openai_webhook_delivery"`) || !strings.Contains(logs, `"result":"duplicate"`) {
		t.Fatal("deliveries are not logged")
	}
}

// TestCodexWebhookSignature: a missing, wrong or stale signature gets 401
// and creates nothing; a delivery signed with any listed v1 signature is
// accepted; deliveries that tide does not act on are recorded as ignored.
func TestCodexWebhookSignature(t *testing.T) {
	w := newCodexWorld(t, tidev1.GuestOs_GUEST_OS_MACOS)
	w.openai.AddSession("sess_sig", "ccarenv_sig")
	body := codexfake.ActionRequired("sess_sig")
	now := w.env.Clock.Now().Unix()
	for name, d := range map[string]codexfake.Delivery{
		"wrong signature":  {ID: "wh_bad", Body: body, Signature: "v1,AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="},
		"other version":    {ID: "wh_v2", Body: body, Signature: "v2,abc"},
		"stale timestamp":  {ID: "wh_old", Body: body, Timestamp: now - 301},
		"future timestamp": {ID: "wh_new", Body: body, Timestamp: now + 301},
	} {
		if code := w.sender.Send(t, d); code != http.StatusUnauthorized {
			t.Errorf("%s: %d", name, code)
		}
	}
	// A body changed after signing.
	other := &codexfake.Sender{URL: w.sender.URL, Secret: "whsec_" + "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", Now: w.env.Clock.Now, Client: w.env.Server.Client()}
	if code := other.Send(t, codexfake.Delivery{ID: "wh_key", Body: body}); code != http.StatusUnauthorized {
		t.Errorf("another secret: %d", code)
	}
	unknown := &codexfake.Sender{URL: w.env.Server.URL + "/webhooks/openai/019a0000-0000-7000-8000-000000000000", Secret: codexWebhookSecret,
		Now: w.env.Clock.Now, Client: w.env.Server.Client()}
	if code := unknown.Send(t, codexfake.Delivery{ID: "wh_unknown", Body: body}); code != http.StatusUnauthorized {
		t.Errorf("unknown integration: %d", code)
	}
	req, _ := http.NewRequest(http.MethodPost, w.sender.URL, bytes.NewReader(body))
	resp, err := w.env.Server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no headers: %d", resp.StatusCode)
	}
	if n := len(w.requests(t)); n != 0 || w.openai.Reads() != 0 {
		t.Fatalf("%d requests and %d session reads after invalid deliveries", n, w.openai.Reads())
	}
	// Within the tolerance, with a rotated secret listed first.
	if code := w.sender.Send(t, codexfake.Delivery{ID: "wh_ok", Body: body, Timestamp: now - 299, ExtraSignatures: []string{"v1,b2xkLXNlY3JldA=="}}); code != http.StatusOK {
		t.Fatalf("valid delivery %d", code)
	}
	w.requestBySource(t, "sess_sig:ccarenv_sig")
	// Other events and other actions are recorded and ignored.
	if code := w.deliver(t, "wh_idle", codexfake.Event("agent.session.idle", "sess_sig")); code != http.StatusOK {
		t.Fatalf("idle %d", code)
	}
	if code := w.deliver(t, "wh_fn", bytes.Replace(body, []byte("environment_connection"), []byte("function_call"), 1)); code != http.StatusOK {
		t.Fatalf("function_call %d", code)
	}
	// A signed body that does not parse.
	if code := w.deliver(t, "wh_json", []byte("{")); code != http.StatusBadRequest {
		t.Fatalf("bad JSON %d", code)
	}
	logs := w.env.Logs.String()
	if strings.Count(logs, `"result":"invalid_signature"`) < 7 || strings.Count(logs, `"result":"ignored"`) < 2 {
		t.Fatal("results are not logged")
	}
}

// TestCodexLookupFailureRedelivers: a failed session read answers 500 and
// records nothing, so OpenAI's retry of the same delivery ID creates the
// request; a deleted session is ignored.
func TestCodexLookupFailureRedelivers(t *testing.T) {
	w := newCodexWorld(t, tidev1.GuestOs_GUEST_OS_LINUX)
	w.openai.AddSession("sess_retry", "ccarenv_retry")
	w.openai.Fail(http.StatusInternalServerError)
	if code := w.deliver(t, "wh_retry", codexfake.ActionRequired("sess_retry")); code != http.StatusInternalServerError {
		t.Fatalf("failed lookup %d", code)
	}
	w.openai.Fail(0)
	if code := w.deliver(t, "wh_retry", codexfake.ActionRequired("sess_retry")); code != http.StatusOK {
		t.Fatalf("retry %d", code)
	}
	w.requestBySource(t, "sess_retry:ccarenv_retry")
	if code := w.deliver(t, "wh_gone", codexfake.ActionRequired("sess_gone")); code != http.StatusOK {
		t.Fatalf("deleted session %d", code)
	}
	stays(t, "one request", func() bool { return len(w.requests(t)) == 1 })
}

// TestCodexSessionFailedReleasesCompute: agent.session.failed for a
// session that is still failed ends its request and deletes its runner;
// a queued request ends with CANCELED.
func TestCodexSessionFailedReleasesCompute(t *testing.T) {
	w := newCodexWorld(t, tidev1.GuestOs_GUEST_OS_LINUX)
	w.openai.AddSession("sess_f", "ccarenv_f")
	if code := w.deliver(t, "wh_f1", codexfake.ActionRequired("sess_f")); code != http.StatusOK {
		t.Fatal(code)
	}
	req := w.requestBySource(t, "sess_f:ccarenv_f")
	var runnerID string
	eventually(t, "runner waits for work", func() bool {
		r := w.request(t, req.GetRequestId())
		runnerID = r.GetRunnerId()
		return runnerID != "" && w.runner(t, runnerID).GetState() == tidev1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK
	})
	// A failed event for a session that recovered changes nothing.
	if code := w.deliver(t, "wh_f2", codexfake.Event("agent.session.failed", "sess_f")); code != http.StatusOK {
		t.Fatal(code)
	}
	if r := w.request(t, req.GetRequestId()); r.GetState() != tidev1.RequestState_REQUEST_STATE_STARTING {
		t.Fatalf("request %v after a stale failed event", r.GetState())
	}
	w.openai.Update("sess_f", func(s *codexfake.Session) { s.Status, s.NeedsConnection = "failed", false })
	if code := w.deliver(t, "wh_f3", codexfake.Event("agent.session.failed", "sess_f")); code != http.StatusOK {
		t.Fatal(code)
	}
	r := w.waitRequest(t, req.GetRequestId(), tidev1.RequestState_REQUEST_STATE_FAILED)
	if r.GetEndReason() != tidev1.RequestEndReason_REQUEST_END_REASON_CANCELED_BY_SOURCE {
		t.Fatalf("end reason %v", r.GetEndReason())
	}
	w.waitRunner(t, runnerID, tidev1.RunnerState_RUNNER_STATE_DELETED)

	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) {
		o.CreateError = connect.NewWireError(connect.CodeUnavailable, errors.New("full"))
	})
	w.openai.AddSession("sess_q", "ccarenv_q")
	if code := w.deliver(t, "wh_q1", codexfake.ActionRequired("sess_q")); code != http.StatusOK {
		t.Fatal(code)
	}
	queued := w.requestBySource(t, "sess_q:ccarenv_q")
	w.openai.Update("sess_q", func(s *codexfake.Session) { s.Status, s.NeedsConnection = "failed", false })
	if code := w.deliver(t, "wh_q2", codexfake.Event("agent.session.failed", "sess_q")); code != http.StatusOK {
		t.Fatal(code)
	}
	w.waitRequest(t, queued.GetRequestId(), tidev1.RequestState_REQUEST_STATE_CANCELED)
}

// TestCodexMaxTTLEndsSession: the executor does not exit by itself, so a
// max_ttl end of the sandbox ends the request with SESSION_TTL_EXPIRED.
func TestCodexMaxTTLEndsSession(t *testing.T) {
	w := newCodexWorld(t, tidev1.GuestOs_GUEST_OS_LINUX)
	w.openai.AddSession("sess_ttl", "ccarenv_ttl")
	if code := w.deliver(t, "wh_ttl", codexfake.ActionRequired("sess_ttl")); code != http.StatusOK {
		t.Fatal(code)
	}
	req := w.requestBySource(t, "sess_ttl:ccarenv_ttl")
	var runner *tidev1.Runner
	eventually(t, "runner waits for work", func() bool {
		r := w.request(t, req.GetRequestId())
		if r.GetRunnerId() == "" {
			return false
		}
		runner = w.runner(t, r.GetRunnerId())
		return runner.GetState() == tidev1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK
	})
	w.fake.EndSandbox(w.osOrg, runner.GetOwnsandSandboxId(), ownsandv1.SandboxEndReason_SANDBOX_END_REASON_MAX_TTL_EXPIRED, ownsandv1.SandboxState_SANDBOX_STATE_DELETED)
	r := w.waitRequest(t, req.GetRequestId(), tidev1.RequestState_REQUEST_STATE_ENDED)
	if r.GetEndReason() != tidev1.RequestEndReason_REQUEST_END_REASON_SESSION_TTL_EXPIRED {
		t.Fatalf("end reason %v", r.GetEndReason())
	}
	if rr := w.waitRunner(t, runner.GetRunnerId(), tidev1.RunnerState_RUNNER_STATE_DELETED); rr.GetEndReason() != tidev1.RunnerEndReason_RUNNER_END_REASON_SANDBOX_ENDED {
		t.Fatalf("runner end %v", rr.GetEndReason())
	}
}

// TestCodexProfileGuestOS: without the test rule, the interim rule
// rejects every OPENAI_CODEX profile; the queue wait stays at most 5
// minutes.
func TestCodexProfileGuestOS(t *testing.T) {
	w := newAgentWorld(t, layers.Options{})
	id := w.createIntegration(t, &tidev1.CreateAgentIntegrationRequest{
		Vendor: tidev1.AgentVendor_AGENT_VENDOR_OPENAI_CODEX, Name: "codex",
		Settings: &tidev1.CreateAgentIntegrationRequest_OpenaiCodex{OpenaiCodex: &tidev1.OpenAiCodexSettings{}},
		Credentials: &tidev1.CreateAgentIntegrationRequest_OpenaiCodexCredentials{OpenaiCodexCredentials: &tidev1.OpenAiCodexCredentials{
			EnvironmentKey: codexEnvKey, WebhookSecret: codexWebhookSecret, ApiKey: codexAPIKey}},
	})
	_, err := w.env.User(w.org.Admin).Profile.CreateRunnerProfile(context.Background(), connect.NewRequest(&tidev1.CreateRunnerProfileRequest{
		OrganizationId: w.org.ID, Profile: agentProfileProto("codex", id, tidev1.GuestOs_GUEST_OS_LINUX, 5*time.Minute)}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "no guest OS is verified") {
		t.Fatalf("interim rule: %v", err)
	}
	cw := newCodexWorld(t, tidev1.GuestOs_GUEST_OS_LINUX)
	_, err = cw.env.User(cw.org.Admin).Profile.CreateRunnerProfile(context.Background(), connect.NewRequest(&tidev1.CreateRunnerProfileRequest{
		OrganizationId: cw.org.ID, Profile: agentProfileProto("codex-long", cw.integrationID, tidev1.GuestOs_GUEST_OS_LINUX, 6*time.Minute)}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("a queue wait over 5 minutes: %v", err)
	}
}
