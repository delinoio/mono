// SPDX-License-Identifier: FSL-1.1-ALv2

package layers_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	"github.com/delinoio/mono/go/ownsand"
	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/servers/tide-api/internal/api"
	"github.com/delinoio/mono/servers/tide-api/internal/app"
	"github.com/delinoio/mono/servers/tide-api/internal/layers"
	"github.com/delinoio/mono/servers/tide-api/internal/ownsandclient/ownsandfake"
	"github.com/delinoio/mono/servers/tide-api/internal/testenv"
	"github.com/delinoio/mono/servers/tide-api/internal/work"
)

// programAdapter is a test AGENT_SESSION adapter that starts a runner
// program, as the vendor layers will.
type programAdapter struct {
	work.BaseAdapter
	started chan string
}

func (programAdapter) Kind() tidev1.WorkloadKind {
	return tidev1.WorkloadKind_WORKLOAD_KIND_AGENT_SESSION
}
func (programAdapter) BindsAtDispatch() bool                      { return true }
func (programAdapter) Prepare(context.Context, *work.Guest) error { return nil }
func (a programAdapter) Start(ctx context.Context, g *work.Guest) (work.StartResult, error) {
	execID := api.NewID()
	if _, err := g.Client.Exec(ctx, g.Runner.SandboxID, ownsand.ExecParams{ExecID: execID, Command: []string{"agent", "worker", "start"}}); err != nil {
		return work.StartResult{}, err
	}
	a.started <- g.Runner.ID
	return work.StartResult{ProgramExecID: execID}, nil
}

// TestRunnerProgramLifecycle: a program runner waits for work, becomes BUSY
// when the source reports it, and its exit ends the request and deletes the
// sandbox. The engine follows the program with AttachExec.
func TestRunnerProgramLifecycle(t *testing.T) {
	fake := ownsandfake.New()
	l := &layers.Layers{}
	adapter := programAdapter{started: make(chan string, 1)}
	env := testenv.New(t, testenv.WithLayerOptions(layers.Options{OwnSand: fake}, l), testenv.WithExtension(func(r *app.Registry) error {
		l.Engine.RegisterAdapter(adapter)
		return nil
	}))
	w := &workWorld{world: &world{env: env, fake: fake, l: l, org: env.NewOrg("alice")}, osOrg: api.NewID()}
	fake.AddOrg(w.osOrg, ownsandKey('a'))
	if _, err := w.setLink(w.org.Admin, w.osOrg, ownsandKey('a')); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	exit := make(chan struct{})
	fake.SetExec(nil, func(ctx context.Context, sandboxID, execID string, p ownsand.AttachParams) (*ownsand.ExecResult, error) {
		select {
		case <-exit:
			code := int64(0)
			return &ownsand.ExecResult{ExecID: execID, Kind: ownsand.ExecResultExited, ExitCode: &code}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	in, err := env.User(w.org.Admin).Integration.CreateAgentIntegration(ctx, connect.NewRequest(&tidev1.CreateAgentIntegrationRequest{OrganizationId: w.org.ID,
		Vendor: tidev1.AgentVendor_AGENT_VENDOR_CURSOR, Name: "cursor",
		Settings:    &tidev1.CreateAgentIntegrationRequest_Cursor{Cursor: &tidev1.CursorSettings{PoolName: "pool"}},
		Credentials: &tidev1.CreateAgentIntegrationRequest_CursorCredentials{CursorCredentials: &tidev1.CursorCredentials{ServiceAccountApiKey: "k"}}}))
	if err != nil {
		t.Fatal(err)
	}
	pp := devProfileProto("agent", 0, 0)
	pp.WorkloadKind, pp.WorkspacePath, pp.AgentIntegrationId = tidev1.WorkloadKind_WORKLOAD_KIND_AGENT_SESSION, "", in.Msg.GetIntegration().GetAgentIntegrationId()
	profileID := w.createProfile(t, pp)
	p, err := l.Profiles.Get(ctx, env.DB.Pool, w.org.ID, profileID)
	if err != nil {
		t.Fatal(err)
	}
	var req *work.Request
	err = env.DB.InTx(ctx, func(tx pgx.Tx) error {
		var created bool
		req, created, err = l.Engine.CreateRequest(ctx, tx, work.NewRequest{Profile: p, Source: work.SourceCursor, SourceKey: "cursor-req-1"})
		if err == nil && !created {
			err = errors.New("not created")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	// A repeated claim does not create a second request.
	err = env.DB.InTx(ctx, func(tx pgx.Tx) error {
		again, created, err := l.Engine.CreateRequest(ctx, tx, work.NewRequest{Profile: p, Source: work.SourceCursor, SourceKey: "cursor-req-1"})
		if err == nil && (created || again.ID != req.ID) {
			err = errors.New("second request")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	env.App.Service.WorkChanged(w.org.ID)
	var runnerID string
	select {
	case runnerID = <-adapter.started:
	case <-time.After(15 * time.Second):
		t.Fatal("the program did not start")
	}
	eventually(t, "WAITING_FOR_WORK", func() bool {
		return w.runner(t, runnerID).GetState() == tidev1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK
	})
	if err := l.Engine.MarkBusy(ctx, w.org.ID, runnerID, ""); err != nil {
		t.Fatal(err)
	}
	got, _ := l.Engine.GetRequest(ctx, env.DB.Pool, w.org.ID, req.ID)
	if got.State != tidev1.RequestState_REQUEST_STATE_RUNNING || got.StartTime == nil {
		t.Fatalf("request %+v", got)
	}
	close(exit)
	eventually(t, "runner deleted", func() bool { return w.runner(t, runnerID).GetState() == tidev1.RunnerState_RUNNER_STATE_DELETED })
	r := w.runner(t, runnerID)
	got, _ = l.Engine.GetRequest(ctx, env.DB.Pool, w.org.ID, req.ID)
	if r.GetEndReason() != tidev1.RunnerEndReason_RUNNER_END_REASON_WORK_DONE || got.State != tidev1.RequestState_REQUEST_STATE_ENDED ||
		got.EndReason != tidev1.RequestEndReason_REQUEST_END_REASON_COMPLETED || fake.Active(w.osOrg) != 0 {
		t.Fatalf("after exit: runner %v request %+v active %d", r, got, fake.Active(w.osOrg))
	}
}
