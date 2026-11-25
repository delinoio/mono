// SPDX-License-Identifier: FSL-1.1-ALv2

package github_test

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/delinoio/mono/go/ownsand"
	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/servers/tide-api/internal/api"
	"github.com/delinoio/mono/servers/tide-api/internal/app"
	"github.com/delinoio/mono/servers/tide-api/internal/github"
	"github.com/delinoio/mono/servers/tide-api/internal/githubapi"
	"github.com/delinoio/mono/servers/tide-api/internal/githubapi/githubfake"
	"github.com/delinoio/mono/servers/tide-api/internal/layers"
	"github.com/delinoio/mono/servers/tide-api/internal/lease"
	"github.com/delinoio/mono/servers/tide-api/internal/ownsandclient/ownsandfake"
	"github.com/delinoio/mono/servers/tide-api/internal/testenv"
)

// programs is the fake guest: the installation exec exits with 0, and a
// start exec (run.sh or run.cmd) runs until the test ends it with exit.
type programs struct {
	mu       sync.Mutex
	commands [][]string
	running  map[string]chan struct{} // sandbox ID -> exit signal
	// gate, when set, holds each start exec (the registration is recorded
	// and the runner is still PREPARING) until it is closed; entered gets
	// the sandbox ID of a held start.
	gate    chan struct{}
	entered chan string
}

// hold holds the start execs until gate is closed.
func (p *programs) hold(gate chan struct{}, entered chan string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.gate, p.entered = gate, entered
}

func (p *programs) exec(ctx context.Context, sandboxID string, e ownsand.ExecParams) (*ownsand.ExecResult, error) {
	p.mu.Lock()
	p.commands = append(p.commands, e.Command)
	p.mu.Unlock()
	joined := strings.Join(e.Command, " ")
	if strings.Contains(joined, "--jitconfig") {
		p.mu.Lock()
		gate, entered := p.gate, p.entered
		p.mu.Unlock()
		if gate != nil {
			select {
			case entered <- sandboxID:
			default:
			}
			select {
			case <-gate:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		p.mu.Lock()
		if p.running[sandboxID] == nil {
			p.running[sandboxID] = make(chan struct{})
		}
		p.mu.Unlock()
		if e.OnEvent != nil {
			if err := e.OnEvent(ownsand.ExecEvent{Kind: ownsand.ExecEventStarted, ExecID: e.ExecID}); err != nil {
				return nil, err
			}
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	code := int64(0)
	return &ownsand.ExecResult{ExecID: e.ExecID, Kind: ownsand.ExecResultExited, ExitCode: &code}, nil
}

func (p *programs) attach(ctx context.Context, sandboxID, execID string, _ ownsand.AttachParams) (*ownsand.ExecResult, error) {
	p.mu.Lock()
	ch := p.running[sandboxID]
	p.mu.Unlock()
	if ch == nil {
		return nil, connect.NewError(connect.CodeNotFound, nil)
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-ch:
	}
	code := int64(0)
	return &ownsand.ExecResult{ExecID: execID, Kind: ownsand.ExecResultExited, ExitCode: &code}, nil
}

// exit ends the runner program of a sandbox.
func (p *programs) exit(sandboxID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ch := p.running[sandboxID]; ch != nil {
		select {
		case <-ch:
		default:
			close(ch)
		}
	}
}

func (p *programs) all() [][]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([][]string(nil), p.commands...)
}

type ghWorld struct {
	env    *testenv.Env
	os     *ownsandfake.Fake
	gh     *githubfake.Fake
	l      *layers.Layers
	org    testenv.Org
	osOrg  string
	progs  *programs
	reader *sdkmetric.ManualReader
	ghLogs *testenv.SafeBuffer
}

type worldOpts struct {
	unconfigured   bool
	manualDispatch bool
	warm           int32
}

func newGHWorld(t *testing.T, o worldOpts) *ghWorld {
	t.Helper()
	w := &ghWorld{os: ownsandfake.New(), l: &layers.Layers{}, progs: &programs{running: map[string]chan struct{}{}}, reader: sdkmetric.NewManualReader(),
		ghLogs: &testenv.SafeBuffer{}}
	// The fake GitHub and the client use the clock of the control plane,
	// which exists only after testenv.New.
	var clock atomic.Pointer[testenv.Clock]
	now := func() time.Time {
		if c := clock.Load(); c != nil {
			return c.Now()
		}
		return time.Now()
	}
	w.gh = githubfake.New(now)
	t.Cleanup(w.gh.Server.Close)
	w.os.SetExec(w.progs.exec, w.progs.attach)
	opts := layers.Options{OwnSand: w.os}
	if !o.unconfigured {
		opts.GitHub = w.githubOptions(now)
	}
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(w.reader))
	w.env = testenv.New(t, testenv.WithLayerOptions(opts, w.l), func(ao *app.Options) {
		ao.Metrics = provider
		if o.manualDispatch {
			// Hold the lease elsewhere so only explicit passes run in the test.
			if _, err := ao.DB.Pool.Exec(t.Context(), "INSERT INTO instance_leases (lease_name, holder, expire_time) VALUES ($1, $2, $3)",
				lease.Name, "manual-dispatch", ao.Now().Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
		}
	})
	clock.Store(w.env.Clock)
	t.Cleanup(func() {
		if t.Failed() {
			for _, line := range strings.Split(w.env.Logs.String(), "\n") {
				if strings.Contains(line, `"level":"WARN"`) || strings.Contains(line, `"level":"ERROR"`) || strings.Contains(line, "state_changed") || strings.Contains(line, "github") {
					t.Log(line)
				}
			}
		}
	})
	w.gh.Target = w.env.Server.URL + "/webhooks/github"
	w.gh.TargetClient = w.env.Server.Client()
	w.org = w.env.NewOrg("alice")
	w.osOrg = api.NewID()
	key := "osk_" + strings.Repeat("a", 43)
	w.os.AddOrg(w.osOrg, key)
	if _, err := w.env.User(w.org.Admin).OwnSandLink.SetOwnSandLink(context.Background(), connect.NewRequest(&tidev1.SetOwnSandLinkRequest{
		OrganizationId: w.org.ID, OwnsandOrganizationId: w.osOrg, OwnsandApiKey: key})); err != nil {
		t.Fatal(err)
	}
	return w
}

func (w *ghWorld) githubOptions(now func() time.Time) github.Options {
	return github.Options{
		Client:        githubapi.New(w.gh.Config(), nil, slog.New(slog.NewJSONHandler(w.ghLogs, nil)), now),
		WebhookSecret: w.gh.WebhookSecret,
		Timing:        github.Timing{RedeliveryInterval: time.Hour, RedeliveryWindow: 24 * time.Hour, PrepareTimeout: time.Minute, RegistrationGiveUp: time.Minute},
	}
}

func wantCode(t *testing.T, what string, err error, want connect.Code) {
	t.Helper()
	if got := testenv.Code(err); got != want {
		t.Fatalf("%s: code %v (%v), want %v", what, got, err, want)
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func stays(t *testing.T, what string, cond func() bool) {
	t.Helper()
	until := time.Now().Add(700 * time.Millisecond)
	for time.Now().Before(until) {
		if !cond() {
			t.Fatalf("%s changed", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// link installs the app on a GitHub organization and links it through
// the RPCs, as the console does.
func (w *ghWorld) link(t *testing.T, ghID int64, login string) *tidev1.GitHubInstallation {
	t.Helper()
	w.gh.AddInstallation(ghID, login, "Organization", "octo")
	state := w.installState(t, w.org.Admin)
	code := "code-" + strconv.FormatInt(ghID, 10)
	w.gh.AddCode(code, "octo")
	resp, err := w.env.User(w.org.Admin).GitHub.LinkGitHubInstallation(context.Background(), connect.NewRequest(&tidev1.LinkGitHubInstallationRequest{
		OrganizationId: w.org.ID, GithubInstallationId: ghID, Code: code, State: state}))
	if err != nil {
		t.Fatal(err)
	}
	return resp.Msg.GetInstallation()
}

func (w *ghWorld) installState(t *testing.T, user string) string {
	t.Helper()
	resp, err := w.env.User(user).GitHub.CreateGitHubInstallUrl(context.Background(), connect.NewRequest(&tidev1.CreateGitHubInstallUrlRequest{OrganizationId: w.org.ID}))
	if err != nil {
		t.Fatal(err)
	}
	_, state, ok := strings.Cut(resp.Msg.GetUrl(), "?state=")
	if !ok || state == "" {
		t.Fatalf("url %s", resp.Msg.GetUrl())
	}
	return state
}

func ghProfile(name string, warm int32) *tidev1.RunnerProfile {
	return &tidev1.RunnerProfile{
		Name: name, WorkloadKind: tidev1.WorkloadKind_WORKLOAD_KIND_GITHUB_JOB, GuestOs: tidev1.GuestOs_GUEST_OS_LINUX,
		Source:    &tidev1.RunnerProfile_BaseImage{BaseImage: tidev1.GuestOs_GUEST_OS_LINUX},
		Resources: &tidev1.Resources{VcpuCount: 2, MemoryMib: 2048, DiskGib: 10}, NetworkMode: tidev1.NetworkMode_NETWORK_MODE_OUTBOUND,
		IdleTtl: durationpb.New(time.Hour), MaxTtl: durationpb.New(4 * time.Hour), MaxQueueWait: durationpb.New(time.Hour), WarmCount: warm,
	}
}

func (w *ghWorld) createProfile(t *testing.T, p *tidev1.RunnerProfile) string {
	t.Helper()
	resp, err := w.env.User(w.org.Admin).Profile.CreateRunnerProfile(context.Background(), connect.NewRequest(&tidev1.CreateRunnerProfileRequest{OrganizationId: w.org.ID, Profile: p}))
	if err != nil {
		t.Fatal(err)
	}
	return resp.Msg.GetProfile().GetRunnerProfileId()
}

// job builds a workflow_job payload.
func job(action string, ghInstID, jobID int64, labels []string, runnerName string) map[string]any {
	wj := map[string]any{"id": jobID, "run_id": jobID * 10, "name": "build", "labels": labels, "status": action, "runner_name": nil}
	if runnerName != "" {
		wj["runner_name"] = runnerName
	}
	return map[string]any{"action": action, "installation": map[string]any{"id": ghInstID}, "repository": map[string]any{"full_name": "acme/app"},
		"sender": map[string]any{"login": "octo"}, "workflow_job": wj}
}

func installationEvent(action string, ghInstID int64, login string) map[string]any {
	return map[string]any{"action": action, "installation": map[string]any{"id": ghInstID, "account": map[string]any{"login": login, "type": "Organization"},
		"target_type": "Organization"}, "sender": map[string]any{"login": "octo"}}
}

func (w *ghWorld) send(t *testing.T, event string, payload any) string {
	t.Helper()
	guid, status := w.gh.Send(event, payload)
	if status != 204 {
		t.Fatalf("delivery of %s: status %d", event, status)
	}
	return guid
}

func (w *ghWorld) requests(t *testing.T) []*tidev1.Request {
	t.Helper()
	resp, err := w.env.User(w.org.Owner).Work.ListRequests(context.Background(), connect.NewRequest(&tidev1.ListRequestsRequest{OrganizationId: w.org.ID, IncludeEnded: true}))
	if err != nil {
		t.Fatal(err)
	}
	return resp.Msg.GetRequests()
}

func (w *ghWorld) requestOfJob(t *testing.T, jobID int64) *tidev1.Request {
	t.Helper()
	for _, r := range w.requests(t) {
		if r.GetSourceKey() == strconv.FormatInt(jobID, 10) {
			return r
		}
	}
	return nil
}

func (w *ghWorld) runners(t *testing.T) []*tidev1.Runner {
	t.Helper()
	resp, err := w.env.User(w.org.Owner).Work.ListRunners(context.Background(), connect.NewRequest(&tidev1.ListRunnersRequest{OrganizationId: w.org.ID, IncludeEnded: true}))
	if err != nil {
		t.Fatal(err)
	}
	return resp.Msg.GetRunners()
}

func (w *ghWorld) runner(t *testing.T, id string) *tidev1.Runner {
	t.Helper()
	resp, err := w.env.User(w.org.Owner).Work.GetRunner(context.Background(), connect.NewRequest(&tidev1.GetRunnerRequest{OrganizationId: w.org.ID, RunnerId: id}))
	if err != nil {
		t.Fatal(err)
	}
	return resp.Msg.GetRunner()
}

func (w *ghWorld) runnersIn(t *testing.T, state tidev1.RunnerState) []*tidev1.Runner {
	t.Helper()
	var out []*tidev1.Runner
	for _, r := range w.runners(t) {
		if r.GetState() == state {
			out = append(out, r)
		}
	}
	return out
}

// webhookCounts returns tide_webhook_deliveries_total by result.
func (w *ghWorld) webhookCounts(t *testing.T) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := w.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "tide_webhook_deliveries" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				sender, _ := dp.Attributes.Value("sender")
				result, _ := dp.Attributes.Value("result")
				if sender.AsString() == "github" {
					got[result.AsString()] = dp.Value
				}
			}
		}
	}
	return got
}
