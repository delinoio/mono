// SPDX-License-Identifier: FSL-1.1-ALv2

package layers_test

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/servers/tide-api/internal/agent"
	"github.com/delinoio/mono/servers/tide-api/internal/agent/cursor"
	"github.com/delinoio/mono/servers/tide-api/internal/agent/cursor/cursorfake"
	"github.com/delinoio/mono/servers/tide-api/internal/app"
	"github.com/delinoio/mono/servers/tide-api/internal/db"
	"github.com/delinoio/mono/servers/tide-api/internal/layers"
	"github.com/delinoio/mono/servers/tide-api/internal/ownsandclient/ownsandfake"
	"github.com/delinoio/mono/servers/tide-api/internal/testenv"
	"github.com/delinoio/mono/servers/tide-api/internal/work"
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
	w := startCursorWorld(t, guest, timing, fake)
	eventually(t, "the Cursor stream", func() bool { return fake.Streams() == 1 })
	return w
}

func startCursorWorld(t *testing.T, guest tidev1.GuestOs, timing agent.Timing, fake *cursorfake.Fake, opts ...testenv.Option) *cursorWorld {
	t.Helper()
	w := newAgentWorld(t, layers.Options{Cursor: cursor.Options{BaseURL: fake.Server.URL, RelistInterval: time.Hour}, AgentTiming: timing}, opts...)
	cw := &cursorWorld{agentWorld: w, cursor: fake}
	cw.integrationID = w.createIntegration(t, &tidev1.CreateAgentIntegrationRequest{
		Vendor: tidev1.AgentVendor_AGENT_VENDOR_CURSOR, Name: "cursor",
		Settings:    &tidev1.CreateAgentIntegrationRequest_Cursor{Cursor: &tidev1.CursorSettings{PoolName: "tide-pool"}},
		Credentials: &tidev1.CreateAgentIntegrationRequest_CursorCredentials{CursorCredentials: &tidev1.CursorCredentials{ServiceAccountApiKey: cursorKey}},
	})
	cw.profileID = w.createProfile(t, agentProfileProto("cursor-agents", cw.integrationID, guest, time.Hour))
	return cw
}

func TestCursorClaimDuringProfileDeletion(t *testing.T) {
	w := newCursorWorld(t, tidev1.GuestOs_GUEST_OS_LINUX)
	const id = "bc-deleted-profile"
	commit := holdTx(t, w.env, "DELETE FROM runner_profiles WHERE runner_profile_id = $1", w.profileID)
	w.cursor.AddRequest(id, true)
	waitForLockWaiter(t, w.env)
	if n := w.cursor.ClaimCount(id); n != 1 {
		t.Fatalf("%d claims while admission waits for the profile, want 1", n)
	}
	commit()
	eventually(t, "release of the unused vendor claim", func() bool { return w.cursor.Released(id) == 1 })
	if _, held := w.cursor.Claim(id); held {
		t.Error("the vendor claim is still active")
	}
	w.assertNoRequestsOrClaims(t)
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

func TestCursorClaimExpiresAtRunnerBinding(t *testing.T) {
	for _, tc := range []struct {
		name      string
		raceSweep bool
	}{
		{"binding", false},
		{"competing_sweep", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			w := newCursorWorld(t, tidev1.GuestOs_GUEST_OS_LINUX)
			wait, resume := holdCreateSandbox(t, w.fake, 1)
			const key = "bc-binding-deadline"
			w.cursor.AddRequest(key, true)
			wait()
			req := w.requestBySource(t, key)
			if _, ok := w.cursor.Claim(key); !ok || req.GetState() != tidev1.RequestState_REQUEST_STATE_QUEUED {
				t.Fatalf("expected a held claim and queued request, got %v", req)
			}

			if tc.raceSweep {
				commit := holdTx(t, w.env, "SELECT 1 FROM requests WHERE request_id = $1 FOR UPDATE", req.GetRequestId())
				w.env.Clock.Advance(time.Hour)
				resume()
				waitForLockWaiter(t, w.env)
				done := make(chan struct{})
				go func() {
					defer close(done)
					w.l.Engine.Pass(ctx, w.org.ID)
				}()
				eventually(t, "binding and the sweep both wait for the request lock", func() bool {
					var n int
					err := w.env.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
						WHERE datname = current_database() AND wait_event_type = 'Lock'
						AND query LIKE '%FROM requests WHERE request_id = $1 FOR UPDATE%'`).Scan(&n)
					if err != nil {
						t.Fatal(err)
					}
					return n == 2
				})
				commit()
				<-done
			} else {
				w.env.Clock.Advance(time.Hour)
				resume()
			}

			assertQueueExpiry(t, w.waitRequest(t, req.GetRequestId(), tidev1.RequestState_REQUEST_STATE_EXPIRED))
			eventually(t, "the claim is released after expiry", func() bool {
				claim, err := w.l.Agents.Claim(ctx, req.GetRequestId())
				message, ok := w.cursor.Failed(key)
				return err == nil && claim.State == agent.ClaimReleased && ok && strings.Contains(message, "maximum queue wait")
			})
			assertUnusedSandboxDeleted(t, w.fake, w.osOrg)
			if got := w.cursor.FailAttempts(key); got != 1 {
				t.Fatalf("vendor failure notifications: %d, want 1", got)
			}
		})
	}
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

func TestCursorWatchPaginationRepeats(t *testing.T) {
	const (
		firstToken  = "page-token-A"
		secondToken = "page-token-B"
		backoff     = 100 * time.Millisecond
	)
	for _, tc := range []struct {
		name  string
		extra []cursorfake.Page
		want  []string
	}{
		{name: "same token", want: []string{"", firstToken, "", firstToken}},
		{name: "cycle", extra: []cursorfake.Page{{NextPageToken: secondToken}}, want: []string{"", firstToken, secondToken, "", firstToken}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := cursorfake.New(t, cursorKey, "tide-pool")
			for _, id := range []string{"earlier", "offending", "recovered"} {
				fake.AddRequest(id, false)
			}
			pages := []cursorfake.Page{{Requests: []cursorfake.Request{{ID: "earlier"}}, NextPageToken: firstToken}}
			pages = append(pages, tc.extra...)
			pages = append(pages,
				cursorfake.Page{Requests: []cursorfake.Request{{ID: "offending"}}, NextPageToken: firstToken, StreamCursor: "invalid-cursor"},
				cursorfake.Page{NextPageToken: firstToken},
				cursorfake.Page{Requests: []cursorfake.Request{{ID: "recovered"}}, StreamCursor: "recovered-cursor"},
			)
			fake.ScriptPages(pages...)
			timing := fastAgentTiming()
			timing.BackoffMin = backoff
			w := startCursorWorld(t, tidev1.GuestOs_GUEST_OS_LINUX, timing, fake)
			eventually(t, "the Cursor recovery stream", func() bool { return fake.Streams() == 1 })

			lists, claims, streams := fake.WatchCalls()
			var tokens []string
			for _, call := range lists {
				tokens = append(tokens, call.PageToken)
			}
			if !slices.Equal(tokens, tc.want) {
				t.Fatalf("list tokens = %q, want %q", tokens, tc.want)
			}
			if !slices.Equal(claims, []string{"earlier", "recovered"}) || !slices.Equal(streams, []string{"recovered-cursor"}) {
				t.Fatalf("claim calls = %q, stream cursors = %q", claims, streams)
			}
			offending := 1 + len(tc.extra)
			if elapsed := lists[offending+1].At.Sub(lists[offending].At); elapsed < backoff {
				t.Errorf("fresh list started after %v, want at least %v", elapsed, backoff)
			}
			logs := w.env.Logs.String()
			if !strings.Contains(logs, `"msg":"vendor_watch_reconnect"`) || !strings.Contains(logs, `"cause":"error"`) {
				t.Error("the error reconnect was not logged")
			}
			if strings.Contains(logs, firstToken) || strings.Contains(logs, secondToken) {
				t.Error("a page token appeared in the logs")
			}
		})
	}
}

func TestCursorWatchPaginationUsesFinalCursor(t *testing.T) {
	fake := cursorfake.New(t, cursorKey, "tide-pool")
	fake.AddRequest("first", false)
	fake.AddRequest("second", false)
	fake.ScriptPages(
		cursorfake.Page{Requests: []cursorfake.Request{{ID: "first"}}, NextPageToken: "next", StreamCursor: "first-cursor"},
		cursorfake.Page{Requests: []cursorfake.Request{{ID: "second"}}, StreamCursor: "final-cursor"},
	)
	w := startCursorWorld(t, tidev1.GuestOs_GUEST_OS_LINUX, agent.Timing{}, fake)
	eventually(t, "the Cursor stream", func() bool { return fake.Streams() == 1 })

	lists, claims, streams := fake.WatchCalls()
	if len(lists) != 2 || lists[0].PageToken != "" || lists[1].PageToken != "next" {
		t.Errorf("list calls = %v, want the initial page then next", lists)
	}
	if !slices.Equal(claims, []string{"first", "second"}) || !slices.Equal(streams, []string{"final-cursor"}) {
		t.Fatalf("claim calls = %q, stream cursors = %q", claims, streams)
	}
	w.requestBySource(t, "first")
	w.requestBySource(t, "second")
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
	eventually(t, "the Cursor watch resumes after the failed release", func() bool {
		return w.cursor.Streams() == 1
	})
	w.cursor.FailClaimCalls(false)
	w.cursor.AddRequest("bc-after", true)
	w.requestBySource(t, "bc-after")
	if _, err := w.env.User(w.org.Owner).Organization.DeleteOrganization(context.Background(), connect.NewRequest(&tidev1.DeleteOrganizationRequest{OrganizationId: w.org.ID})); err != nil {
		t.Fatal(err)
	}
	if m, ok := w.cursor.Failed("bc-org"); !ok || !strings.Contains(m, "organization") {
		t.Fatalf("fail message %q %v", m, ok)
	}
	eventually(t, "the watch stops", func() bool { return w.cursor.Streams() == 0 })
	stays(t, "the deleted organization's watch stays stopped", func() bool { return w.cursor.Streams() == 0 })
}

func TestCursorOrganizationDeletionKeepsTerminalHeldClaims(t *testing.T) {
	timing := fastAgentTiming()
	timing.PollInterval = time.Hour // Keep the poller from releasing the claims during the test.
	w := newCursorWorldTiming(t, tidev1.GuestOs_GUEST_OS_LINUX, timing)
	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) {
		o.CreateError = connect.NewWireError(connect.CodeUnavailable, errors.New("full"))
	})
	ctx := t.Context()
	type request struct {
		vendorID string
		id       string
		attempts int
	}
	claims := []request{{vendorID: "bc-ended-first"}, {vendorID: "bc-ended-second"}}
	for i := range claims {
		w.cursor.AddRequest(claims[i].vendorID, true)
		claims[i].id = w.requestBySource(t, claims[i].vendorID).GetRequestId()
	}

	w.cursor.FailClaimCalls(true)
	if _, err := w.env.User(w.org.Owner).Profile.DeleteRunnerProfile(ctx, connect.NewRequest(&tidev1.DeleteRunnerProfileRequest{
		OrganizationId: w.org.ID, RunnerProfileId: w.profileID,
	})); err != nil {
		t.Fatal(err)
	}
	for i := range claims {
		w.waitRequest(t, claims[i].id, tidev1.RequestState_REQUEST_STATE_CANCELED)
		claims[i].attempts = w.cursor.FailAttempts(claims[i].vendorID)
	}

	if err := w.deleteOrganization(ctx); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("DeleteOrganization while Cursor returns 503 = %v, want internal", err)
	}
	for _, c := range claims {
		w.assertOrganizationRetainedClaim(t, c.id, c.vendorID)
		if got := w.cursor.FailAttempts(c.vendorID); got != c.attempts+1 {
			t.Fatalf("Cursor fail attempts for %s = %d, want %d", c.vendorID, got, c.attempts+1)
		}
	}
	t.Log("Cursor returned 503 for both ended claims. Tide kept both claims, the organization and the credential.")

	w.cursor.FailClaimCalls(false)
	if err := w.deleteOrganization(ctx); err != nil {
		t.Fatalf("DeleteOrganization after Cursor recovered: %v", err)
	}
	for _, c := range claims {
		if _, held := w.cursor.Claim(c.vendorID); held || w.cursor.FailAttempts(c.vendorID) != c.attempts+2 {
			t.Fatalf("Cursor did not clear the retained claim for %s on the successful deletion retry", c.vendorID)
		}
	}
	if err := w.getOrganization(ctx); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("GetOrganization after successful deletion = %v, want not found", err)
	}
	t.Log("The next deletion call cleared both vendor claims and removed the organization.")
}

func TestCursorOrganizationDeletionReleasesBusyRunnerClaim(t *testing.T) {
	timing := fastAgentTiming()
	timing.PollInterval = time.Hour // Prevent a background release after the work hook ends the request.
	w := newCursorWorldTiming(t, tidev1.GuestOs_GUEST_OS_LINUX, timing)
	const id = "bc-busy"
	w.cursor.AddRequest(id, true)
	req := w.requestBySource(t, id)
	workerID, held := w.cursor.Claim(id)
	if !held {
		t.Fatal("Cursor has no claim for the request")
	}
	eventually(t, "runner waits for work", func() bool {
		runnerID := w.request(t, req.GetRequestId()).GetRunnerId()
		return runnerID != "" && w.runner(t, runnerID).GetState() == tidev1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK
	})
	w.cursor.Connect(workerID)
	w.l.Agents.Poll(t.Context())
	running := w.waitRequest(t, req.GetRequestId(), tidev1.RequestState_REQUEST_STATE_RUNNING)
	w.waitRunner(t, running.GetRunnerId(), tidev1.RunnerState_RUNNER_STATE_BUSY)

	if err := w.deleteOrganization(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, held := w.cursor.Claim(id); held {
		t.Fatal("Cursor still holds the busy runner's claim after deletion")
	}
	if released := w.cursor.Released(id); released != 1 {
		t.Fatalf("Cursor release count = %d, want 1", released)
	}
	if err := w.getOrganization(t.Context()); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("GetOrganization after successful deletion = %v, want not found", err)
	}
	t.Log("Deletion ended the busy runner and cleared its vendor claim once before removing the organization.")
}

// cursorRequestReadFault cancels one GetRequest query for the selected
// request. It leaves the joined claim query and the RPC context usable.
type cursorRequestReadFault struct {
	requestID atomic.Pointer[string]
}

func (f *cursorRequestReadFault) TraceQueryStart(ctx context.Context, _ *pgx.Conn, q pgx.TraceQueryStartData) context.Context {
	const getRequest = "SELECT " + work.RequestColumns + " FROM requests WHERE organization_id = $1 AND request_id = $2"
	if q.SQL == getRequest {
		if id := f.requestID.Load(); id != nil && q.Args[1] == *id && f.requestID.CompareAndSwap(id, nil) {
			queryCtx, cancel := context.WithCancel(ctx)
			cancel()
			return queryCtx
		}
	}
	return ctx
}

func (*cursorRequestReadFault) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// TestCursorOrganizationDeletionRequestReadFailure faults the follow-up
// request read after the agent hook has found a held claim.
func TestCursorOrganizationDeletionRequestReadFailure(t *testing.T) {
	ctx := t.Context()
	fake := cursorfake.New(t, cursorKey, "tide-pool")
	fault := &cursorRequestReadFault{}
	timing := fastAgentTiming()
	timing.PollInterval = time.Hour
	w := startCursorWorld(t, tidev1.GuestOs_GUEST_OS_LINUX, timing, fake, func(o *app.Options) {
		cfg := o.DB.Pool.Config()
		cfg.ConnConfig.Tracer = fault
		pool, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		o.DB = &db.DB{Pool: pool}
	})
	eventually(t, "the Cursor stream", func() bool { return fake.Streams() == 1 })
	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) {
		o.CreateError = connect.NewWireError(connect.CodeUnavailable, errors.New("full"))
	})
	const id = "bc-request-read-failed"
	fake.AddRequest(id, true)
	requestID := w.requestBySource(t, id).GetRequestId()
	w.waitRequest(t, requestID, tidev1.RequestState_REQUEST_STATE_QUEUED)
	fault.requestID.Store(&requestID)
	err := w.deleteOrganization(ctx)
	if fault.requestID.Load() != nil {
		t.Fatal("the follow-up request read did not receive the fault")
	}
	if connect.CodeOf(err) != connect.CodeCanceled {
		t.Fatalf("DeleteOrganization with a canceled request read = %v, want canceled", err)
	}
	if !strings.Contains(w.env.Logs.String(), "load request "+requestID+" for held vendor claim") {
		t.Fatal("the agent hook did not report the failed follow-up request read")
	}
	w.assertOrganizationRetainedClaim(t, requestID, id)
	if attempts := fake.FailAttempts(id); attempts != 0 {
		t.Fatalf("Cursor fail attempts after a failed request read = %d, want 0", attempts)
	}
	t.Log("The request read was canceled. The RPC returned canceled and retained the organization, credential and claim without a vendor call.")

	if err := w.deleteOrganization(ctx); err != nil {
		t.Fatalf("DeleteOrganization after the request read recovered: %v", err)
	}
	if _, held := fake.Claim(id); held || fake.FailAttempts(id) != 1 {
		t.Fatal("Cursor did not clear the queued claim on the successful deletion retry")
	}
	if err := w.getOrganization(ctx); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("GetOrganization after successful deletion = %v, want not found", err)
	}
	t.Log("The next deletion call cleared the vendor claim and removed the organization.")
}

func (w *cursorWorld) deleteOrganization(ctx context.Context) error {
	_, err := w.env.User(w.org.Owner).Organization.DeleteOrganization(ctx, connect.NewRequest(&tidev1.DeleteOrganizationRequest{OrganizationId: w.org.ID}))
	return err
}

func (w *cursorWorld) getOrganization(ctx context.Context) error {
	_, err := w.env.User(w.org.Owner).Organization.GetOrganization(ctx, connect.NewRequest(&tidev1.GetOrganizationRequest{OrganizationId: w.org.ID}))
	return err
}

func (w *cursorWorld) assertOrganizationRetainedClaim(t *testing.T, requestID, vendorRequestID string) {
	t.Helper()
	ctx := t.Context()
	if err := w.getOrganization(ctx); err != nil {
		t.Fatalf("GetOrganization after failed deletion: %v", err)
	}
	key, err := w.l.Integrations.Credential(ctx, w.org.ID, w.integrationID, tidev1.AgentCredential_AGENT_CREDENTIAL_SERVICE_ACCOUNT_API_KEY)
	if err != nil || key != cursorKey {
		t.Fatalf("Cursor credential did not survive the failed deletion: %v", err)
	}
	if claim, err := w.l.Agents.Claim(ctx, requestID); err != nil || claim.State != agent.ClaimHeld {
		t.Fatalf("claim after failed deletion = %+v, %v, want held", claim, err)
	}
	if _, held := w.cursor.Claim(vendorRequestID); !held {
		t.Fatal("Cursor no longer holds the claim after failed deletion")
	}
}

func TestCursorOrganizationDeletionDatabaseFailure(t *testing.T) {
	for _, tc := range []struct {
		name      string
		setup     string
		restore   string
		wantError string
	}{
		{
			name:      "claim query",
			setup:     "ALTER TABLE agent_claims RENAME TO unavailable_claims",
			restore:   "ALTER TABLE unavailable_claims RENAME TO agent_claims",
			wantError: `"msg":"organization_deletion_prepare_failed","hook":"agent"`,
		},
		{
			name: "deletion transaction",
			setup: `CREATE TABLE deletion_blocker (organization_id uuid REFERENCES organizations);
				INSERT INTO deletion_blocker SELECT organization_id FROM organizations`,
			restore:   "DROP TABLE deletion_blocker",
			wantError: `violates foreign key constraint`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newCursorWorld(t, tidev1.GuestOs_GUEST_OS_LINUX)
			ctx := context.Background()
			if _, err := w.env.DB.Pool.Exec(ctx, tc.setup); err != nil {
				t.Fatal(err)
			}
			owner := w.env.User(w.org.Owner)
			if _, err := owner.Organization.DeleteOrganization(ctx, connect.NewRequest(&tidev1.DeleteOrganizationRequest{OrganizationId: w.org.ID})); connect.CodeOf(err) != connect.CodeInternal {
				t.Fatalf("DeleteOrganization error = %v, want internal", err)
			}
			if !strings.Contains(w.env.Logs.String(), tc.wantError) {
				t.Fatalf("missing deletion error %q", tc.wantError)
			}
			if _, err := w.env.DB.Pool.Exec(ctx, tc.restore); err != nil {
				t.Fatal(err)
			}
			eventually(t, "the Cursor watch resumes", func() bool { return w.cursor.Streams() == 1 })
			w.cursor.AddRequest("bc-after-db-error", true)
			w.requestBySource(t, "bc-after-db-error")
		})
	}
}
