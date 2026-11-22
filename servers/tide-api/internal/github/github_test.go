// SPDX-License-Identifier: FSL-1.1-ALv2

package github_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/protos/delino/tide/v1/tidev1connect"
	"github.com/delinoio/mono/servers/tide-api/internal/api"
	"github.com/delinoio/mono/servers/tide-api/internal/github"
	"github.com/delinoio/mono/servers/tide-api/internal/githubapi"
	"github.com/delinoio/mono/servers/tide-api/internal/webhook"
)

var tideLinux = []string{"self-hosted", "tide-linux"}

type githubClients = tidev1connect.GitHubServiceClient

// TestJobLifecycle runs one GITHUB_JOB from the queued event to the
// completed event with the fake GitHub API and the fake OwnSand.
func TestJobLifecycle(t *testing.T) {
	w := newGHWorld(t, worldOpts{})
	inst := w.link(t, 501, "acme")
	if inst.GetState() != tidev1.GitHubInstallationState_GIT_HUB_INSTALLATION_STATE_ACTIVE || inst.GetGithubRunnerGroupId() != 1 || inst.GetRunnerGroupName() != "Default" ||
		inst.GetGithubAccountLogin() != "acme" {
		t.Fatalf("installation %v", inst)
	}
	profileID := w.createProfile(t, ghProfile("linux", 0))

	queued := job("queued", 501, 9001, tideLinux, "")
	guid := w.send(t, "workflow_job", queued)
	req := w.requestOfJob(t, 9001)
	if req == nil || req.GetState() != tidev1.RequestState_REQUEST_STATE_QUEUED || req.GetRunnerProfileId() != profileID {
		t.Fatalf("request %v", req)
	}
	if j := req.GetGithubJob(); j.GetGithubJobId() != 9001 || j.GetGithubInstallationId() != 501 || j.GetRepositoryFullName() != "acme/app" ||
		j.GetGithubWorkflowRunId() != 90010 || j.GetJobName() != "build" {
		t.Fatalf("github job %v", j)
	}
	// A repeated delivery and a new delivery of the same job create no
	// second request.
	body, _ := json.Marshal(queued)
	if status := w.gh.SendRaw(w.gh.Headers("workflow_job", guid, body), body); status != http.StatusNoContent {
		t.Fatalf("duplicate: %d", status)
	}
	w.send(t, "workflow_job", queued)
	if n := len(w.requests(t)); n != 1 {
		t.Fatalf("%d requests", n)
	}

	eventually(t, "a waiting runner", func() bool { return len(w.runnersIn(t, tidev1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK)) == 1 })
	r := w.runnersIn(t, tidev1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK)[0]
	if r.GetGithubInstallationId() != 501 || r.GetRequestId() != "" {
		t.Fatalf("runner %v", r)
	}
	reg, ok := w.gh.RunnerByName("tide-" + r.GetRunnerId())
	if !ok || reg.RunnerGroupID != 1 || !strings.Contains(strings.Join(reg.Labels, ","), "tide-linux") || reg.Org != "acme" {
		t.Fatalf("registration %+v", reg)
	}
	jit, ok := w.os.File(w.osOrg, r.GetOwnsandSandboxId(), github.JITFilePath(tidev1.GuestOs_GUEST_OS_LINUX))
	if !ok || string(jit) != reg.JIT {
		t.Fatalf("jit file %q", jit)
	}
	var install, start bool
	for _, cmd := range w.progs.all() {
		joined := strings.Join(cmd, " ")
		if strings.Contains(joined, reg.JIT) {
			t.Fatal("a command contains the JIT configuration")
		}
		install = install || strings.Contains(joined, "actions-runner-linux-$arch-"+github.RunnerVersion+".tar.gz") && strings.Contains(joined, "baebf204307bac6193480852732f0aba2ae97b6c5597c244f4d5558390147c40")
		start = start || strings.Contains(joined, "./run.sh --jitconfig") && strings.Contains(joined, github.LogFilePath(tidev1.GuestOs_GUEST_OS_LINUX))
	}
	if !install || !start {
		t.Fatalf("install %v start %v: %v", install, start, w.progs.all())
	}

	w.send(t, "workflow_job", job("in_progress", 501, 9001, tideLinux, "tide-"+r.GetRunnerId()))
	req = w.requestOfJob(t, 9001)
	if req.GetState() != tidev1.RequestState_REQUEST_STATE_RUNNING || req.GetRunnerId() != r.GetRunnerId() ||
		w.runner(t, r.GetRunnerId()).GetState() != tidev1.RunnerState_RUNNER_STATE_BUSY {
		t.Fatalf("request %v runner %v", req, w.runner(t, r.GetRunnerId()))
	}
	// The JIT runner exits after its job: tide deletes the sandbox and keeps
	// the request until the completed event.
	w.progs.exit(r.GetOwnsandSandboxId())
	eventually(t, "runner deleted", func() bool { return w.runner(t, r.GetRunnerId()).GetState() == tidev1.RunnerState_RUNNER_STATE_DELETED })
	if got := w.runner(t, r.GetRunnerId()); got.GetEndReason() != tidev1.RunnerEndReason_RUNNER_END_REASON_WORK_DONE ||
		w.requestOfJob(t, 9001).GetState() != tidev1.RequestState_REQUEST_STATE_RUNNING {
		t.Fatalf("runner %v", got)
	}
	w.send(t, "workflow_job", job("completed", 501, 9001, tideLinux, "tide-"+r.GetRunnerId()))
	w.l.GitHub.Wait()
	if req = w.requestOfJob(t, 9001); req.GetState() != tidev1.RequestState_REQUEST_STATE_ENDED || req.GetEndReason() != tidev1.RequestEndReason_REQUEST_END_REASON_COMPLETED {
		t.Fatalf("request %v", req)
	}
	stays(t, "no new runner", func() bool { return len(w.runners(t)) == 1 })

	counts := w.webhookCounts(t)
	if counts[string(webhook.ResultDuplicate)] != 1 || counts[string(webhook.ResultProcessed)] < 4 {
		t.Fatalf("metric %v", counts)
	}
	for _, logs := range []string{w.env.Logs.String(), w.ghLogs.String()} {
		if strings.Contains(logs, reg.JIT) || strings.Contains(logs, "ghs_fake") || strings.Contains(logs, "ghu_fake") {
			t.Fatal("logs contain a secret")
		}
	}
	if !strings.Contains(w.env.Logs.String(), `"github_job_id":9001`) || !strings.Contains(w.env.Logs.String(), `"delivery_id":"`+guid+`"`) {
		t.Fatal("delivery logs lack the job or delivery ID")
	}
}

// TestJobMatching: only jobs whose labels are exactly one tide-<profile>
// of an existing GITHUB_JOB profile (optionally with self-hosted) create
// requests.
func TestJobMatching(t *testing.T) {
	w := newGHWorld(t, worldOpts{})
	w.link(t, 501, "acme")
	w.createProfile(t, ghProfile("linux", 0))
	for i, labels := range [][]string{
		{"tide-linux", "gpu"},
		{"ubuntu-latest"},
		{"self-hosted"},
		{"tide-macos"},
		{"tide-linux", "tide-other"},
		{"self-hosted", "linux", "tide-linux"},
	} {
		w.send(t, "workflow_job", job("queued", 501, int64(100+i), labels, ""))
	}
	// An installation that is not linked, and a waiting job, are ignored.
	w.send(t, "workflow_job", job("queued", 999, 200, tideLinux, ""))
	w.send(t, "workflow_job", job("waiting", 501, 201, tideLinux, ""))
	if n := len(w.requests(t)); n != 0 {
		t.Fatalf("%d requests from non-matching jobs", n)
	}
	w.send(t, "workflow_job", job("queued", 501, 300, []string{"Self-Hosted", "TIDE-LINUX"}, ""))
	w.send(t, "workflow_job", job("queued", 501, 301, []string{"tide-linux"}, ""))
	if n := len(w.requests(t)); n != 2 {
		t.Fatalf("%d requests from matching jobs", n)
	}
}

func TestMatchProfileName(t *testing.T) {
	for _, tc := range []struct {
		labels []string
		want   string
	}{
		{[]string{"tide-linux"}, "linux"},
		{[]string{"self-hosted", "tide-linux"}, "linux"},
		{[]string{"tide-linux", "tide-linux"}, "linux"},
		{[]string{"tide-linux", "x64"}, ""},
		{[]string{"tide-"}, ""},
		{[]string{"self-hosted"}, ""},
		{nil, ""},
	} {
		got, ok := github.MatchProfileName(tc.labels)
		if got != tc.want || ok != (tc.want != "") {
			t.Errorf("%v = %q %v", tc.labels, got, ok)
		}
	}
}

// TestPoolsPerInstallation: each installation is its own runner pool; a
// runner of one pool never takes the request of another.
func TestPoolsPerInstallation(t *testing.T) {
	w := newGHWorld(t, worldOpts{})
	w.link(t, 501, "acme")
	w.link(t, 502, "beta")
	w.createProfile(t, ghProfile("linux", 0))
	w.send(t, "workflow_job", job("queued", 501, 1, tideLinux, ""))
	w.send(t, "workflow_job", job("queued", 502, 2, tideLinux, ""))
	w.send(t, "workflow_job", job("queued", 502, 3, tideLinux, ""))
	eventually(t, "three runners", func() bool { return len(w.runnersIn(t, tidev1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK)) == 3 })
	pools := map[int64]int{}
	var inA, inB string
	for _, r := range w.runnersIn(t, tidev1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK) {
		pools[r.GetGithubInstallationId()]++
		if r.GetGithubInstallationId() == 501 {
			inA = r.GetRunnerId()
		} else {
			inB = r.GetRunnerId()
		}
	}
	if pools[501] != 1 || pools[502] != 2 {
		t.Fatalf("pools %v", pools)
	}
	stays(t, "covered pools", func() bool { return len(w.runners(t)) == 3 })
	if reg, _ := w.gh.RunnerByName("tide-" + inB); reg.Org != "beta" {
		t.Fatalf("runner of pool 502 registered in %q", reg.Org)
	}
	// GitHub never reports a runner of another organization, but a payload
	// that names one does not bind it.
	w.send(t, "workflow_job", job("in_progress", 502, 2, tideLinux, "tide-"+inA))
	if w.requestOfJob(t, 2).GetState() != tidev1.RequestState_REQUEST_STATE_QUEUED || w.runner(t, inA).GetState() != tidev1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK {
		t.Fatal("a runner of another pool was bound")
	}
}

// TestCanceledJobSurplus: a job that never started ends with CANCELED, the
// newest surplus runner loses its registration and its sandbox, and a
// runner whose removal GitHub refuses (it took a job) stays.
func TestCanceledJobSurplus(t *testing.T) {
	w := newGHWorld(t, worldOpts{})
	w.link(t, 501, "acme")
	w.createProfile(t, ghProfile("linux", 0))
	w.send(t, "workflow_job", job("queued", 501, 1, tideLinux, ""))
	eventually(t, "first runner", func() bool { return len(w.runnersIn(t, tidev1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK)) == 1 })
	first := w.runnersIn(t, tidev1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK)[0]
	w.send(t, "workflow_job", job("queued", 501, 2, tideLinux, ""))
	eventually(t, "second runner", func() bool { return len(w.runnersIn(t, tidev1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK)) == 2 })
	var second *tidev1.Runner
	for _, r := range w.runnersIn(t, tidev1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK) {
		if r.GetRunnerId() != first.GetRunnerId() {
			second = r
		}
	}

	w.send(t, "workflow_job", job("completed", 501, 1, tideLinux, ""))
	w.l.GitHub.Wait()
	if req := w.requestOfJob(t, 1); req.GetState() != tidev1.RequestState_REQUEST_STATE_CANCELED || req.GetEndReason() != tidev1.RequestEndReason_REQUEST_END_REASON_CANCELED_BY_SOURCE {
		t.Fatalf("request %v", req)
	}
	eventually(t, "surplus runner deleted", func() bool {
		return w.runner(t, second.GetRunnerId()).GetState() == tidev1.RunnerState_RUNNER_STATE_DELETED
	})
	if r := w.runner(t, second.GetRunnerId()); r.GetEndReason() != tidev1.RunnerEndReason_RUNNER_END_REASON_SURPLUS {
		t.Fatalf("surplus runner %v", r)
	}
	if _, ok := w.gh.RunnerByName("tide-" + second.GetRunnerId()); ok {
		t.Fatal("the surplus registration was not removed")
	}
	if w.runner(t, first.GetRunnerId()).GetState() != tidev1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK {
		t.Fatal("the covering runner was removed")
	}

	// The remaining runner took a job meanwhile (GitHub knows, tide not
	// yet): GitHub refuses the removal, and the runner stays.
	w.gh.SetBusy("tide-"+first.GetRunnerId(), true)
	w.send(t, "workflow_job", job("completed", 501, 2, tideLinux, ""))
	w.l.GitHub.Wait()
	stays(t, "refused runner", func() bool {
		return w.runner(t, first.GetRunnerId()).GetState() == tidev1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK
	})
	if _, ok := w.gh.RunnerByName("tide-" + first.GetRunnerId()); !ok {
		t.Fatal("the busy registration was removed")
	}
}

// TestWarmRunnerPerPool: a warm count keeps idle runners per ACTIVE
// installation, and the surplus rule keeps them.
func TestWarmRunnerPerPool(t *testing.T) {
	w := newGHWorld(t, worldOpts{})
	w.link(t, 501, "acme")
	w.createProfile(t, ghProfile("linux", 1))
	eventually(t, "a warm runner", func() bool { return len(w.runnersIn(t, tidev1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK)) == 1 })
	stays(t, "one warm runner", func() bool { return len(w.runners(t)) == 1 })
	w.send(t, "workflow_job", job("queued", 501, 1, tideLinux, ""))
	eventually(t, "a second runner", func() bool { return len(w.runnersIn(t, tidev1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK)) == 2 })
	w.send(t, "workflow_job", job("completed", 501, 1, tideLinux, ""))
	w.l.GitHub.Wait()
	eventually(t, "one surplus runner removed", func() bool { return len(w.runnersIn(t, tidev1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK)) == 1 })
	stays(t, "the warm runner stays", func() bool { return len(w.runnersIn(t, tidev1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK)) == 1 })
}

// TestLostDeliveries: a delivery that GitHub could not deliver is
// redelivered by the redelivery job and then processed once.
func TestLostDeliveries(t *testing.T) {
	w := newGHWorld(t, worldOpts{})
	w.link(t, 501, "acme")
	w.createProfile(t, ghProfile("linux", 0))
	w.gh.SendLost("workflow_job", job("queued", 501, 7, tideLinux, ""))
	if len(w.requests(t)) != 0 {
		t.Fatal("a lost delivery created a request")
	}
	// A delivery that tide processed although GitHub recorded a failure
	// (for example a slow answer) is not redelivered.
	guid := w.send(t, "workflow_job", job("queued", 501, 8, tideLinux, ""))
	w.gh.MarkFailed(guid)
	if n := w.l.GitHub.RedeliverOnce(context.Background()); n != 1 {
		t.Fatalf("%d redeliveries", n)
	}
	eventually(t, "redelivered request", func() bool { return w.requestOfJob(t, 7) != nil })
	if n := w.l.GitHub.RedeliverOnce(context.Background()); n != 0 {
		t.Fatalf("%d redeliveries after success", n)
	}
	if w.gh.Calls("redeliver_hook_delivery") != 1 {
		t.Fatalf("%d redelivery calls", w.gh.Calls("redeliver_hook_delivery"))
	}
	// The in_progress delivery of a job whose queued delivery is lost
	// creates the request and binds it.
	eventually(t, "runners", func() bool { return len(w.runnersIn(t, tidev1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK)) == 2 })
	r := w.runnersIn(t, tidev1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK)[0]
	w.send(t, "workflow_job", job("in_progress", 501, 9, tideLinux, "tide-"+r.GetRunnerId()))
	if req := w.requestOfJob(t, 9); req == nil || req.GetState() != tidev1.RequestState_REQUEST_STATE_RUNNING || req.GetRunnerId() != r.GetRunnerId() {
		t.Fatalf("request %v", req)
	}
	// A completed event whose in_progress was lost ends the request as
	// COMPLETED when it names a tide runner.
	w.send(t, "workflow_job", job("completed", 501, 8, tideLinux, "tide-"+w.runnersIn(t, tidev1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK)[0].GetRunnerId()))
	if req := w.requestOfJob(t, 8); req.GetState() != tidev1.RequestState_REQUEST_STATE_ENDED || req.GetEndReason() != tidev1.RequestEndReason_REQUEST_END_REASON_COMPLETED {
		t.Fatalf("request %v", req)
	}
	w.l.GitHub.Wait()
}

// TestCompletedBeforeQueued: a completed event of a job without a request
// (its queued delivery failed and waits for the redelivery) stores an
// ended request, so the late queued and in_progress deliveries start
// nothing.
func TestCompletedBeforeQueued(t *testing.T) {
	w := newGHWorld(t, worldOpts{})
	w.link(t, 501, "acme")
	w.createProfile(t, ghProfile("linux", 0))
	w.send(t, "workflow_job", job("completed", 501, 40, tideLinux, ""))
	w.l.GitHub.Wait()
	req := w.requestOfJob(t, 40)
	if req == nil || req.GetState() != tidev1.RequestState_REQUEST_STATE_CANCELED || req.GetEndReason() != tidev1.RequestEndReason_REQUEST_END_REASON_CANCELED_BY_SOURCE {
		t.Fatalf("request %v", req)
	}
	w.send(t, "workflow_job", job("queued", 501, 40, tideLinux, ""))
	w.send(t, "workflow_job", job("in_progress", 501, 40, tideLinux, "tide-"+api.NewID()))
	stays(t, "no runner", func() bool { return len(w.runners(t)) == 0 })
	if got := w.requestOfJob(t, 40); got.GetRequestId() != req.GetRequestId() || got.GetState() != tidev1.RequestState_REQUEST_STATE_CANCELED || len(w.requests(t)) != 1 {
		t.Fatalf("request after the late deliveries %v", got)
	}
	// A completed event of a job that matches no profile stores nothing.
	w.send(t, "workflow_job", job("completed", 501, 41, []string{"ubuntu-latest"}, ""))
	if w.requestOfJob(t, 41) != nil {
		t.Fatal("a request for a job that matches no profile")
	}
}

// TestJobWaitsForDeactivation: a queued delivery that arrives while a
// deactivation of the installation holds the link waits for it and then
// stores no request for the inactive installation.
func TestJobWaitsForDeactivation(t *testing.T) {
	w := newGHWorld(t, worldOpts{})
	w.link(t, 501, "acme")
	w.createProfile(t, ghProfile("linux", 0))
	ctx := context.Background()
	tx, err := w.env.DB.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT 1 FROM github_installations WHERE github_installation_id = 501 FOR UPDATE"); err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() {
		_, status := w.gh.Send("workflow_job", job("queued", 501, 60, tideLinux, ""))
		done <- status
	}()
	select {
	case status := <-done:
		t.Fatalf("the delivery did not wait for the deactivation (status %d)", status)
	case <-time.After(500 * time.Millisecond):
	}
	if _, err := tx.Exec(ctx, "UPDATE github_installations SET state = $1 WHERE github_installation_id = 501",
		int16(tidev1.GitHubInstallationState_GIT_HUB_INSTALLATION_STATE_SUSPENDED)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if status := <-done; status != http.StatusNoContent {
		t.Fatalf("status %d", status)
	}
	if n := len(w.requests(t)); n != 0 {
		t.Fatalf("%d requests for a suspended installation", n)
	}
}

// TestInstallationSuspendedAndDeleted: suspend and deleted make the link
// inactive, cancel its queued requests and delete its waiting runners;
// unsuspend makes it ACTIVE again.
func TestInstallationSuspendedAndDeleted(t *testing.T) {
	w := newGHWorld(t, worldOpts{})
	w.link(t, 501, "acme")
	w.createProfile(t, ghProfile("linux", 0))
	w.send(t, "workflow_job", job("queued", 501, 1, tideLinux, ""))
	eventually(t, "a waiting runner", func() bool { return len(w.runnersIn(t, tidev1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK)) == 1 })
	// A second job runs on a busy runner while the installation is
	// suspended.
	r := w.runnersIn(t, tidev1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK)[0]
	w.send(t, "workflow_job", job("in_progress", 501, 1, tideLinux, "tide-"+r.GetRunnerId()))
	w.send(t, "workflow_job", job("queued", 501, 2, tideLinux, ""))
	eventually(t, "a second runner", func() bool { return len(w.runnersIn(t, tidev1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK)) == 1 })
	waiting := w.runnersIn(t, tidev1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK)[0]

	w.gh.SetSuspended(501, true)
	w.send(t, "installation", installationEvent("suspend", 501, "acme"))
	list, err := w.env.User(w.org.Member).GitHub.ListGitHubInstallations(context.Background(), connect.NewRequest(&tidev1.ListGitHubInstallationsRequest{OrganizationId: w.org.ID}))
	if err != nil || list.Msg.GetInstallations()[0].GetState() != tidev1.GitHubInstallationState_GIT_HUB_INSTALLATION_STATE_SUSPENDED {
		t.Fatalf("list %v %v", list, err)
	}
	if q := w.requestOfJob(t, 2); q.GetState() != tidev1.RequestState_REQUEST_STATE_CANCELED || q.GetEndReason() != tidev1.RequestEndReason_REQUEST_END_REASON_INTEGRATION_REMOVED {
		t.Fatalf("queued request %v", q)
	}
	if b := w.requestOfJob(t, 1); b.GetState() != tidev1.RequestState_REQUEST_STATE_ENDED || b.GetEndReason() != tidev1.RequestEndReason_REQUEST_END_REASON_INTEGRATION_REMOVED {
		t.Fatalf("running request %v", b)
	}
	eventually(t, "waiting runner deleted", func() bool {
		return w.runner(t, waiting.GetRunnerId()).GetState() == tidev1.RunnerState_RUNNER_STATE_DELETED
	})
	if w.runner(t, r.GetRunnerId()).GetState() != tidev1.RunnerState_RUNNER_STATE_BUSY {
		t.Fatal("the busy runner was deleted")
	}
	// No new demand while suspended.
	w.send(t, "workflow_job", job("queued", 501, 3, tideLinux, ""))
	if w.requestOfJob(t, 3) != nil {
		t.Fatal("a suspended installation created a request")
	}
	w.gh.SetSuspended(501, false)
	w.send(t, "installation", installationEvent("unsuspend", 501, "acme"))
	w.send(t, "workflow_job", job("queued", 501, 4, tideLinux, ""))
	if w.requestOfJob(t, 4) == nil {
		t.Fatal("an unsuspended installation created no request")
	}
	w.send(t, "installation", installationEvent("deleted", 501, "acme"))
	list, _ = w.env.User(w.org.Member).GitHub.ListGitHubInstallations(context.Background(), connect.NewRequest(&tidev1.ListGitHubInstallationsRequest{OrganizationId: w.org.ID}))
	if list.Msg.GetInstallations()[0].GetState() != tidev1.GitHubInstallationState_GIT_HUB_INSTALLATION_STATE_DELETED {
		t.Fatal("not deleted")
	}
	if q := w.requestOfJob(t, 4); q.GetState() != tidev1.RequestState_REQUEST_STATE_CANCELED {
		t.Fatalf("request %v", q)
	}
	w.send(t, "installation", installationEvent("unsuspend", 501, "acme"))
	list, _ = w.env.User(w.org.Member).GitHub.ListGitHubInstallations(context.Background(), connect.NewRequest(&tidev1.ListGitHubInstallationsRequest{OrganizationId: w.org.ID}))
	if list.Msg.GetInstallations()[0].GetState() != tidev1.GitHubInstallationState_GIT_HUB_INSTALLATION_STATE_DELETED {
		t.Fatal("a deleted installation became active")
	}
	w.l.GitHub.Wait()
}

// TestWebhookSignature: deliveries without a valid signature get 401 and
// change nothing; oversized and malformed deliveries are rejected.
func TestWebhookSignature(t *testing.T) {
	w := newGHWorld(t, worldOpts{})
	w.link(t, 501, "acme")
	w.createProfile(t, ghProfile("linux", 0))
	body, _ := json.Marshal(job("queued", 501, 1, tideLinux, ""))
	headers := w.gh.Headers("workflow_job", "guid-1", body)
	for name, mutate := range map[string]func(h map[string]string){
		"missing":   func(h map[string]string) { delete(h, "X-Hub-Signature-256") },
		"wrong":     func(h map[string]string) { h["X-Hub-Signature-256"] = githubapi.Sign([]byte("another secret"), body) },
		"sha1 only": func(h map[string]string) { delete(h, "X-Hub-Signature-256"); h["X-Hub-Signature"] = "sha1=00" },
		"other body": func(h map[string]string) {
			h["X-Hub-Signature-256"] = githubapi.Sign([]byte(w.gh.WebhookSecret), []byte("{}"))
		},
	} {
		h := map[string]string{}
		for k, v := range headers {
			h[k] = v
		}
		mutate(h)
		if status := w.gh.SendRaw(h, body); status != http.StatusUnauthorized {
			t.Errorf("%s: status %d", name, status)
		}
	}
	if len(w.requests(t)) != 0 {
		t.Fatal("an unsigned delivery created a request")
	}
	if seen := w.webhookCounts(t)[string(webhook.ResultInvalidSignature)]; seen != 4 {
		t.Fatalf("invalid_signature = %d", seen)
	}
	// The unsigned delivery recorded no delivery ID: the signed one with the
	// same ID is processed.
	if status := w.gh.SendRaw(headers, body); status != http.StatusNoContent || len(w.requests(t)) != 1 {
		t.Fatalf("signed delivery: %d", status)
	}
	big := bytes.Repeat([]byte(" "), webhook.MaxBodyBytes+1)
	if status := w.gh.SendRaw(w.gh.Headers("workflow_job", "guid-2", big), big); status != http.StatusRequestEntityTooLarge {
		t.Fatalf("large body: %d", status)
	}
	noID := w.gh.Headers("workflow_job", "", body)
	if status := w.gh.SendRaw(noID, body); status != http.StatusBadRequest {
		t.Fatalf("no delivery id: %d", status)
	}
	bad := []byte("{not json")
	if status := w.gh.SendRaw(w.gh.Headers("workflow_job", "guid-3", bad), bad); status != http.StatusBadRequest {
		t.Fatalf("bad json: %d", status)
	}
	if strings.Contains(w.env.Logs.String(), `"labels"`) {
		t.Fatal("a webhook body was logged")
	}
}

// TestNotConfigured: without a GitHub App, the RPCs that need GitHub fail
// with failed_precondition and the webhook answers 503.
func TestNotConfigured(t *testing.T) {
	w := newGHWorld(t, worldOpts{unconfigured: true})
	ctx := context.Background()
	_, err := w.env.User(w.org.Admin).GitHub.CreateGitHubInstallUrl(ctx, connect.NewRequest(&tidev1.CreateGitHubInstallUrlRequest{OrganizationId: w.org.ID}))
	wantCode(t, "install url", err, connect.CodeFailedPrecondition)
	_, err = w.env.User(w.org.Admin).GitHub.LinkGitHubInstallation(ctx, connect.NewRequest(&tidev1.LinkGitHubInstallationRequest{OrganizationId: w.org.ID,
		GithubInstallationId: 1, Code: "c", State: "s"}))
	wantCode(t, "link", err, connect.CodeFailedPrecondition)
	if _, err := w.env.User(w.org.Member).GitHub.ListGitHubInstallations(ctx, connect.NewRequest(&tidev1.ListGitHubInstallationsRequest{OrganizationId: w.org.ID})); err != nil {
		t.Fatal(err)
	}
	// Authorization comes first: an outsider gets not_found.
	_, err = w.env.User("mallory").GitHub.CreateGitHubInstallUrl(ctx, connect.NewRequest(&tidev1.CreateGitHubInstallUrlRequest{OrganizationId: w.org.ID}))
	wantCode(t, "outsider", err, connect.CodeNotFound)
	body := []byte(`{}`)
	if status := w.gh.SendRaw(w.gh.Headers("workflow_job", "g", body), body); status != http.StatusServiceUnavailable {
		t.Fatalf("webhook: %d", status)
	}
	if w.webhookCounts(t)[string(webhook.ResultNotConfigured)] != 1 {
		t.Fatal("metric")
	}
}

// TestLinkFlow covers the state, the code exchange and the installation
// checks of LinkGitHubInstallation, the runner group update and the unlink.
func TestLinkFlow(t *testing.T) {
	w := newGHWorld(t, worldOpts{})
	ctx := context.Background()
	admin := w.env.User(w.org.Admin).GitHub
	link := func(state, code string, id int64) error {
		_, err := admin.LinkGitHubInstallation(ctx, connect.NewRequest(&tidev1.LinkGitHubInstallationRequest{OrganizationId: w.org.ID, GithubInstallationId: id, Code: code, State: state}))
		return err
	}
	w.gh.AddInstallation(501, "acme", "Organization", "octo")
	w.gh.AddInstallation(502, "octo", "User", "octo")
	w.gh.AddInstallation(503, "other", "Organization", "someone")

	resp, err := admin.CreateGitHubInstallUrl(ctx, connect.NewRequest(&tidev1.CreateGitHubInstallUrlRequest{OrganizationId: w.org.ID}))
	if err != nil || !strings.HasPrefix(resp.Msg.GetUrl(), w.gh.Server.URL+"/apps/tide-test/installations/new?state=") ||
		resp.Msg.GetExpireTime().AsTime().Sub(w.env.Clock.Now()) != 10*time.Minute {
		t.Fatalf("url %v %v", resp, err)
	}
	wantCode(t, "unknown state", link("nope", "c", 501), connect.CodeInvalidArgument)
	// A state of another user (the owner) does not work for the admin.
	ownerState := w.installState(t, w.org.Owner)
	w.gh.AddCode("oauthcode1", "octo")
	wantCode(t, "state of another user", link(ownerState, "oauthcode1", 501), connect.CodeInvalidArgument)
	// Expired after 10 minutes.
	expired := w.installState(t, w.org.Admin)
	w.env.Clock.Advance(11 * time.Minute)
	wantCode(t, "expired state", link(expired, "oauthcode1", 501), connect.CodeInvalidArgument)
	// A bad code consumes the state.
	state := w.installState(t, w.org.Admin)
	wantCode(t, "bad code", link(state, "wrong", 501), connect.CodeInvalidArgument)
	wantCode(t, "used state", link(state, "oauthcode1", 501), connect.CodeInvalidArgument)
	// Personal accounts and installations that the user cannot access fail.
	w.gh.AddCode("oauthcode2", "octo")
	wantCode(t, "personal account", link(w.installState(t, w.org.Admin), "oauthcode2", 502), connect.CodeFailedPrecondition)
	w.gh.AddCode("oauthcode3", "octo")
	wantCode(t, "not the user's installation", link(w.installState(t, w.org.Admin), "oauthcode3", 503), connect.CodePermissionDenied)
	w.gh.AddCode("oauthcode4", "octo")
	if err := link(w.installState(t, w.org.Admin), "oauthcode4", 501); err != nil {
		t.Fatal(err)
	}
	w.gh.AddCode("oauthcode5", "octo")
	wantCode(t, "linked twice", link(w.installState(t, w.org.Admin), "oauthcode5", 501), connect.CodeAlreadyExists)

	// Another tide organization cannot link the same installation.
	other := w.env.NewOrg("bob")
	st, err := w.env.User(other.Admin).GitHub.CreateGitHubInstallUrl(ctx, connect.NewRequest(&tidev1.CreateGitHubInstallUrlRequest{OrganizationId: other.ID}))
	if err != nil {
		t.Fatal(err)
	}
	_, s, _ := strings.Cut(st.Msg.GetUrl(), "?state=")
	w.gh.AddCode("oauthcode6", "octo")
	_, err = w.env.User(other.Admin).GitHub.LinkGitHubInstallation(ctx, connect.NewRequest(&tidev1.LinkGitHubInstallationRequest{OrganizationId: other.ID,
		GithubInstallationId: 501, Code: "oauthcode6", State: s}))
	wantCode(t, "another organization", err, connect.CodeAlreadyExists)

	list, err := admin.ListGitHubInstallations(ctx, connect.NewRequest(&tidev1.ListGitHubInstallationsRequest{OrganizationId: w.org.ID}))
	if err != nil || len(list.Msg.GetInstallations()) != 1 {
		t.Fatalf("list %v %v", list, err)
	}
	inst := list.Msg.GetInstallations()[0]
	groups, err := admin.ListGitHubRunnerGroups(ctx, connect.NewRequest(&tidev1.ListGitHubRunnerGroupsRequest{OrganizationId: w.org.ID, InstallationId: inst.GetInstallationId()}))
	if err != nil || len(groups.Msg.GetRunnerGroups()) != 2 || !groups.Msg.GetRunnerGroups()[0].GetIsDefault() {
		t.Fatalf("groups %v %v", groups, err)
	}
	_, err = admin.UpdateGitHubInstallation(ctx, connect.NewRequest(&tidev1.UpdateGitHubInstallationRequest{OrganizationId: w.org.ID, InstallationId: inst.GetInstallationId(), GithubRunnerGroupId: 99}))
	wantCode(t, "unknown group", err, connect.CodeInvalidArgument)
	up, err := admin.UpdateGitHubInstallation(ctx, connect.NewRequest(&tidev1.UpdateGitHubInstallationRequest{OrganizationId: w.org.ID, InstallationId: inst.GetInstallationId(), GithubRunnerGroupId: 7}))
	if err != nil || up.Msg.GetInstallation().GetGithubRunnerGroupId() != 7 || up.Msg.GetInstallation().GetRunnerGroupName() != "tide" {
		t.Fatalf("update %v %v", up, err)
	}

	// New runners register in the selected group; unlink removes the
	// registration of the waiting runner, deletes it and cancels the job.
	w.createProfile(t, ghProfile("linux", 0))
	w.send(t, "workflow_job", job("queued", 501, 1, tideLinux, ""))
	eventually(t, "a waiting runner", func() bool { return len(w.runnersIn(t, tidev1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK)) == 1 })
	r := w.runnersIn(t, tidev1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK)[0]
	if reg, _ := w.gh.RunnerByName("tide-" + r.GetRunnerId()); reg.RunnerGroupID != 7 {
		t.Fatalf("runner group %d", reg.RunnerGroupID)
	}
	if _, err := admin.UnlinkGitHubInstallation(ctx, connect.NewRequest(&tidev1.UnlinkGitHubInstallationRequest{OrganizationId: w.org.ID, InstallationId: inst.GetInstallationId()})); err != nil {
		t.Fatal(err)
	}
	if _, ok := w.gh.RunnerByName("tide-" + r.GetRunnerId()); ok {
		t.Fatal("registration not removed")
	}
	if q := w.requestOfJob(t, 1); q.GetState() != tidev1.RequestState_REQUEST_STATE_CANCELED || q.GetEndReason() != tidev1.RequestEndReason_REQUEST_END_REASON_INTEGRATION_REMOVED {
		t.Fatalf("request %v", q)
	}
	eventually(t, "runner deleted", func() bool { return w.runner(t, r.GetRunnerId()).GetState() == tidev1.RunnerState_RUNNER_STATE_DELETED })
	for action, want := range map[tidev1.AuditAction]int{
		tidev1.AuditAction_AUDIT_ACTION_GITHUB_INSTALLATION_LINKED:   1,
		tidev1.AuditAction_AUDIT_ACTION_GITHUB_INSTALLATION_UPDATED:  1,
		tidev1.AuditAction_AUDIT_ACTION_GITHUB_INSTALLATION_UNLINKED: 1,
	} {
		var n int
		if err := w.env.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM audit_events WHERE organization_id = $1 AND action = $2", w.org.ID, int16(action)).Scan(&n); err != nil || n != want {
			t.Errorf("audit %v = %d, %v", action, n, err)
		}
	}
	// After the unlink, the installation can be linked again.
	w.gh.AddCode("oauthcode7", "octo")
	if err := link(w.installState(t, w.org.Admin), "oauthcode7", 501); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(w.env.Logs.String(), "oauthcode4") && strings.Contains(w.env.Logs.String(), `"code":"oauthcode4"`) {
		t.Fatal("an OAuth code was logged")
	}
}

// TestAuthz: installation management needs owner or admin; members read
// the links; personal API keys and outsiders get nothing.
func TestAuthz(t *testing.T) {
	w := newGHWorld(t, worldOpts{})
	inst := w.link(t, 501, "acme")
	ctx := context.Background()
	_, apiKey := w.env.NewAPIKey(w.org.ID, w.org.Member)
	type call func(c githubClients) error
	cases := map[string]call{
		"CreateGitHubInstallUrl": func(c githubClients) error {
			_, err := c.CreateGitHubInstallUrl(ctx, connect.NewRequest(&tidev1.CreateGitHubInstallUrlRequest{OrganizationId: w.org.ID}))
			return err
		},
		"LinkGitHubInstallation": func(c githubClients) error {
			_, err := c.LinkGitHubInstallation(ctx, connect.NewRequest(&tidev1.LinkGitHubInstallationRequest{OrganizationId: w.org.ID, GithubInstallationId: 1, Code: "x", State: "y"}))
			return err
		},
		"ListGitHubRunnerGroups": func(c githubClients) error {
			_, err := c.ListGitHubRunnerGroups(ctx, connect.NewRequest(&tidev1.ListGitHubRunnerGroupsRequest{OrganizationId: w.org.ID, InstallationId: inst.GetInstallationId()}))
			return err
		},
		"UpdateGitHubInstallation": func(c githubClients) error {
			_, err := c.UpdateGitHubInstallation(ctx, connect.NewRequest(&tidev1.UpdateGitHubInstallationRequest{OrganizationId: w.org.ID, InstallationId: inst.GetInstallationId(), GithubRunnerGroupId: 1}))
			return err
		},
		"UnlinkGitHubInstallation": func(c githubClients) error {
			_, err := c.UnlinkGitHubInstallation(ctx, connect.NewRequest(&tidev1.UnlinkGitHubInstallationRequest{OrganizationId: w.org.ID, InstallationId: "01900000-0000-7000-8000-000000000000"}))
			return err
		},
	}
	for name, fn := range cases {
		wantCode(t, name+" member", fn(w.env.User(w.org.Member).GitHub), connect.CodePermissionDenied)
		wantCode(t, name+" api key", fn(w.env.Bearer(apiKey).GitHub), connect.CodePermissionDenied)
		wantCode(t, name+" outsider", fn(w.env.User("mallory").GitHub), connect.CodeNotFound)
		if name != "LinkGitHubInstallation" && name != "UnlinkGitHubInstallation" {
			if err := fn(w.env.User(w.org.Owner).GitHub); err != nil {
				t.Errorf("%s owner: %v", name, err)
			}
		}
	}
	list := func(c githubClients) error {
		_, err := c.ListGitHubInstallations(ctx, connect.NewRequest(&tidev1.ListGitHubInstallationsRequest{OrganizationId: w.org.ID}))
		return err
	}
	if err := list(w.env.User(w.org.Member).GitHub); err != nil {
		t.Fatal(err)
	}
	wantCode(t, "list api key", list(w.env.Bearer(apiKey).GitHub), connect.CodePermissionDenied)
	wantCode(t, "list outsider", list(w.env.User("mallory").GitHub), connect.CodeNotFound)
	wantCode(t, "unlink unknown", cases["UnlinkGitHubInstallation"](w.env.User(w.org.Owner).GitHub), connect.CodeNotFound)
}

// TestOrganizationDeletion: deleting the organization removes the GitHub
// registrations of its runners before the sandboxes go, and the links with
// the organization.
func TestOrganizationDeletion(t *testing.T) {
	w := newGHWorld(t, worldOpts{})
	w.link(t, 501, "acme")
	w.createProfile(t, ghProfile("linux", 0))
	w.send(t, "workflow_job", job("queued", 501, 1, tideLinux, ""))
	eventually(t, "a waiting runner", func() bool { return len(w.runnersIn(t, tidev1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK)) == 1 })
	r := w.runnersIn(t, tidev1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK)[0]
	if _, err := w.env.User(w.org.Owner).Organization.DeleteOrganization(context.Background(), connect.NewRequest(&tidev1.DeleteOrganizationRequest{OrganizationId: w.org.ID})); err != nil {
		t.Fatal(err)
	}
	if _, ok := w.gh.RunnerByName("tide-" + r.GetRunnerId()); ok {
		t.Fatal("registration not removed")
	}
	var n int
	if err := w.env.DB.Pool.QueryRow(context.Background(), "SELECT count(*) FROM github_installations").Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d links left, %v", n, err)
	}
	// Deliveries of the installation are ignored now.
	w.send(t, "workflow_job", job("queued", 501, 2, tideLinux, ""))
}
