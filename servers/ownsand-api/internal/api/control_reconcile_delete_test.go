// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/api"
	"github.com/delinoio/mono/servers/ownsand-api/internal/app"
	"github.com/delinoio/mono/servers/ownsand-api/internal/events"
	"github.com/delinoio/mono/servers/ownsand-api/internal/hub"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

func TestDeleteEventsDuringInitialReconciliation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		snapshot bool
		replace  bool
		retry    bool
	}{
		{name: "sandbox"},
		{name: "sandbox_with_lookup_retry", retry: true},
		{name: "snapshot", snapshot: true},
		{name: "closed_channel", snapshot: true, replace: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			env := testenv.New(t, func(o *app.Options) {
				o.Timing.CommandWait = 200 * time.Millisecond
			})
			health, err := env.Server.Client().Get(env.Server.URL + "/healthz")
			if err != nil {
				t.Fatal(err)
			}
			_ = health.Body.Close()
			if health.StatusCode != http.StatusOK {
				t.Fatalf("loopback health check returned %s", health.Status)
			}
			org := env.NewOrg("alice")
			client := org.OwnerClient
			dev, before := setupAgent(t, env, org, nil)
			sandboxID := createRunningSandboxOnDevice(t, org, dev)
			otherDev, other := setupAgent(t, env, org, nil)
			otherID := createRunningSandboxOnDevice(t, org, otherDev)
			var laterID string
			if tc.retry {
				laterID = createRunningSandboxOnDevice(t, org, otherDev)
			}

			var snapshotID string
			if tc.snapshot {
				res, err := client.Snapshot.CreateSnapshot(ctx, connect.NewRequest(&ownsandv1.CreateSnapshotRequest{
					OrganizationId: org.ID, SandboxId: sandboxID,
				}))
				if err != nil {
					t.Fatal(err)
				}
				snapshotID = res.Msg.GetSnapshot().GetSnapshotId()
				testenv.Eventually(t, "a READY local snapshot", func() bool {
					res, err := client.Snapshot.GetSnapshot(ctx, connect.NewRequest(&ownsandv1.GetSnapshotRequest{
						OrganizationId: org.ID, SnapshotId: snapshotID,
					}))
					return err == nil && res.Msg.GetSnapshot().GetState() == ownsandv1.SnapshotState_SNAPSHOT_STATE_READY && before.HasSnapshot(snapshotID)
				})
			}
			newAgent := func() *testenv.FakeAgent {
				agent := env.NewFakeAgent(dev)
				agent.SetSandbox(sandboxID, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING, 0)
				if tc.snapshot {
					agent.AddSnapshot(&ownsandv1.SnapshotStateEntry{
						SnapshotId: snapshotID, State: ownsandv1.SnapshotState_SNAPSHOT_STATE_READY, GuestOs: ownsandv1.GuestOs_GUEST_OS_LINUX,
					})
				}
				return agent
			}
			before.Close()

			control := env.App.Control
			if _, ok := control.ProcessedCursor(); !ok {
				t.Fatal("the lease holder has no event baseline")
			}
			var allowLookup atomic.Bool
			var lookups atomic.Int32
			if tc.retry {
				var original func(context.Context, string, string) (*api.SandboxRowForTest, error)
				original = control.SetLoadDeleteTargetForTest(func(ctx context.Context, orgID, id string) (*api.SandboxRowForTest, error) {
					if id == sandboxID {
						lookups.Add(1)
						if !allowLookup.Load() {
							return nil, errors.New("injected deferred lookup failure")
						}
					}
					return original(ctx, orgID, id)
				})
			}

			atBarrier := make(chan *hub.Conn, 1)
			release := make(chan struct{})
			env.App.Service.SetAfterInitialReportCommitForTest(func(conn *hub.Conn) {
				if conn.DeviceID == dev.ID {
					atBarrier <- conn
					select {
					case <-release:
					case <-conn.Done():
					}
				}
			})
			agent := newAgent()
			agent.OpenChannel()
			var held *hub.Conn
			select {
			case held = <-atBarrier:
			case <-time.After(5 * time.Second):
				t.Fatal("the report did not commit")
			}
			if env.App.Service.Hub.Get(dev.ID) != held || held.Reconciled() {
				t.Fatal("the held stream must be current and unreconciled")
			}
			t.Log("the TLS agent channel reached the barrier after the first report committed")

			matchDelete := func(e *ownsandv1.AgentEnvelope) bool {
				if tc.snapshot {
					return e.GetCommand().GetDeleteSnapshot().GetSnapshotId() == snapshotID
				}
				return e.GetCommand().GetDeleteSandbox().GetSandboxId() == sandboxID
			}
			if tc.snapshot {
				if _, err := client.Snapshot.DeleteSnapshot(ctx, connect.NewRequest(&ownsandv1.DeleteSnapshotRequest{
					OrganizationId: org.ID, SnapshotId: snapshotID,
				})); err != nil {
					t.Fatal(err)
				}
			} else if state, err := callDeleteSandbox(ctx, client, org.ID, sandboxID); err != nil || state.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_DELETING {
				t.Fatalf("DeleteSandbox while the channel was held returned %v, %v", state, err)
			}
			committed, err := events.LatestID(ctx, env.DB.Pool)
			if err != nil {
				t.Fatal(err)
			}
			testenv.Eventually(t, "control cursor to pass the committed delete", func() bool {
				id, ok := control.ProcessedCursor()
				return ok && id >= committed
			})
			for _, e := range agent.Received() {
				if e.GetStateAck().GetStateReport() != nil || matchDelete(e) {
					t.Fatalf("received acknowledgement or delete before releasing the report: %v", e)
				}
			}

			deleteOnReadyDevice(t, org, other, otherID)
			t.Log("the ready device received its delete while the first device was held")

			if tc.replace {
				agent.Close()
				testenv.Eventually(t, "held channel retired", func() bool {
					return env.App.Service.Hub.Get(dev.ID) != held
				})
				env.App.Service.SetAfterInitialReportCommitForTest(nil)
				close(release)
				agent = newAgent()
				agent.Connect()
			} else {
				close(release)
				if tc.retry {
					testenv.Eventually(t, "worker to retry the deferred lookup", func() bool {
						return lookups.Load() >= 2 && strings.Contains(env.Logs.String(), "control_deferred_delete_retry")
					})
					deleteOnReadyDevice(t, org, other, laterID)
					allowLookup.Store(true)
					t.Log("a failing deferred lookup retried while a later delete reached the ready device")
				}
			}
			agent.WaitFor("delete command on the reconciled channel", matchDelete)
			if !tc.replace && env.App.Service.Hub.Get(dev.ID) != held {
				t.Fatal("the delete was not delivered on the channel that applied the report")
			}
			if tc.snapshot {
				agent.Eventually("local snapshot copy removed", func() bool { return !agent.HasSnapshot(snapshotID) })
			} else if sb := waitState(t, client, org.ID, sandboxID, ownsandv1.SandboxState_SANDBOX_STATE_DELETED); sb.GetEndReason() != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DELETED_BY_CALLER {
				t.Fatalf("sandbox delete end reason = %v", sb.GetEndReason())
			}
			agent.Heartbeat(101)
			agent.WaitFor("heartbeat acknowledgement", func(e *ownsandv1.AgentEnvelope) bool {
				return e.GetHeartbeatAck().GetSequence() == 101
			})
			var acked bool
			var deletes int
			for _, e := range agent.Received() {
				acked = acked || e.GetStateAck().GetStateReport() != nil
				if matchDelete(e) {
					if !acked {
						t.Fatal("the delete arrived before the first report acknowledgement")
					}
					deletes++
				}
			}
			if deletes != 1 {
				t.Fatalf("received %d matching delete commands, want one", deletes)
			}
			t.Log("the report acknowledgement preceded one delete command, and the deletion completed")
		})
	}
}

func createRunningSandboxOnDevice(t *testing.T, org *testenv.Org, dev *testenv.Device) string {
	t.Helper()
	req := createReq(org.ID)
	req.Placement = &ownsandv1.Placement{DeviceId: &dev.ID}
	res, err := org.OwnerClient.Sandbox.CreateSandbox(t.Context(), connect.NewRequest(req))
	if err != nil {
		t.Fatal(err)
	}
	id := res.Msg.GetSandbox().GetSandboxId()
	waitState(t, org.OwnerClient, org.ID, id, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	return id
}

func deleteOnReadyDevice(t *testing.T, org *testenv.Org, agent *testenv.FakeAgent, id string) {
	t.Helper()
	if _, err := callDeleteSandbox(t.Context(), org.OwnerClient, org.ID, id); err != nil {
		t.Fatal(err)
	}
	agent.WaitFor("delete on the other ready device", func(e *ownsandv1.AgentEnvelope) bool {
		return e.GetCommand().GetDeleteSandbox().GetSandboxId() == id
	})
}
