// SPDX-License-Identifier: FSL-1.1-ALv2

package layers_test

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/servers/tide-api/internal/agent"
	"github.com/delinoio/mono/servers/tide-api/internal/agent/cursor"
	"github.com/delinoio/mono/servers/tide-api/internal/agent/cursor/cursorfake"
	"github.com/delinoio/mono/servers/tide-api/internal/layers"
	"github.com/delinoio/mono/servers/tide-api/internal/ownsandclient/ownsandfake"
)

const cursorKey = "crsr_service_account_key_0123456789"

type cursorWorld struct {
	*agentWorld
	cursor        *cursorfake.Fake
	integrationID string
	profileID     string
}

func newCursorWorld(t *testing.T, guest tidev1.GuestOs) *cursorWorld {
	t.Helper()
	return newCursorWorldTiming(t, guest, agent.Timing{})
}

// newCursorWorldTiming is newCursorWorld with agent timing (zero: the fast
// test timing).
func newCursorWorldTiming(t *testing.T, guest tidev1.GuestOs, timing agent.Timing) *cursorWorld {
	t.Helper()
	fake := cursorfake.New(t, cursorKey, "tide-pool")
	w := newAgentWorld(t, layers.Options{Cursor: cursor.Options{BaseURL: fake.Server.URL, RelistInterval: time.Hour}, AgentTiming: timing})
	cw := &cursorWorld{agentWorld: w, cursor: fake}
	cw.integrationID = w.createIntegration(t, &tidev1.CreateAgentIntegrationRequest{
		Vendor: tidev1.AgentVendor_AGENT_VENDOR_CURSOR, Name: "cursor",
		Settings:    &tidev1.CreateAgentIntegrationRequest_Cursor{Cursor: &tidev1.CursorSettings{PoolName: "tide-pool"}},
		Credentials: &tidev1.CreateAgentIntegrationRequest_CursorCredentials{CursorCredentials: &tidev1.CursorCredentials{ServiceAccountApiKey: cursorKey}},
	})
	cw.profileID = w.createProfile(t, agentProfileProto("cursor-agents", cw.integrationID, guest, time.Hour))
	eventually(t, "the Cursor stream", func() bool { return fake.Streams() == 1 })
	return cw
}

// TestCursorRequestLifecycle: a pending request is claimed with a tide
// worker ID and becomes a request; the runner installs nothing new, gets
// the key only in its credential file, starts `agent worker ... start` with
// the claim's worker ID, becomes BUSY when the worker connects, and its
// exit ends the request (ENDED, COMPLETED) and the runner.
func TestCursorRequestLifecycle(t *testing.T) {
	w := newCursorWorld(t, tidev1.GuestOs_GUEST_OS_LINUX)
	w.cursor.AddRequest("bc-1", true)
	req := w.requestBySource(t, "bc-1")
	if req.GetKind() != tidev1.WorkloadKind_WORKLOAD_KIND_AGENT_SESSION || req.GetRunnerProfileId() != w.profileID {
		t.Fatalf("request %v", req)
	}
	workerID, ok := w.cursor.Claim("bc-1")
	if !ok || workerID != "tide-"+req.GetRequestId() {
		t.Fatalf("claim %q %v", workerID, ok)
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
	dir := agent.RunnerDir(tidev1.GuestOs_GUEST_OS_LINUX, runner.GetRunnerId())
	cred, ok := w.fake.File(w.osOrg, sandboxID, dir+"/"+agent.CredentialFile)
	if !ok || string(cred) != cursorKey {
		t.Fatalf("credential file %q %v", cred, ok)
	}
	script := w.guest.startScript(sandboxID)
	for _, want := range []string{
		"export CURSOR_AGENT_WORKER_ID='" + workerID + "'",
		"exec 'agent' 'worker' '--pool' 'tide-pool' '--worker-dir' \"$HOME\" '--idle-release-timeout' '600' 'start' >>'" + dir + "/runner.log' 2>&1",
		"CURSOR_API_KEY=$(cat '" + dir + "/credential')",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("start script lacks %q:\n%s", want, script)
		}
	}
	if strings.Contains(w.guest.all(), cursorKey) {
		t.Fatal("an exec command or environment carries the API key")
	}
	if w.request(t, req.GetRequestId()).GetState() != tidev1.RequestState_REQUEST_STATE_STARTING {
		t.Fatal("not STARTING before the worker connects")
	}
	w.cursor.Connect(workerID)
	w.waitRunner(t, runner.GetRunnerId(), tidev1.RunnerState_RUNNER_STATE_BUSY)
	w.waitRequest(t, req.GetRequestId(), tidev1.RequestState_REQUEST_STATE_RUNNING)
	w.cursor.Disconnect(workerID)
	w.guest.exitProgram(sandboxID, 0)
	r := w.waitRequest(t, req.GetRequestId(), tidev1.RequestState_REQUEST_STATE_ENDED)
	if r.GetEndReason() != tidev1.RequestEndReason_REQUEST_END_REASON_COMPLETED {
		t.Fatalf("end reason %v", r.GetEndReason())
	}
	run := w.waitRunner(t, runner.GetRunnerId(), tidev1.RunnerState_RUNNER_STATE_DELETED)
	if run.GetEndReason() != tidev1.RunnerEndReason_RUNNER_END_REASON_WORK_DONE {
		t.Fatalf("runner end %v", run.GetEndReason())
	}
	// The claim is done: Cursor freed it, so the release found no claim.
	eventually(t, "claim released", func() bool {
		c, err := w.l.Agents.Claim(context.Background(), req.GetRequestId())
		return err == nil && c.State == agent.ClaimReleased
	})
	if _, failed := w.cursor.Failed("bc-1"); failed {
		t.Fatal("a served request was failed")
	}
	for _, h := range w.cursor.AuthHeaders() {
		if !strings.HasPrefix(h, "Basic ") {
			t.Fatalf("auth header %q", h)
		}
	}
	if strings.Contains(w.env.Logs.String(), cursorKey) {
		t.Fatal("the API key reached the log")
	}
}

// TestCursorFollowUpBeforeRelease: when a follow-up of a request arrives
// before the claim poller released the claim of the ended request, tide
// releases the earlier claim before it claims the follow-up, so that a
// later poll does not release the follow-up's claim (Cursor identifies a
// claim only by the request ID).
func TestCursorFollowUpBeforeRelease(t *testing.T) {
	timing := fastAgentTiming()
	timing.PollInterval = time.Hour // the test runs the poller with Poll
	w := newCursorWorldTiming(t, tidev1.GuestOs_GUEST_OS_LINUX, timing)
	ctx := context.Background()
	w.cursor.AddRequest("bc-follow", true)
	first := w.requestBySource(t, "bc-follow")
	workerID := "tide-" + first.GetRequestId()
	var runner *tidev1.Runner
	eventually(t, "runner waits for work", func() bool {
		r := w.request(t, first.GetRequestId())
		if r.GetRunnerId() == "" {
			return false
		}
		runner = w.runner(t, r.GetRunnerId())
		return runner.GetState() == tidev1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK
	})
	w.cursor.Connect(workerID)
	w.l.Agents.Poll(ctx)
	w.waitRequest(t, first.GetRequestId(), tidev1.RequestState_REQUEST_STATE_RUNNING)
	// The worker exits after its idle release timeout; Cursor frees its
	// claim, and tide's claim row stays held until the next poll.
	w.cursor.Disconnect(workerID)
	w.guest.exitProgram(runner.GetOwnsandSandboxId(), 0)
	w.waitRequest(t, first.GetRequestId(), tidev1.RequestState_REQUEST_STATE_ENDED)
	if c, err := w.l.Agents.Claim(ctx, first.GetRequestId()); err != nil || c.State != agent.ClaimHeld {
		t.Fatalf("claim %+v %v", c, err)
	}
	w.cursor.AddRequest("bc-follow", true)
	second := w.requestBySource(t, "bc-follow#2")
	if c, err := w.l.Agents.Claim(ctx, first.GetRequestId()); err != nil || c.State != agent.ClaimReleased {
		t.Fatalf("earlier claim %+v %v", c, err)
	}
	w.l.Agents.Poll(ctx)
	if got, ok := w.cursor.Claim("bc-follow"); !ok || got != "tide-"+second.GetRequestId() {
		t.Fatalf("live claim %q %v", got, ok)
	}
	if n := w.cursor.Released("bc-follow"); n != 0 {
		t.Fatalf("%d releases of the follow-up's claim", n)
	}
}

// TestCursorDuplicateEvents: repeated created events and lists of the same
// pending request create one request and one claim.
func TestCursorDuplicateEvents(t *testing.T) {
	w := newCursorWorld(t, tidev1.GuestOs_GUEST_OS_LINUX)
	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) {
		o.CreateError = connect.NewWireError(connect.CodeUnavailable, errors.New("full"))
	})
	w.cursor.AddRequest("bc-dup", true)
	req := w.requestBySource(t, "bc-dup")
	for range 3 {
		w.cursor.Announce("bc-dup", 1)
	}
	w.cursor.DropStreams()
	eventually(t, "a new stream", func() bool { return w.cursor.Streams() == 1 })
	stays(t, "one request", func() bool {
		n := 0
		for _, r := range w.requests(t) {
			if strings.HasPrefix(r.GetSourceKey(), "bc-dup") {
				n++
			}
		}
		return n == 1 && w.cursor.ClaimCount("bc-dup") == 1
	})
	if w.request(t, req.GetRequestId()).GetState() != tidev1.RequestState_REQUEST_STATE_QUEUED {
		t.Fatalf("the request left the queue: %v", w.request(t, req.GetRequestId()))
	}
}

// TestCursorClaimEndsBeforeRunner: at the maximum queue wait and on a user
// cancellation, tide ends the waiting turn with `fail` and a message; a
// canceled entry that Cursor shows again is not claimed again.
func TestCursorClaimEndsBeforeRunner(t *testing.T) {
	w := newCursorWorld(t, tidev1.GuestOs_GUEST_OS_LINUX)
	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) {
		o.CreateError = connect.NewWireError(connect.CodeUnavailable, errors.New("full"))
	})

	w.cursor.AddRequest("bc-wait", true)
	waiting := w.requestBySource(t, "bc-wait")
	w.env.Clock.Advance(2 * time.Hour)
	r := w.waitRequest(t, waiting.GetRequestId(), tidev1.RequestState_REQUEST_STATE_EXPIRED)
	if r.GetEndReason() != tidev1.RequestEndReason_REQUEST_END_REASON_MAX_QUEUE_WAIT {
		t.Fatalf("end reason %v", r.GetEndReason())
	}
	eventually(t, "fail at max queue wait", func() bool {
		m, ok := w.cursor.Failed("bc-wait")
		return ok && strings.Contains(m, "maximum queue wait")
	})

	w.cursor.AddRequest("bc-cancel", true)
	canceled := w.requestBySource(t, "bc-cancel")
	w.cancel(t, canceled.GetRequestId())
	eventually(t, "fail at cancel", func() bool {
		m, ok := w.cursor.Failed("bc-cancel")
		return ok && strings.Contains(m, "canceled")
	})
	c, err := w.l.Agents.Claim(context.Background(), canceled.GetRequestId())
	if err != nil || c.State != agent.ClaimReleased {
		t.Fatalf("claim %+v %v", c, err)
	}
	// The same entry again: declined, so no second request.
	createdAt, _ := strconv.ParseInt(c.DemandMarker, 10, 64)
	w.cursor.Announce("bc-cancel", createdAt)
	stays(t, "no second request", func() bool {
		for _, r := range w.requests(t) {
			if r.GetSourceKey() == "bc-cancel#2" {
				return false
			}
		}
		return true
	})

	// Deleting the profile ends the queued request and fails its turn.
	w.cursor.AddRequest("bc-profile", true)
	queued := w.requestBySource(t, "bc-profile")
	if _, err := w.env.User(w.org.Admin).Profile.DeleteRunnerProfile(context.Background(), connect.NewRequest(&tidev1.DeleteRunnerProfileRequest{
		OrganizationId: w.org.ID, RunnerProfileId: w.profileID})); err != nil {
		t.Fatal(err)
	}
	if r := w.request(t, queued.GetRequestId()); r.GetEndReason() != tidev1.RequestEndReason_REQUEST_END_REASON_PROFILE_DELETED {
		t.Fatalf("request %v", r)
	}
	eventually(t, "fail at profile deletion", func() bool {
		m, ok := w.cursor.Failed("bc-profile")
		return ok && strings.Contains(m, "profile")
	})
	eventually(t, "the watch stops without a profile", func() bool { return w.cursor.Streams() == 0 })
}

func TestCursorClaimRejectedWithoutEligibleDevice(t *testing.T) {
	w := newCursorWorld(t, tidev1.GuestOs_GUEST_OS_LINUX)
	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) {
		o.CreateError = connect.NewWireError(connect.CodeFailedPrecondition, errors.New("no eligible device"))
	})
	w.cursor.AddRequest("bc-no-device", true)
	req := w.requestBySource(t, "bc-no-device")
	r := w.waitRequest(t, req.GetRequestId(), tidev1.RequestState_REQUEST_STATE_FAILED)
	if r.GetEndReason() != tidev1.RequestEndReason_REQUEST_END_REASON_NO_ELIGIBLE_DEVICE {
		t.Fatalf("request %v", r)
	}
	eventually(t, "vendor claim released for no eligible device", func() bool {
		claim, err := w.l.Agents.Claim(t.Context(), req.GetRequestId())
		return err == nil && claim.State == agent.ClaimReleased
	})
	if message, ok := w.cursor.Failed("bc-no-device"); !ok || !strings.Contains(message, "can run the runner profile") {
		t.Fatalf("Cursor failure: %q (reported: %t)", message, ok)
	}
}

// TestCursorIntegrationDeletionKeepsHeldClaims: when the release of a
// queued request's claim fails at the profile deletion, the integration
// cannot be deleted until the claim poller released the claim.
func TestCursorIntegrationDeletionKeepsHeldClaims(t *testing.T) {
	w := newCursorWorld(t, tidev1.GuestOs_GUEST_OS_LINUX)
	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) {
		o.CreateError = connect.NewWireError(connect.CodeUnavailable, errors.New("full"))
	})
	ctx := context.Background()
	w.cursor.AddRequest("bc-held", true)
	req := w.requestBySource(t, "bc-held")
	w.cursor.FailClaimCalls(true)
	if _, err := w.env.User(w.org.Admin).Profile.DeleteRunnerProfile(ctx, connect.NewRequest(&tidev1.DeleteRunnerProfileRequest{
		OrganizationId: w.org.ID, RunnerProfileId: w.profileID})); err != nil {
		t.Fatal(err)
	}
	del := func() error {
		_, err := w.env.User(w.org.Admin).Integration.DeleteAgentIntegration(ctx, connect.NewRequest(&tidev1.DeleteAgentIntegrationRequest{
			OrganizationId: w.org.ID, AgentIntegrationId: w.integrationID}))
		return err
	}
	if err := del(); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("delete with a held claim: %v", err)
	}
	w.cursor.FailClaimCalls(false)
	eventually(t, "the poller releases the claim", func() bool {
		c, err := w.l.Agents.Claim(ctx, req.GetRequestId())
		return err == nil && c.State == agent.ClaimReleased
	})
	if m, ok := w.cursor.Failed("bc-held"); !ok || !strings.Contains(m, "profile") {
		t.Fatalf("fail message %q %v", m, ok)
	}
	if err := del(); err != nil {
		t.Fatal(err)
	}
}

// TestCursorWatchReconnect: after the stream drops, tide reconnects with
// backoff, lists again and claims a request whose created event it missed.
func TestCursorWatchReconnect(t *testing.T) {
	w := newCursorWorld(t, tidev1.GuestOs_GUEST_OS_LINUX)
	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) {
		o.CreateError = connect.NewWireError(connect.CodeUnavailable, errors.New("full"))
	})
	lists := w.cursor.Lists()
	w.cursor.DropStreams()
	w.cursor.AddRequest("bc-missed", false)
	w.requestBySource(t, "bc-missed")
	if w.cursor.Lists() <= lists {
		t.Fatal("no list after the reconnect")
	}
	if !strings.Contains(w.env.Logs.String(), `"msg":"vendor_watch_reconnect","organization_id"`) || !strings.Contains(w.env.Logs.String(), `"cause":"error"`) {
		t.Fatal("the reconnect was not logged")
	}
}

// TestCursorWindowsNeedsGitBash: on Windows, Prepare fails clearly when
// the guest has no Git Bash, and the request returns to the queue.
func TestCursorWindowsNeedsGitBash(t *testing.T) {
	w := newCursorWorld(t, tidev1.GuestOs_GUEST_OS_WINDOWS)
	w.guest.mu.Lock()
	w.guest.installCode = 14
	w.guest.mu.Unlock()
	w.cursor.AddRequest("bc-win", true)
	req := w.requestBySource(t, "bc-win")
	eventually(t, "prepare failure logged", func() bool {
		return strings.Contains(w.env.Logs.String(), "Cursor workers on Windows need Git for Windows with Git Bash in the guest image")
	})
	eventually(t, "start attempts", func() bool { return w.request(t, req.GetRequestId()).GetStartAttempts() >= 1 })
	if !strings.Contains(w.guest.all(), "irm 'https://cursor.com/install?win32=true' | iex") {
		t.Fatal("no Windows install script")
	}
}

// TestCursorOrganizationDeletion: deleting the organization stops the
// watch and fails the queued request's turn while the key still exists; a
// failed release stops the deletion.
func TestCursorOrganizationDeletion(t *testing.T) {
	w := newCursorWorld(t, tidev1.GuestOs_GUEST_OS_LINUX)
	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) {
		o.CreateError = connect.NewWireError(connect.CodeUnavailable, errors.New("full"))
	})
	w.cursor.AddRequest("bc-org", true)
	req := w.requestBySource(t, "bc-org")
	// A release that fails stops the deletion and keeps the claim.
	w.cursor.FailClaimCalls(true)
	if _, err := w.env.User(w.org.Owner).Organization.DeleteOrganization(context.Background(), connect.NewRequest(&tidev1.DeleteOrganizationRequest{OrganizationId: w.org.ID})); err == nil {
		t.Fatal("the deletion went on after a failed release")
	}
	if c, err := w.l.Agents.Claim(context.Background(), req.GetRequestId()); err != nil || c.State != agent.ClaimHeld {
		t.Fatalf("claim %+v %v", c, err)
	}
	if _, ok := w.cursor.Claim("bc-org"); !ok {
		t.Fatal("the vendor claim is gone")
	}
	w.cursor.FailClaimCalls(false)
	if _, err := w.env.User(w.org.Owner).Organization.DeleteOrganization(context.Background(), connect.NewRequest(&tidev1.DeleteOrganizationRequest{OrganizationId: w.org.ID})); err != nil {
		t.Fatal(err)
	}
	if m, ok := w.cursor.Failed("bc-org"); !ok || !strings.Contains(m, "organization") {
		t.Fatalf("fail message %q %v", m, ok)
	}
	eventually(t, "the watch stops", func() bool { return w.cursor.Streams() == 0 })
}
