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

	"github.com/delinoio/mono/go/ownsand"
	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/servers/sandrun-api/internal/agent"
	"github.com/delinoio/mono/servers/sandrun-api/internal/api"
	"github.com/delinoio/mono/servers/sandrun-api/internal/app"
	"github.com/delinoio/mono/servers/sandrun-api/internal/layers"
	"github.com/delinoio/mono/servers/sandrun-api/internal/ownsandclient/ownsandfake"
	"github.com/delinoio/mono/servers/sandrun-api/internal/testenv"
	"github.com/delinoio/mono/servers/sandrun-api/internal/work"
)

// programAdapter is a test AGENT_SESSION adapter that starts a runner
// program, as the vendor layers will.
type programAdapter struct {
	work.BaseAdapter
	started chan string
}

func (programAdapter) Kind() sandrunv1.WorkloadKind {
	return sandrunv1.WorkloadKind_WORKLOAD_KIND_AGENT_SESSION
}
func (programAdapter) BindsAtDispatch() bool                      { return true }
func (programAdapter) Prepare(context.Context, *work.Guest) error { return nil }
func (programAdapter) ProgramExited(req *work.Request, res *ownsand.ExecResult) (work.RequestEnd, bool) {
	return new(agent.Adapter).ProgramExited(req, res)
}
func (a programAdapter) Start(ctx context.Context, g *work.Guest) (work.StartResult, error) {
	execID := api.NewID()
	if _, err := g.Client.Exec(ctx, g.Runner.SandboxID, ownsand.ExecParams{ExecID: execID, Command: []string{"agent", "worker", "start"}}); err != nil {
		return work.StartResult{}, err
	}
	a.started <- g.Runner.ID
	return work.StartResult{ProgramExecID: execID}, nil
}

type programAttachScenario int

const (
	attachOnce programAttachScenario = iota
	attachAfterKeyRotation
	attachAfterInactiveLink
	attachNotFound
)

type runnerProgramCase struct {
	name         string
	exitCode     int64
	runnerReason sandrunv1.RunnerEndReason
	requestEnd   work.RequestEnd
	endRequest   bool
	deleteRunner bool
	attach       programAttachScenario
}

func TestRunnerProgramLifecycle(t *testing.T) {
	for _, tc := range []runnerProgramCase{
		{
			name: "successful exit", runnerReason: sandrunv1.RunnerEndReason_RUNNER_END_REASON_WORK_DONE,
			requestEnd: work.RequestEnd{State: sandrunv1.RequestState_REQUEST_STATE_ENDED, Reason: sandrunv1.RequestEndReason_REQUEST_END_REASON_COMPLETED},
		},
		{
			name: "key rotation during attach", attach: attachAfterKeyRotation,
			runnerReason: sandrunv1.RunnerEndReason_RUNNER_END_REASON_WORK_DONE,
			requestEnd:   work.RequestEnd{State: sandrunv1.RequestState_REQUEST_STATE_ENDED, Reason: sandrunv1.RequestEndReason_REQUEST_END_REASON_COMPLETED},
		},
		{
			name: "temporarily inactive link", attach: attachAfterInactiveLink,
			runnerReason: sandrunv1.RunnerEndReason_RUNNER_END_REASON_WORK_DONE,
			requestEnd:   work.RequestEnd{State: sandrunv1.RequestState_REQUEST_STATE_ENDED, Reason: sandrunv1.RequestEndReason_REQUEST_END_REASON_COMPLETED},
		},
		{
			name: "exec not found", attach: attachNotFound,
		},
		{
			name: "nonzero exit", exitCode: 7, runnerReason: sandrunv1.RunnerEndReason_RUNNER_END_REASON_WORK_DONE,
			requestEnd: work.RequestEnd{State: sandrunv1.RequestState_REQUEST_STATE_FAILED, Reason: sandrunv1.RequestEndReason_REQUEST_END_REASON_RUNNER_LOST},
		},
		{
			name: "request ends during retry", endRequest: true, runnerReason: sandrunv1.RunnerEndReason_RUNNER_END_REASON_WORK_DONE,
			requestEnd: work.RequestEnd{State: sandrunv1.RequestState_REQUEST_STATE_CANCELED, Reason: sandrunv1.RequestEndReason_REQUEST_END_REASON_CANCELED_BY_USER},
		},
		{
			name: "runner deletion during retry", deleteRunner: true, runnerReason: sandrunv1.RunnerEndReason_RUNNER_END_REASON_DELETED_BY_USER,
			requestEnd: work.RequestEnd{State: sandrunv1.RequestState_REQUEST_STATE_FAILED, Reason: sandrunv1.RequestEndReason_REQUEST_END_REASON_RUNNER_LOST},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testRunnerProgramExit(t, tc)
		})
	}
}

func testRunnerProgramExit(t *testing.T, tc runnerProgramCase) {
	t.Helper()
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
	ctx := t.Context()
	type execTarget struct{ sandboxID, execID string }
	firstAttach := make(chan execTarget, 1)
	allowRetry := make(chan struct{})
	exit := make(chan struct{})
	exitTarget := make(chan execTarget, 1)
	first := true // Only the program follower calls AttachExec.
	fake.SetExec(nil, func(ctx context.Context, sandboxID, execID string, _ ownsand.AttachParams) (*ownsand.ExecResult, error) {
		target := execTarget{sandboxID, execID}
		if first {
			first = false
			firstAttach <- target
			switch tc.attach {
			case attachNotFound:
				return nil, connect.NewWireError(connect.CodeNotFound, errors.New("exec not found"))
			case attachAfterKeyRotation, attachAfterInactiveLink:
				select {
				case <-allowRetry:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				if tc.attach == attachAfterKeyRotation {
					return nil, connect.NewWireError(connect.CodeUnauthenticated, errors.New("old key revoked"))
				}
				return nil, connect.NewError(connect.CodeUnavailable, errors.New("attach interrupted"))
			}
		}
		select {
		case <-exit:
			select {
			case exitTarget <- target:
			default:
			}
			return &ownsand.ExecResult{ExecID: execID, Kind: ownsand.ExecResultExited, ExitCode: &tc.exitCode}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	in, err := env.User(w.org.Admin).Integration.CreateAgentIntegration(ctx, connect.NewRequest(&sandrunv1.CreateAgentIntegrationRequest{OrganizationId: w.org.ID,
		Vendor: sandrunv1.AgentVendor_AGENT_VENDOR_CURSOR, Name: "cursor",
		Settings:    &sandrunv1.CreateAgentIntegrationRequest_Cursor{Cursor: &sandrunv1.CursorSettings{PoolName: "pool"}},
		Credentials: &sandrunv1.CreateAgentIntegrationRequest_CursorCredentials{CursorCredentials: &sandrunv1.CursorCredentials{ServiceAccountApiKey: "k"}}}))
	if err != nil {
		t.Fatal(err)
	}
	pp := devProfileProto("agent", 0, 0)
	pp.WorkloadKind, pp.WorkspacePath, pp.AgentIntegrationId = sandrunv1.WorkloadKind_WORKLOAD_KIND_AGENT_SESSION, "", in.Msg.GetIntegration().GetAgentIntegrationId()
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
		return w.runner(t, runnerID).GetState() == sandrunv1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK
	})
	if err := l.Engine.MarkBusy(ctx, w.org.ID, runnerID, ""); err != nil {
		t.Fatal(err)
	}
	var original execTarget
	select {
	case original = <-firstAttach:
	case <-time.After(15 * time.Second):
		t.Fatal("the follower did not attach")
	}
	active := sandrunv1.OwnSandLinkState_OWN_SAND_LINK_STATE_ACTIVE
	switch tc.attach {
	case attachAfterKeyRotation:
		fake.AddOrg(w.osOrg, ownsandKey('b'))
		replacement, err := w.setLink(w.org.Admin, w.osOrg, ownsandKey('b'))
		if err != nil || replacement.GetState() != active {
			t.Fatalf("replace the OwnSand key: %v, %v", replacement, err)
		}
		close(allowRetry)
	case attachAfterInactiveLink:
		const setLinkState = "UPDATE ownsand_links SET state = $2 WHERE organization_id = $1"
		if _, err := env.DB.Pool.Exec(ctx, setLinkState, w.org.ID, int16(sandrunv1.OwnSandLinkState_OWN_SAND_LINK_STATE_INVALID)); err != nil {
			t.Fatal(err)
		}
		close(allowRetry)
		eventually(t, "the interrupted attach", func() bool {
			return strings.Contains(env.Logs.String(), `"rpc":"AttachExec","code":"unavailable"`)
		})
		stays(t, "attach count while the link is inactive", func() bool { return fake.Calls("AttachExec") == 1 })
		if _, err := env.DB.Pool.Exec(ctx, setLinkState, w.org.ID, int16(active)); err != nil {
			t.Fatal(err)
		}
	case attachNotFound:
		stays(t, "busy runner with one attach after not_found", func() bool {
			return fake.Calls("AttachExec") == 1 && w.runner(t, runnerID).GetState() == sandrunv1.RunnerState_RUNNER_STATE_BUSY
		})
		return
	}
	restore := env.FailProgramExitCommits()
	close(exit)
	eventually(t, "PostgreSQL rejecting the exit commit", func() bool {
		return strings.Contains(env.Logs.String(), "injected program exit commit failure")
	})
	got, err := l.Engine.GetRequest(ctx, env.DB.Pool, w.org.ID, req.ID)
	if err != nil || got.State != sandrunv1.RequestState_REQUEST_STATE_RUNNING || got.StartTime == nil ||
		w.runner(t, runnerID).GetState() != sandrunv1.RunnerState_RUNNER_STATE_BUSY || fake.Active(w.osOrg) != 1 {
		t.Fatalf("after failed commit: request %+v, error %v", got, err)
	}
	if target := <-exitTarget; target != original {
		t.Fatalf("exit attach targeted %+v, want %+v", target, original)
	}
	if tc.attach == attachAfterKeyRotation {
		lk, err := env.User(w.org.Admin).OwnSandLink.GetOwnSandLink(ctx, connect.NewRequest(&sandrunv1.GetOwnSandLinkRequest{OrganizationId: w.org.ID}))
		if err != nil || lk.Msg.GetLink().GetState() != active {
			t.Fatalf("late rejection of the old key affected the active link: %v, %v", lk, err)
		}
	}
	if tc.endRequest {
		if _, err := l.Engine.EndRequest(ctx, w.org.ID, req.ID, tc.requestEnd); err != nil {
			t.Fatal(err)
		}
	}
	if tc.deleteRunner {
		// Keep the runner in DELETING while the follower retries the exit.
		fake.Update(w.osOrg, func(o *ownsandfake.Org) { o.DeleteError = errors.New("sandbox deletion temporarily unavailable") })
		resp, err := env.User(w.org.Admin).Work.DeleteRunner(ctx, connect.NewRequest(&sandrunv1.DeleteRunnerRequest{
			OrganizationId: w.org.ID, RunnerId: runnerID,
		}))
		if err != nil || resp.Msg.GetRunner().GetState() != sandrunv1.RunnerState_RUNNER_STATE_DELETING {
			t.Fatalf("start concurrent deletion: %v, %v", resp, err)
		}
	}
	restore()
	if tc.deleteRunner {
		stays(t, "the user's deletion", func() bool {
			r := w.runner(t, runnerID)
			return r.GetState() == sandrunv1.RunnerState_RUNNER_STATE_DELETING && r.GetEndReason() == tc.runnerReason
		})
		fake.Update(w.osOrg, func(o *ownsandfake.Org) { o.DeleteError = nil })
	}
	eventually(t, "runner deleted", func() bool { return w.runner(t, runnerID).GetState() == sandrunv1.RunnerState_RUNNER_STATE_DELETED })
	r := w.runner(t, runnerID)
	got, err = l.Engine.GetRequest(ctx, env.DB.Pool, w.org.ID, req.ID)
	if err != nil || r.GetEndReason() != tc.runnerReason || got.State != tc.requestEnd.State || got.EndReason != tc.requestEnd.Reason || fake.Active(w.osOrg) != 0 {
		t.Fatalf("after exit: runner %v request %+v active %d error %v", r, got, fake.Active(w.osOrg), err)
	}
	wantAttach := 1
	if tc.attach != attachOnce {
		wantAttach = 2
	}
	if attach, exec := fake.Calls("AttachExec"), fake.Calls("Exec"); attach != wantAttach || exec != 1 {
		t.Fatalf("program used %d attach calls and %d execs, want %d attach calls and one exec", attach, exec, wantAttach)
	}
}
