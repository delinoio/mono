// SPDX-License-Identifier: FSL-1.1-ALv2

package layers_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/servers/tide-api/internal/agent"
	"github.com/delinoio/mono/servers/tide-api/internal/agent/devin"
	"github.com/delinoio/mono/servers/tide-api/internal/agent/devin/devinfake"
	"github.com/delinoio/mono/servers/tide-api/internal/layers"
	"github.com/delinoio/mono/servers/tide-api/internal/ownsandclient/ownsandfake"
	"github.com/delinoio/mono/servers/tide-api/internal/work"
)

const (
	devinAPIToken     = "apk_devin_service_user_token_0123"
	devinOutpostToken = "dot_devin_outpost_token_4567"
)

type devinWorld struct {
	*agentWorld
	devin     *devinfake.Fake
	profileID string
}

func newDevinWorld(t *testing.T, guest tidev1.GuestOs) *devinWorld {
	t.Helper()
	fake := devinfake.New(t, devinAPIToken, "tide-outpost")
	w := newAgentWorld(t, layers.Options{Devin: devin.Options{BaseURL: fake.Server.URL, ReleaseGrace: time.Minute}})
	dw := &devinWorld{agentWorld: w, devin: fake}
	id := w.createIntegration(t, &tidev1.CreateAgentIntegrationRequest{
		Vendor: tidev1.AgentVendor_AGENT_VENDOR_DEVIN, Name: "devin",
		Settings: &tidev1.CreateAgentIntegrationRequest_Devin{Devin: &tidev1.DevinSettings{OutpostName: "tide-outpost"}},
		Credentials: &tidev1.CreateAgentIntegrationRequest_DevinCredentials{DevinCredentials: &tidev1.DevinCredentials{
			ApiToken: devinAPIToken, OutpostToken: devinOutpostToken}},
	})
	dw.profileID = w.createProfile(t, agentProfileProto("devin-agents", id, guest, time.Hour))
	eventually(t, "the Devin watch", func() bool { return fake.Streams() == 1 })
	return dw
}

func (w *devinWorld) full() {
	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) {
		o.CreateError = connect.NewWireError(connect.CodeUnavailable, errors.New("full"))
	})
}

// TestDevinSessionLifecycle: a pending session is claimed with a tide
// acceptor ID; the runner gets only the outpost token (in its credential
// file), starts `devin worker start --session ... --acceptor-id ...`,
// becomes BUSY when the session runs, and its exit ends the request; tide
// releases the claim after the session terminated. The API token never
// reaches the guest.
func TestDevinSessionLifecycle(t *testing.T) {
	w := newDevinWorld(t, tidev1.GuestOs_GUEST_OS_MACOS)
	w.devin.AddSession("ses-1", true)
	req := w.requestBySource(t, "ses-1")
	phase, acceptor := w.devin.State("ses-1")
	if phase != "claimed" || acceptor != "tide-"+req.GetRequestId() {
		t.Fatalf("queue entry %s %s", phase, acceptor)
	}
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
	dir := agent.RunnerDir(tidev1.GuestOs_GUEST_OS_MACOS, runner.GetRunnerId())
	files := w.fake.Files(w.osOrg, sandboxID)
	if len(files) != 1 || string(files[dir+"/"+agent.CredentialFile]) != devinOutpostToken {
		t.Fatalf("guest files %q", files)
	}
	script := w.guest.startScript(sandboxID)
	want := "exec 'devin' 'worker' 'start' '--session=ses-1' '--outpost=outpost_env-0001' '--acceptor-id=" + acceptor + "' >>'" + dir + "/runner.log' 2>&1"
	if !strings.Contains(script, want) || !strings.Contains(script, "DEVIN_OUTPOSTS_TOKEN=$(cat '"+dir+"/credential')") {
		t.Fatalf("start script:\n%s", script)
	}
	if strings.Contains(script, "--once") {
		t.Fatal("the worker uses --once")
	}
	all := w.guest.all()
	if strings.Contains(all, devinAPIToken) || strings.Contains(all, devinOutpostToken) {
		t.Fatal("an exec command or environment carries a Devin credential")
	}
	if !strings.Contains(all, "curl -fsSL https://cli.devin.ai/install.sh | bash") {
		t.Fatal("no install script")
	}
	w.devin.SetSessionStatus("ses-1", "running")
	w.waitRunner(t, runner.GetRunnerId(), tidev1.RunnerState_RUNNER_STATE_BUSY)
	w.waitRequest(t, req.GetRequestId(), tidev1.RequestState_REQUEST_STATE_RUNNING)
	w.devin.SetSessionStatus("ses-1", "terminated")
	w.guest.exitProgram(sandboxID, 0)
	r := w.waitRequest(t, req.GetRequestId(), tidev1.RequestState_REQUEST_STATE_ENDED)
	if r.GetEndReason() != tidev1.RequestEndReason_REQUEST_END_REASON_COMPLETED {
		t.Fatalf("end reason %v", r.GetEndReason())
	}
	w.waitRunner(t, runner.GetRunnerId(), tidev1.RunnerState_RUNNER_STATE_DELETED)
	eventually(t, "claim released", func() bool { return w.devin.Releases("ses-1") == 1 })
	if len(w.devin.Messages("ses-1")) != 0 {
		t.Fatal("operator message for a served session")
	}
	stays(t, "one request", func() bool { return len(w.requests(t)) == 1 })
	logs := w.env.Logs.String()
	if strings.Contains(logs, devinAPIToken) || strings.Contains(logs, devinOutpostToken) || strings.Contains(logs, "connect-token-secret") {
		t.Fatal("a credential reached the log")
	}
}

// TestDevinClaimRelease: at the maximum queue wait tide shows an operator
// message and releases the claim (the session returns to the queue and is
// new demand); after a user cancellation the same entry is not claimed
// again; an entry that another acceptor takes ends the queued request.
func TestDevinClaimRelease(t *testing.T) {
	w := newDevinWorld(t, tidev1.GuestOs_GUEST_OS_LINUX)
	w.full()
	w.devin.AddSession("ses-wait", true)
	waiting := w.requestBySource(t, "ses-wait")
	w.env.Clock.Advance(2 * time.Hour)
	w.waitRequest(t, waiting.GetRequestId(), tidev1.RequestState_REQUEST_STATE_EXPIRED)
	eventually(t, "release at max queue wait", func() bool { return w.devin.Releases("ses-wait") == 1 })
	if m := w.devin.Messages("ses-wait"); len(m) != 1 || !strings.Contains(m[0], "maximum queue wait") {
		t.Fatalf("operator messages %q", m)
	}
	// The released session is pending again: a new request takes it.
	again := w.requestBySource(t, "ses-wait#2")
	if again.GetState() != tidev1.RequestState_REQUEST_STATE_QUEUED {
		t.Fatalf("second request %v", again)
	}

	w.devin.AddSession("ses-cancel", true)
	canceled := w.requestBySource(t, "ses-cancel")
	w.cancel(t, canceled.GetRequestId())
	eventually(t, "release at cancel", func() bool { return w.devin.Releases("ses-cancel") == 1 })
	stays(t, "no request after the cancel", func() bool {
		for _, r := range w.requests(t) {
			if r.GetSourceKey() == "ses-cancel#2" {
				return false
			}
		}
		return true
	})

	w.devin.AddSession("ses-lost", true)
	lost := w.requestBySource(t, "ses-lost")
	w.devin.ExpireClaim("ses-lost")
	eventually(t, "claimed again", func() bool { return w.devin.Claims("ses-lost") == 2 })
	w.devin.ClaimElsewhere("ses-lost")
	r := w.waitRequest(t, lost.GetRequestId(), tidev1.RequestState_REQUEST_STATE_CANCELED)
	if r.GetEndReason() != tidev1.RequestEndReason_REQUEST_END_REASON_CANCELED_BY_SOURCE {
		t.Fatalf("end reason %v", r.GetEndReason())
	}
	if w.devin.Releases("ses-lost") != 0 {
		t.Fatal("tide released another acceptor's claim")
	}
}

// TestDevinSourceKeyAfterRetention: when the retention job deleted the
// first request of a session, the next request of the session still gets
// an unused source key.
func TestDevinSourceKeyAfterRetention(t *testing.T) {
	w := newDevinWorld(t, tidev1.GuestOs_GUEST_OS_LINUX)
	w.full()
	w.devin.AddSession("ses-keep", true)
	first := w.requestBySource(t, "ses-keep")
	w.env.Clock.Advance(2 * time.Hour)
	w.waitRequest(t, first.GetRequestId(), tidev1.RequestState_REQUEST_STATE_EXPIRED)
	second := w.requestBySource(t, "ses-keep#2")
	// The retention job deletes the first request (and its claim).
	if _, err := w.env.DB.Pool.Exec(context.Background(), "DELETE FROM requests WHERE request_id = $1", first.GetRequestId()); err != nil {
		t.Fatal(err)
	}
	w.env.Clock.Advance(2 * time.Hour)
	w.waitRequest(t, second.GetRequestId(), tidev1.RequestState_REQUEST_STATE_EXPIRED)
	third := w.requestBySource(t, "ses-keep#3")
	if third.GetState() != tidev1.RequestState_REQUEST_STATE_QUEUED {
		t.Fatalf("third request %v", third)
	}
}

// TestDevinCancelReleasedByPoller: when the claim poller releases the claim
// of a canceled request (as after a restart between the cancellation commit
// and its release step), the session that returns to the queue is still
// declined.
func TestDevinCancelReleasedByPoller(t *testing.T) {
	w := newDevinWorld(t, tidev1.GuestOs_GUEST_OS_LINUX)
	w.full()
	w.devin.AddSession("ses-recover", true)
	req := w.requestBySource(t, "ses-recover")
	ctx := context.Background()
	// The cancellation commits without its release step.
	err := w.env.DB.InTx(ctx, func(tx pgx.Tx) error {
		_, err := w.l.Engine.EndRequestTx(ctx, tx, req.GetRequestId(), work.RequestEnd{State: tidev1.RequestState_REQUEST_STATE_CANCELED,
			Reason: tidev1.RequestEndReason_REQUEST_END_REASON_CANCELED_BY_USER})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the poller releases the claim", func() bool { return w.devin.Releases("ses-recover") == 1 })
	stays(t, "no request after the cancel", func() bool {
		for _, r := range w.requests(t) {
			if r.GetSourceKey() == "ses-recover#2" {
				return false
			}
		}
		return true
	})
}

// TestDevinStartCheckPartialFailure: when the poller cannot read the queue
// entry of one STARTING session, it still marks the runner of another
// session BUSY when that session runs (a missed watch event).
func TestDevinStartCheckPartialFailure(t *testing.T) {
	w := newDevinWorld(t, tidev1.GuestOs_GUEST_OS_LINUX)
	waiting := func(sid string) *tidev1.Runner {
		req := w.requestBySource(t, sid)
		var runner *tidev1.Runner
		eventually(t, sid+" runner waits for work", func() bool {
			r := w.request(t, req.GetRequestId())
			if r.GetRunnerId() == "" {
				return false
			}
			runner = w.runner(t, r.GetRunnerId())
			return runner.GetState() == tidev1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK
		})
		return runner
	}
	w.devin.AddSession("ses-gone", true)
	waiting("ses-gone")
	w.devin.AddSession("ses-ok", true)
	ok := waiting("ses-ok")
	w.devin.Forget("ses-gone")
	w.devin.SetSessionStatusSilently("ses-ok", "running")
	w.waitRunner(t, ok.GetRunnerId(), tidev1.RunnerState_RUNNER_STATE_BUSY)
	if !strings.Contains(w.env.Logs.String(), "agent_session_check_failed") {
		t.Fatal("the failed check was not logged")
	}
}

// TestDevinWatchReconnect: a stream end (scheduled, at most 5 minutes) and
// failing watch calls (backoff) both lead to a new list, so a session whose
// event tide missed is claimed; repeated events create no second request.
func TestDevinWatchReconnect(t *testing.T) {
	w := newDevinWorld(t, tidev1.GuestOs_GUEST_OS_WINDOWS)
	w.full()
	lists := w.devin.Lists()
	w.devin.FailWatches(2)
	w.devin.EndStreams()
	w.devin.AddSession("ses-missed", false)
	req := w.requestBySource(t, "ses-missed")
	// One list after the scheduled end and one after each failed watch.
	eventually(t, "the watch is back", func() bool { return w.devin.Streams() == 1 && w.devin.Lists() >= lists+3 })
	logs := w.env.Logs.String()
	if !strings.Contains(logs, `"cause":"scheduled"`) || !strings.Contains(logs, `"cause":"error"`) {
		t.Fatal("reconnects were not logged")
	}
	for range 3 {
		w.devin.Announce("ses-missed")
	}
	stays(t, "one request", func() bool { return len(w.requests(t)) == 1 && w.devin.Claims("ses-missed") == 1 })
	if w.request(t, req.GetRequestId()).GetState() != tidev1.RequestState_REQUEST_STATE_QUEUED {
		t.Fatal("the request left the queue")
	}
}

func TestDevinInstallScripts(t *testing.T) {
	win := devin.InstallScript(tidev1.GuestOs_GUEST_OS_WINDOWS)
	for _, want := range []string{"irm https://static.devin.ai/cli/setup.ps1 | iex", "$env:PROCESSOR_ARCHITECTURE -ne 'AMD64'", "git.exe"} {
		if !strings.Contains(win, want) {
			t.Fatalf("windows script lacks %q", want)
		}
	}
	mac := devin.InstallScript(tidev1.GuestOs_GUEST_OS_MACOS)
	for _, want := range []string{"curl -fsSL https://cli.devin.ai/install.sh | bash", `"$(uname -m)" != arm64`, "command -v git"} {
		if !strings.Contains(mac, want) {
			t.Fatalf("posix script lacks %q", want)
		}
	}
}
