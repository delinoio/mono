// SPDX-License-Identifier: FSL-1.1-ALv2

package layers_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/delinoio/mono/go/ownsand"
	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/servers/tide-api/internal/agent"
	"github.com/delinoio/mono/servers/tide-api/internal/api"
	"github.com/delinoio/mono/servers/tide-api/internal/layers"
	"github.com/delinoio/mono/servers/tide-api/internal/ownsandclient/ownsandfake"
	"github.com/delinoio/mono/servers/tide-api/internal/testenv"
)

// fastAgentTiming returns watcher and poller intervals for tests.
func fastAgentTiming() agent.Timing {
	return agent.Timing{SyncInterval: 50 * time.Millisecond, PollInterval: 50 * time.Millisecond, BackoffMin: 20 * time.Millisecond, BackoffMax: 200 * time.Millisecond}
}

// guestRecorder is the fake guest of agent session runners: setup scripts
// exit with 0, a runner program start script reports `started` and its
// program runs until the test ends it with exitProgram. It records every
// exec for the credential checks.
type guestRecorder struct {
	mu    sync.Mutex
	execs []ownsand.ExecParams
	// programs maps a sandbox ID to the exit channel of its program.
	programs map[string]chan int64
	started  map[string]string // sandbox ID -> start script
	// installCode is the exit code of install scripts (0: success).
	installCode int64
}

func newGuestRecorder(fake *ownsandfake.Fake) *guestRecorder {
	g := &guestRecorder{programs: map[string]chan int64{}, started: map[string]string{}}
	fake.SetExec(func(ctx context.Context, sandboxID string, p ownsand.ExecParams) (*ownsand.ExecResult, error) {
		script := p.Command[len(p.Command)-1]
		g.mu.Lock()
		g.execs = append(g.execs, p)
		isProgram := strings.Contains(script, agent.LogFile)
		code := int64(0)
		if strings.Contains(script, "install") || strings.Contains(script, "irm ") {
			code = g.installCode
		}
		if isProgram {
			g.started[sandboxID] = script
			if g.programs[sandboxID] == nil {
				g.programs[sandboxID] = make(chan int64, 1)
			}
		}
		g.mu.Unlock()
		if isProgram {
			if err := p.OnEvent(ownsand.ExecEvent{Kind: ownsand.ExecEventStarted, ExecID: p.ExecID}); err != nil {
				return nil, err
			}
		}
		return &ownsand.ExecResult{ExecID: p.ExecID, Kind: ownsand.ExecResultExited, ExitCode: &code}, nil
	}, func(ctx context.Context, sandboxID, execID string, p ownsand.AttachParams) (*ownsand.ExecResult, error) {
		g.mu.Lock()
		ch := g.programs[sandboxID]
		g.mu.Unlock()
		if ch == nil {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		select {
		case code := <-ch:
			return &ownsand.ExecResult{ExecID: execID, Kind: ownsand.ExecResultExited, ExitCode: &code}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	return g
}

func (g *guestRecorder) exitProgram(sandboxID string, code int64) {
	g.mu.Lock()
	ch := g.programs[sandboxID]
	g.mu.Unlock()
	ch <- code
}

func (g *guestRecorder) startScript(sandboxID string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.started[sandboxID]
}

// all returns every exec command and environment value as one text.
func (g *guestRecorder) all() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var b strings.Builder
	for _, p := range g.execs {
		b.WriteString(strings.Join(p.Command, " "))
		for k, v := range p.Env {
			b.WriteString(" " + k + "=" + v)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// agentWorld is an organization with an ACTIVE OwnSand link, the vendor
// fakes and the fake guest.
type agentWorld struct {
	*workWorld
	guest *guestRecorder
}

func newAgentWorld(t *testing.T, opts layers.Options) *agentWorld {
	t.Helper()
	w := &world{fake: ownsandfake.New(), l: &layers.Layers{}}
	opts.OwnSand = w.fake
	opts.AgentTiming = fastAgentTiming()
	w.env = testenv.New(t, testenv.WithLayerOptions(opts, w.l))
	w.org = w.env.NewOrg("alice")
	ww := &workWorld{world: w, osOrg: api.NewID()}
	w.fake.AddOrg(ww.osOrg, ownsandKey('a'))
	if _, err := w.setLink(w.org.Admin, ww.osOrg, ownsandKey('a')); err != nil {
		t.Fatal(err)
	}
	return &agentWorld{workWorld: ww, guest: newGuestRecorder(w.fake)}
}

func (w *agentWorld) createIntegration(t *testing.T, req *tidev1.CreateAgentIntegrationRequest) string {
	t.Helper()
	req.OrganizationId = w.org.ID
	resp, err := w.env.User(w.org.Admin).Integration.CreateAgentIntegration(context.Background(), connect.NewRequest(req))
	if err != nil {
		t.Fatal(err)
	}
	return resp.Msg.GetIntegration().GetAgentIntegrationId()
}

func agentProfileProto(name, integrationID string, guest tidev1.GuestOs, maxWait time.Duration) *tidev1.RunnerProfile {
	return &tidev1.RunnerProfile{
		Name: name, WorkloadKind: tidev1.WorkloadKind_WORKLOAD_KIND_AGENT_SESSION, AgentIntegrationId: integrationID,
		GuestOs: guest, Source: &tidev1.RunnerProfile_BaseImage{BaseImage: guest},
		Resources: &tidev1.Resources{VcpuCount: 2, MemoryMib: 2048, DiskGib: 10}, NetworkMode: tidev1.NetworkMode_NETWORK_MODE_OUTBOUND,
		IdleTtl: durationpb.New(time.Hour), MaxTtl: durationpb.New(4 * time.Hour), MaxQueueWait: durationpb.New(maxWait),
	}
}

// requests returns the requests of the organization, including ended ones.
func (w *agentWorld) requests(t *testing.T) []*tidev1.Request {
	t.Helper()
	resp, err := w.env.User(w.org.Owner).Work.ListRequests(context.Background(), connect.NewRequest(&tidev1.ListRequestsRequest{OrganizationId: w.org.ID, IncludeEnded: true}))
	if err != nil {
		t.Fatal(err)
	}
	return resp.Msg.GetRequests()
}

// requestBySource waits for the request with a source key and returns it.
func (w *agentWorld) requestBySource(t *testing.T, key string) *tidev1.Request {
	t.Helper()
	var out *tidev1.Request
	eventually(t, "request "+key, func() bool {
		for _, r := range w.requests(t) {
			if r.GetSourceKey() == key {
				out = r
				return true
			}
		}
		return false
	})
	return out
}

func (w *agentWorld) request(t *testing.T, id string) *tidev1.Request {
	t.Helper()
	resp, err := w.env.User(w.org.Owner).Work.GetRequest(context.Background(), connect.NewRequest(&tidev1.GetRequestRequest{OrganizationId: w.org.ID, RequestId: id}))
	if err != nil {
		t.Fatal(err)
	}
	return resp.Msg.GetRequest()
}

func (w *agentWorld) waitRequest(t *testing.T, id string, state tidev1.RequestState) *tidev1.Request {
	t.Helper()
	var r *tidev1.Request
	eventually(t, "request "+state.String(), func() bool {
		r = w.request(t, id)
		return r.GetState() == state
	})
	return r
}

func (w *agentWorld) waitRunner(t *testing.T, id string, state tidev1.RunnerState) *tidev1.Runner {
	t.Helper()
	var r *tidev1.Runner
	eventually(t, "runner "+state.String(), func() bool {
		r = w.runner(t, id)
		return r.GetState() == state
	})
	return r
}

func (w *agentWorld) cancel(t *testing.T, id string) {
	t.Helper()
	if _, err := w.env.User(w.org.Admin).Work.CancelRequest(context.Background(), connect.NewRequest(&tidev1.CancelRequestRequest{OrganizationId: w.org.ID, RequestId: id})); err != nil {
		t.Fatal(err)
	}
}
