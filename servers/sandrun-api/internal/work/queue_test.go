// SPDX-License-Identifier: FSL-1.1-ALv2

package work

import (
	"context"
	"strings"
	"testing"
	"time"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/servers/sandrun-api/internal/profile"
)

type testAdapter struct {
	BaseAdapter
	kind  sandrunv1.WorkloadKind
	binds bool
}

func (a testAdapter) Kind() sandrunv1.WorkloadKind                     { return a.kind }
func (a testAdapter) BindsAtDispatch() bool                            { return a.binds }
func (testAdapter) Prepare(context.Context, *Guest) error              { return nil }
func (testAdapter) Start(context.Context, *Guest) (StartResult, error) { return StartResult{}, nil }

func testEngine() *Engine {
	e := &Engine{adapters: map[sandrunv1.WorkloadKind]Adapter{}}
	e.RegisterAdapter(testAdapter{kind: sandrunv1.WorkloadKind_WORKLOAD_KIND_GITHUB_JOB})
	e.RegisterAdapter(testAdapter{kind: sandrunv1.WorkloadKind_WORKLOAD_KIND_DEV_SESSION, binds: true})
	return e
}

func ids(reqs []*Request) string {
	var out []string
	for _, r := range reqs {
		out = append(out, r.ID)
	}
	return strings.Join(out, ",")
}

// TestUncovered checks the coverage rules: binding kinds are covered only by
// their own runner; GitHub pools cover their oldest requests with runners in
// CREATING, PREPARING or WAITING_FOR_WORK, per pool.
func TestUncovered(t *testing.T) {
	e := testEngine()
	t0 := time.Unix(1_800_000_000, 0)
	gh := sandrunv1.WorkloadKind_WORKLOAD_KIND_GITHUB_JOB
	dev := sandrunv1.WorkloadKind_WORKLOAD_KIND_DEV_SESSION
	req := func(id string, kind sandrunv1.WorkloadKind, profile, pool string, age int) *Request {
		return &Request{ID: id, Kind: kind, ProfileID: profile, PoolKey: pool, State: reqQueued, QueueTime: t0.Add(time.Duration(age) * time.Second)}
	}
	// Queue order as the dispatcher reads it: d1 has HIGH priority.
	queued := []*Request{
		req("d1", dev, "pd", "", 5),
		req("g1", gh, "pg", "inst-a", 1),
		req("g2", gh, "pg", "inst-a", 2),
		req("g3", gh, "pg", "inst-b", 3),
		req("d2", dev, "pd", "", 4),
	}
	runners := []*Runner{
		{ID: "r1", Kind: gh, ProfileID: "pg", PoolKey: "inst-a", State: runWaiting},
		{ID: "r2", Kind: gh, ProfileID: "pg", PoolKey: "inst-a", State: runBusy},     // busy: covers nothing
		{ID: "r3", Kind: gh, ProfileID: "pg", PoolKey: "inst-b", State: runDeleting}, // deleting: covers nothing
		{ID: "r4", Kind: dev, ProfileID: "pd", State: runCreating, RequestID: "d2"},  // bound to d2
		{ID: "r5", Kind: dev, ProfileID: "pd", State: runWarm},                       // warm: covers nothing
	}
	if got := ids(e.uncovered(queued, runners)); got != "d1,g2,g3" {
		t.Fatalf("uncovered = %s", got)
	}
	runners = append(runners, &Runner{ID: "r6", Kind: gh, ProfileID: "pg", PoolKey: "inst-a", State: runPrep})
	if got := ids(e.uncovered(queued, runners)); got != "d1,g3" {
		t.Fatalf("uncovered with a second pool runner = %s", got)
	}
	// A kind without an adapter stays uncovered (and the pass skips it).
	queued = append(queued, req("a1", sandrunv1.WorkloadKind_WORKLOAD_KIND_AGENT_SESSION, "pa", "", 0))
	if got := ids(e.uncovered(queued, runners)); got != "d1,g3,a1" {
		t.Fatalf("uncovered with an unknown kind = %s", got)
	}
}

func TestOldestWarm(t *testing.T) {
	runners := []*Runner{
		{ID: "busy", ProfileID: "p", State: runBusy},
		{ID: "bound", ProfileID: "p", State: runWarm, RequestID: "x"},
		{ID: "other", ProfileID: "q", State: runWarm},
		{ID: "w1", ProfileID: "p", State: runWarm},
		{ID: "w2", ProfileID: "p", State: runWarm},
	}
	if r := oldestWarm(runners, "p"); r == nil || r.ID != "w1" {
		t.Fatalf("oldest warm = %v", r)
	}
	if oldestWarm(runners, "z") != nil {
		t.Fatal("warm runner of another profile")
	}
}

func TestSandboxRequest(t *testing.T) {
	p := &profile.Profile{ID: "p1", OrganizationID: "o1", Kind: sandrunv1.WorkloadKind_WORKLOAD_KIND_DEV_SESSION, GuestOS: sandrunv1.GuestOs_GUEST_OS_MACOS,
		BaseImage: sandrunv1.GuestOs_GUEST_OS_MACOS, Resources: profile.Resources{VCPUCount: 4, MemoryMiB: 8192, DiskGiB: 80}, IdleTTL: time.Hour, MaxTTL: 2 * time.Hour}
	r := &Runner{ID: "r1", OrganizationID: "o1", IdempotencyKey: "k1"}
	req := sandboxRequest(p, r)
	if req.GetIdempotencyKey() != "k1" || req.GetPlacement() != nil || req.GetSource().GetBaseImage() != ownsandv1.BaseImage_BASE_IMAGE_MACOS ||
		req.GetResources().GetMemoryMib() != 8192 || req.GetMaxTtl().AsDuration() != 2*time.Hour || req.GetNetwork().GetMode() != ownsandv1.NetworkMode_NETWORK_MODE_OUTBOUND ||
		req.GetLabels()["sandrun.runner-id"] != "r1" || req.GetLabels()["sandrun.organization-id"] != "o1" || req.GetLabels()["sandrun.runner-profile-id"] != "p1" {
		t.Fatalf("request %v", req)
	}
	p.BaseImage, p.TemplateID = 0, "t1"
	if sandboxRequest(p, r).GetSource().GetTemplateId() != "t1" {
		t.Fatal("template source")
	}
}

func TestBaseAdapterOutcomes(t *testing.T) {
	var b BaseAdapter
	if end, keep := b.ProgramExited(nil, nil); keep || end.State != reqEnded || end.Reason != sandrunv1.RequestEndReason_REQUEST_END_REASON_COMPLETED {
		t.Fatalf("program exit %v", end)
	}
	if out := b.SandboxEnded(nil, ownsandv1.SandboxEndReason_SANDBOX_END_REASON_LOST_ON_DEVICE); out.RunnerState != runFailed || out.Request.Reason != sandrunv1.RequestEndReason_REQUEST_END_REASON_RUNNER_LOST {
		t.Fatalf("sandbox end %v", out)
	}
	if end, _ := b.RunnerDeleted(nil, DeleteMemberRemoved); end.Reason != sandrunv1.RequestEndReason_REQUEST_END_REASON_MEMBER_REMOVED {
		t.Fatalf("member removed %v", end)
	}
	if !RequestTerminal(reqExpired) || RequestTerminal(reqStarting) || !RunnerTerminal(runFailed) || RunnerTerminal(runDeleting) {
		t.Fatal("terminal states")
	}
}

func TestRunnerProtoGitHubInstallation(t *testing.T) {
	r := &Runner{ID: "r1", Kind: sandrunv1.WorkloadKind_WORKLOAD_KIND_GITHUB_JOB, PoolKey: "123456"}
	if got := r.Proto().GetGithubInstallationId(); got != 123456 {
		t.Fatalf("github installation = %d", got)
	}
	r.Kind = sandrunv1.WorkloadKind_WORKLOAD_KIND_DEV_SESSION
	if r.Proto().GetGithubInstallationId() != 0 {
		t.Fatal("installation on a dev session runner")
	}
}
