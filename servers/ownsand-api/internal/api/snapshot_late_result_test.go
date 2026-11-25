// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/app"
	"github.com/delinoio/mono/servers/ownsand-api/internal/db"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

type snapshotCreateCommitGate struct {
	transactions sync.Map
	committed    chan struct{}
	resume       chan struct{}
}

type snapshotCreateCommitKey struct{}

func (g *snapshotCreateCommitGate) TraceQueryStart(ctx context.Context, conn *pgx.Conn, query pgx.TraceQueryStartData) context.Context {
	switch {
	case strings.HasPrefix(query.SQL, "INSERT INTO snapshots "):
		g.transactions.Store(conn, true)
	case query.SQL == "commit", query.SQL == "rollback":
		if _, creating := g.transactions.LoadAndDelete(conn); creating && query.SQL == "commit" {
			return context.WithValue(ctx, snapshotCreateCommitKey{}, true)
		}
	}
	return ctx
}

func (g *snapshotCreateCommitGate) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, query pgx.TraceQueryEndData) {
	if ctx.Value(snapshotCreateCommitKey{}) != nil && query.Err == nil {
		close(g.committed)
		<-g.resume
	}
}

func TestLateCreateSnapshotResultAfterDelete(t *testing.T) {
	for _, tc := range []struct {
		name       string
		failCreate bool
	}{
		{name: "successful capture"},
		{name: "failed capture", failCreate: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			gate := &snapshotCreateCommitGate{committed: make(chan struct{}), resume: make(chan struct{})}
			var resumeOnce sync.Once
			resume := func() { resumeOnce.Do(func() { close(gate.resume) }) }
			defer resume()
			env := testenv.New(t, func(o *app.Options) {
				cfg := o.DB.Pool.Config()
				cfg.ConnConfig.Tracer = gate
				pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(pool.Close)
				o.DB = &db.DB{Pool: pool}
			})
			org := env.NewOrg("alice")
			dev, agent := setupAgent(t, env, org, nil)
			client := org.OwnerClient
			sandbox, err := client.Sandbox.CreateSandbox(ctx, connect.NewRequest(createReq(org.ID)))
			if err != nil {
				t.Fatal(err)
			}
			sandboxID := sandbox.Msg.GetSandbox().GetSandboxId()
			waitState(t, client, org.ID, sandboxID, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
			conn := env.App.Service.Hub.Get(dev.ID)

			// Let the test process the create result after verifying command order.
			agent.SetOnCommand(func(a *testenv.FakeAgent, cmd *ownsandv1.AgentCommand) *ownsandv1.AgentCommandResult {
				if cmd.GetCreateSnapshot() != nil {
					return nil
				}
				return a.DefaultAnswer(cmd)
			})
			type createOutcome struct {
				response *connect.Response[ownsandv1.CreateSnapshotResponse]
				err      error
			}
			createDone := make(chan createOutcome, 1)
			go func() {
				res, err := client.Snapshot.CreateSnapshot(ctx, connect.NewRequest(&ownsandv1.CreateSnapshotRequest{
					OrganizationId: org.ID, SandboxId: sandboxID,
				}))
				createDone <- createOutcome{res, err}
			}()
			select {
			case <-gate.committed:
			case got := <-createDone:
				t.Fatalf("CreateSnapshot returned before the commit gate: %v", got.err)
			case <-ctx.Done():
				t.Fatal("CreateSnapshot did not reach the commit gate")
			}
			list, err := client.Snapshot.ListSnapshots(ctx, connect.NewRequest(&ownsandv1.ListSnapshotsRequest{OrganizationId: org.ID}))
			if err != nil {
				t.Fatal(err)
			}
			snapshots := list.Msg.GetSnapshots()
			if len(snapshots) != 1 || snapshots[0].GetState() != ownsandv1.SnapshotState_SNAPSHOT_STATE_CREATING {
				t.Fatalf("ListSnapshots during create = %v, want one CREATING snapshot", snapshots)
			}
			snapID := snapshots[0].GetSnapshotId()
			deleteSnapshotRPC(t, org, snapID)
			firstDelete := agent.WaitFor("initial snapshot delete", func(e *ownsandv1.AgentEnvelope) bool {
				return e.GetCommand().GetDeleteSnapshot().GetSnapshotId() == snapID
			}).GetCommand()
			testenv.Eventually(t, "initial delete acknowledged", func() bool { return env.App.Service.PendingCommands() == 0 })
			if agent.HasSnapshot(snapID) || createSnapshotCommands(agent) != 0 {
				t.Fatal("the agent received the create before it acknowledged the first delete")
			}

			resume()
			select {
			case got := <-createDone:
				if got.err != nil || got.response.Msg.GetSnapshot().GetSnapshotId() != snapID {
					t.Fatalf("CreateSnapshot response = %v, %v, want snapshot %s", got.response, got.err, snapID)
				}
			case <-ctx.Done():
				t.Fatal("CreateSnapshot did not return after releasing the commit gate")
			}
			create := agent.WaitFor("late snapshot create", func(e *ownsandv1.AgentEnvelope) bool {
				return e.GetCommand().GetCreateSnapshot().GetSnapshotId() == snapID
			}).GetCommand()
			var result *ownsandv1.AgentCommandResult
			if tc.failCreate {
				result = &ownsandv1.AgentCommandResult{Error: &ownsandv1.ErrorDetail{Code: ownsandv1.ErrorCode_ERROR_CODE_INTERNAL}}
			} else {
				result = agent.DefaultAnswer(create)
				if !agent.HasSnapshot(snapID) {
					t.Fatal("the late create did not store a local copy")
				}
			}
			result.CommandId = create.GetCommandId()
			agent.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_CommandResult{CommandResult: result}})

			if !tc.failCreate {
				agent.WaitFor("second snapshot delete on the same channel", func(e *ownsandv1.AgentEnvelope) bool {
					cmd := e.GetCommand()
					return cmd.GetCommandId() != firstDelete.GetCommandId() && cmd.GetDeleteSnapshot().GetSnapshotId() == snapID
				})
				agent.Eventually("late copy removed", func() bool { return !agent.HasSnapshot(snapID) })
			}
			// The report acknowledgement follows any command sent by the result handler.
			seq := agent.SendStateReport()
			agent.WaitFor("state report acknowledgement", func(e *ownsandv1.AgentEnvelope) bool {
				return e.GetStateAck().GetStateReport().GetReportSequence() == seq
			})
			wantDeletes := 2
			if tc.failCreate {
				wantDeletes = 1
			}
			if got := deleteSnapshotCommands(agent, snapID); got != wantDeletes {
				t.Fatalf("delete commands = %d, want %d", got, wantDeletes)
			}
			if got := env.App.Service.Hub.Live(dev.ID); got == nil || got != conn {
				t.Fatal("the agent channel changed")
			}
			_, err = client.Snapshot.GetSnapshot(ctx, connect.NewRequest(&ownsandv1.GetSnapshotRequest{OrganizationId: org.ID, SnapshotId: snapID}))
			if connect.CodeOf(err) != connect.CodeNotFound {
				t.Fatalf("GetSnapshot = %v, want not_found", err)
			}
		})
	}
}

func TestSnapshotResultForExistingSnapshotKeepsCopy(t *testing.T) {
	for _, tc := range []struct {
		name            string
		settledByReport bool
	}{
		{name: "result repeated"},
		{name: "state report already settled the row", settledByReport: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, org, sandboxID, agent, _ := snapshotResultEnv(t, 10*time.Second, 20*time.Millisecond)
			agent.SetOnCommand(func(a *testenv.FakeAgent, cmd *ownsandv1.AgentCommand) *ownsandv1.AgentCommandResult {
				if cmd.GetCreateSnapshot() != nil {
					return nil
				}
				return a.DefaultAnswer(cmd)
			})
			snapID := startSnapshot(t, org, sandboxID)
			create := agent.WaitFor("snapshot create", func(e *ownsandv1.AgentEnvelope) bool {
				return e.GetCommand().GetCreateSnapshot().GetSnapshotId() == snapID
			}).GetCommand()
			result := agent.DefaultAnswer(create)
			result.CommandId = create.GetCommandId()
			if tc.settledByReport {
				agent.SendStateReport()
				waitSnapshotState(t, org, snapID, ownsandv1.SnapshotState_SNAPSHOT_STATE_READY)
			}
			envelope := &ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_CommandResult{CommandResult: result}}
			agent.Send(envelope)
			waitSnapshotState(t, org, snapID, ownsandv1.SnapshotState_SNAPSHOT_STATE_READY)
			agent.Send(envelope)
			seq := agent.SendStateReport()
			agent.WaitFor("state report acknowledgement", func(e *ownsandv1.AgentEnvelope) bool {
				return e.GetStateAck().GetStateReport().GetReportSequence() == seq
			})
			if got := deleteSnapshotCommands(agent, snapID); got != 0 || !agent.HasSnapshot(snapID) {
				t.Fatalf("delete commands = %d, local copy = %t, want 0 and true", got, agent.HasSnapshot(snapID))
			}
		})
	}
}
