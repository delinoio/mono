// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/api"
	"github.com/delinoio/mono/servers/ownsand-api/internal/app"
	"github.com/delinoio/mono/servers/ownsand-api/internal/hub"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// updateFault makes the UPDATE (or, for newInsertFault, the INSERT) of one
// table that matches a WHEN predicate fail with a chosen SQLSTATE for its
// first calls. It injects the failure in the database itself, so the server code under test has no test seam. The
// trigger counts the matching calls in a sequence, which survives the
// rollback of the failed statement.
type updateFault struct {
	t    *testing.T
	env  *testenv.Env
	name string
}

// newUpdateFault installs the trigger on table for the updates that satisfy
// when (a predicate over OLD and NEW). The name must be unique per test
// database and a valid identifier fragment.
func newUpdateFault(t *testing.T, env *testenv.Env, name, table, when string) *updateFault {
	t.Helper()
	return newTableFault(t, env, name, table, "UPDATE", when)
}

// newInsertFault installs the trigger on table for the inserts that satisfy
// when (a predicate over NEW). The name must be unique per test database.
func newInsertFault(t *testing.T, env *testenv.Env, name, table, when string) *updateFault {
	t.Helper()
	return newTableFault(t, env, name, table, "INSERT", when)
}

func newTableFault(t *testing.T, env *testenv.Env, name, table, event, when string) *updateFault {
	t.Helper()
	for _, stmt := range []string{
		fmt.Sprintf("CREATE TABLE %s_state (code text, max_failures int)", name),
		fmt.Sprintf("CREATE SEQUENCE %s_calls", name),
		fmt.Sprintf(`CREATE FUNCTION %s_raise() RETURNS trigger LANGUAGE plpgsql AS $$
			DECLARE c text; lim int; n bigint;
			BEGIN
				n := nextval('%s_calls');
				SELECT code, max_failures INTO c, lim FROM %s_state LIMIT 1;
				IF c IS NOT NULL AND n <= lim THEN
					RAISE EXCEPTION 'injected fault' USING ERRCODE = c;
				END IF;
				RETURN NEW;
			END $$`, name, name, name),
		fmt.Sprintf("CREATE TRIGGER %s_fault BEFORE %s ON %s FOR EACH ROW WHEN (%s) EXECUTE FUNCTION %s_raise()", name, event, table, when, name),
	} {
		if _, err := env.DB.Pool.Exec(context.Background(), stmt); err != nil {
			t.Fatal(err)
		}
	}
	return &updateFault{t: t, env: env, name: name}
}

// on makes the first max matching calls fail with the SQLSTATE code.
func (f *updateFault) on(code string, max int) {
	f.t.Helper()
	f.off()
	if _, err := f.env.DB.Pool.Exec(context.Background(), fmt.Sprintf("INSERT INTO %s_state (code, max_failures) VALUES ($1, $2)", f.name), code, max); err != nil {
		f.t.Fatal(err)
	}
}

func (f *updateFault) off() {
	f.t.Helper()
	if _, err := f.env.DB.Pool.Exec(context.Background(), fmt.Sprintf("DELETE FROM %s_state", f.name)); err != nil {
		f.t.Fatal(err)
	}
}

// calls returns how many matching updates reached the trigger.
func (f *updateFault) calls() int {
	f.t.Helper()
	var n int
	if err := f.env.DB.Pool.QueryRow(context.Background(), fmt.Sprintf("SELECT CASE WHEN is_called THEN last_value ELSE 0 END FROM %s_calls", f.name)).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

// sandboxFailFault faults the transition that ends a CREATING sandbox as
// CREATE_FAILED. The state report of a replacement channel ends it as
// LOST_ON_DEVICE, which the predicate does not match.
func sandboxFailFault(t *testing.T, env *testenv.Env) *updateFault {
	return newUpdateFault(t, env, "create_fail", "sandboxes",
		fmt.Sprintf("OLD.agent_state = %d AND NEW.agent_state = %d AND NEW.end_reason = %d",
			ownsandv1.SandboxState_SANDBOX_STATE_CREATING, ownsandv1.SandboxState_SANDBOX_STATE_FAILED, ownsandv1.SandboxEndReason_SANDBOX_END_REASON_CREATE_FAILED))
}

// snapshotFailFault faults the transition CREATING to FAILED of a snapshot.
func snapshotFailFault(t *testing.T, env *testenv.Env) *updateFault {
	return newUpdateFault(t, env, "snapshot_fail", "snapshots",
		fmt.Sprintf("OLD.state = %d AND NEW.state = %d", ownsandv1.SnapshotState_SNAPSHOT_STATE_CREATING, ownsandv1.SnapshotState_SNAPSHOT_STATE_FAILED))
}

// snapshotReadyFault faults the transition CREATING to READY of a snapshot.
func snapshotReadyFault(t *testing.T, env *testenv.Env) *updateFault {
	return newUpdateFault(t, env, "snapshot_ready", "snapshots",
		fmt.Sprintf("OLD.state = %d AND NEW.state = %d", ownsandv1.SnapshotState_SNAPSHOT_STATE_CREATING, ownsandv1.SnapshotState_SNAPSHOT_STATE_READY))
}

func createFailTiming(budget, backoff time.Duration) testenv.Option {
	return func(o *app.Options) {
		o.Timing.CreateFailTimeout = budget
		o.Timing.ExportApplyRetryBackoff = backoff
	}
}

// causeText returns the text of the close cause of conn.
func causeText(conn *hub.Conn) string {
	if cause := context.Cause(conn.Context()); cause != nil {
		return cause.Error()
	}
	return ""
}

// sandboxRowState reads the stored state and end reason of a sandbox, which
// the API shows differently while the device is offline.
func sandboxRowState(t *testing.T, env *testenv.Env, sandboxID string) (state, reason int16) {
	t.Helper()
	if err := env.DB.Pool.QueryRow(context.Background(), "SELECT agent_state, COALESCE(end_reason, 0) FROM sandboxes WHERE sandbox_id = $1", sandboxID).Scan(&state, &reason); err != nil {
		t.Fatal(err)
	}
	return state, reason
}

func requireCreatingRow(t *testing.T, env *testenv.Env, sandboxID string) {
	t.Helper()
	if state, _ := sandboxRowState(t, env, sandboxID); state != int16(ownsandv1.SandboxState_SANDBOX_STATE_CREATING) {
		t.Fatalf("stored sandbox state = %d, want CREATING", state)
	}
}

// requireLostOnReplacement reconnects the agent and checks that the state
// report of the replacement channel ends the stranded sandbox as
// LOST_ON_DEVICE.
func requireLostOnReplacement(t *testing.T, env *testenv.Env, org *testenv.Org, agent *testenv.FakeAgent, sandboxID string) {
	t.Helper()
	agent.ResumeReads()
	reconnect(agent)
	sb := waitState(t, org.OwnerClient, org.ID, sandboxID, ownsandv1.SandboxState_SANDBOX_STATE_FAILED)
	if sb.GetEndReason() != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_LOST_ON_DEVICE {
		t.Fatalf("end reason after the replacement = %v, want LOST_ON_DEVICE", sb.GetEndReason())
	}
}

// requireClaimNotReleased checks the outcome of a compensation that ended
// without success: the channel closed with the claim cause, the record is
// still CREATING, and the failure log carries the attempts.
func requireClaimNotReleased(t *testing.T, env *testenv.Env, conn *hub.Conn, failLog string, retried bool) {
	t.Helper()
	select {
	case <-conn.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the channel stayed open")
	}
	if cause := causeText(conn); !strings.Contains(cause, "claim of an unsent create") {
		t.Fatalf("close cause = %q", cause)
	}
	waitLog(t, env, `"msg":"`+failLog+`"`)
	logs := env.Logs.String()
	for _, want := range []string{`"channel_closed":true`, `"attempts":`} {
		if !strings.Contains(logs, want) {
			t.Fatalf("logs lack %s:\n%s", want, logs)
		}
	}
	if got := strings.Contains(logs, `"msg":"create_fail_transition_retry"`) || strings.Contains(logs, `"msg":"snapshot_create_fail_transition_retry"`); got != retried {
		t.Fatalf("retry logged = %v, want %v:\n%s", got, retried, logs)
	}
}

// failedCreateSend sets up a one-create device with a saturated channel and
// starts a CreateSandbox whose request is canceled after the placement
// committed, so the create command send fails before enqueue while the
// channel stays open. It returns once the RPC failed; the compensation of the
// handler may still run because it does not depend on the request context.
func failedCreateSend(t *testing.T, env *testenv.Env) (org *testenv.Org, sandboxID string, dev *testenv.Device, agent *testenv.FakeAgent, conn *hub.Conn) {
	t.Helper()
	env.App.Service.Hub.QueueFullWait = 30 * time.Second
	ctx := context.Background()
	org = env.NewOrg("alice")
	c := org.OwnerClient
	dev, agent = setupAgent(t, env, org, &ownsandv1.DeviceLimits{VcpuCount: 2, MemoryMib: 8192, DiskGib: 100, VcpuOvercommitPercent: 100})
	conn = env.App.Service.Hub.Get(dev.ID)
	if conn == nil {
		t.Fatal("the device has no channel")
	}
	saturate(t, env, agent)
	rpcCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	rpcDone := make(chan error, 1)
	go func() {
		_, err := c.Sandbox.CreateSandbox(rpcCtx, connect.NewRequest(createReq(org.ID)))
		rpcDone <- err
	}()
	testenv.Eventually(t, "sandbox record committed", func() bool {
		list, err := c.Sandbox.ListSandboxes(ctx, connect.NewRequest(&ownsandv1.ListSandboxesRequest{OrganizationId: org.ID, IncludeEnded: true}))
		return err == nil && len(list.Msg.GetSandboxes()) == 1
	})
	cancel()
	if err := <-rpcDone; err == nil {
		t.Fatal("CreateSandbox on a saturated channel succeeded")
	}
	list, err := c.Sandbox.ListSandboxes(ctx, connect.NewRequest(&ownsandv1.ListSandboxesRequest{OrganizationId: org.ID, IncludeEnded: true}))
	if err != nil || len(list.Msg.GetSandboxes()) != 1 {
		t.Fatalf("sandboxes = %v, %v", list, err)
	}
	return org, list.Msg.GetSandboxes()[0].GetSandboxId(), dev, agent, conn
}

// TestCreateSendFailRetriesTransientError checks that a transient database
// error of the transition that fails an unsent create is retried inside the
// budget: the sandbox ends FAILED with CREATE_FAILED, its reservation is
// released and the channel stays open.
func TestCreateSendFailRetriesTransientError(t *testing.T) {
	env := testenv.New(t, createFailTiming(10*time.Second, 20*time.Millisecond))
	fault := sandboxFailFault(t, env)
	fault.on("40001", 2) // serialization_failure for the first two calls
	org, sandboxID, dev, _, conn := failedCreateSend(t, env)

	assertImportCreateFailed(t, org, sandboxID, dev.ID)
	waitLog(t, env, `"msg":"create_send_failed"`)
	logs := env.Logs.String()
	for _, want := range []string{`"msg":"create_fail_transition_retry"`, `"attempts":3`} {
		if !strings.Contains(logs, want) {
			t.Fatalf("logs lack %s:\n%s", want, logs)
		}
	}
	if strings.Contains(logs, `"msg":"create_send_fail_transition_failed"`) {
		t.Fatal("the compensation was reported as failed")
	}
	if got := fault.calls(); got != 3 {
		t.Fatalf("update calls = %d, want 3 (two injected failures and the success)", got)
	}
	connStaysOpen(t, conn)
}

// TestCreateSendFailUnresolvedClosesChannel checks that a transition that a
// permanent error or the end of the budget leaves unresolved closes the
// channel while it is current, that the failure is logged with the attempts,
// and that the state report of the replacement channel ends the stranded
// sandbox as LOST_ON_DEVICE.
func TestCreateSendFailUnresolvedClosesChannel(t *testing.T) {
	for _, tc := range []struct {
		name    string
		code    string
		budget  time.Duration
		retried bool
	}{
		{name: "permanent error", code: "23514", budget: 10 * time.Second},
		{name: "budget ends", code: "40001", budget: 400 * time.Millisecond, retried: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := testenv.New(t, createFailTiming(tc.budget, 20*time.Millisecond))
			fault := sandboxFailFault(t, env)
			fault.on(tc.code, 1<<30)
			org, sandboxID, _, agent, conn := failedCreateSend(t, env)

			requireClaimNotReleased(t, env, conn, "create_send_fail_transition_failed", tc.retried)
			requireCreatingRow(t, env, sandboxID)
			fault.off()
			requireLostOnReplacement(t, env, org, agent, sandboxID)
		})
	}
}

// TestCreateSendFailSettledByReplacementKeepsChannel checks that a
// compensation that finds the sandbox settled by the replacement channel's
// state report ends without a failure and does not close the replacement.
func TestCreateSendFailSettledByReplacementKeepsChannel(t *testing.T) {
	// A long backoff leaves time to replace the channel before the next attempt.
	env := testenv.New(t, createFailTiming(10*time.Second, 600*time.Millisecond))
	fault := sandboxFailFault(t, env)
	fault.on("40001", 1<<30)
	org, sandboxID, _, agent, _ := failedCreateSend(t, env)
	waitLog(t, env, `"msg":"create_fail_transition_retry"`)

	requireLostOnReplacement(t, env, org, agent, sandboxID)
	replacement := env.App.Service.Hub.Get(agent.Device.ID)
	if replacement == nil {
		t.Fatal("no replacement channel")
	}
	waitLog(t, env, `"msg":"create_send_failed"`)
	if strings.Contains(env.Logs.String(), `"msg":"create_send_fail_transition_failed"`) {
		t.Fatal("a settled record was reported as failed")
	}
	connStaysOpen(t, replacement)
}

// rejectCreates makes the fake agent answer every create sandbox command with
// an error and keep a count of them.
func rejectCreates(agent *testenv.FakeAgent) *atomic.Int64 {
	var n atomic.Int64
	agent.SetOnCommand(func(fa *testenv.FakeAgent, cmd *ownsandv1.AgentCommand) *ownsandv1.AgentCommandResult {
		if cmd.GetCreateSandbox() != nil {
			n.Add(1)
			return &ownsandv1.AgentCommandResult{Error: &ownsandv1.ErrorDetail{Code: ownsandv1.ErrorCode_ERROR_CODE_UNIMPLEMENTED, Message: "rejected by the test"}}
		}
		return fa.DefaultAnswer(cmd)
	})
	return &n
}

// TestAgentRejectedCreateRetriesTransientError checks the same retry for the
// transition that fails a create that the agent rejected.
func TestAgentRejectedCreateRetriesTransientError(t *testing.T) {
	env := testenv.New(t, createFailTiming(10*time.Second, 20*time.Millisecond))
	fault := sandboxFailFault(t, env)
	fault.on("40001", 2)
	org := env.NewOrg("alice")
	dev, agent := setupAgent(t, env, org, nil)
	rejectCreates(agent)
	conn := env.App.Service.Hub.Get(dev.ID)

	created, err := org.OwnerClient.Sandbox.CreateSandbox(context.Background(), connect.NewRequest(createReq(org.ID)))
	if err != nil {
		t.Fatal(err)
	}
	assertImportCreateFailed(t, org, created.Msg.GetSandbox().GetSandboxId(), dev.ID)
	logs := env.Logs.String()
	if !strings.Contains(logs, `"msg":"create_fail_transition_retry"`) || strings.Contains(logs, `"msg":"create_fail_transition_failed"`) {
		t.Fatalf("unexpected logs:\n%s", logs)
	}
	if got := fault.calls(); got != 3 {
		t.Fatalf("update calls = %d, want 3", got)
	}
	connStaysOpen(t, conn)
}

// TestAgentRejectedCreateUnresolvedClosesChannel checks that a rejected
// create whose transition stays unresolved closes the channel and that the
// replacement channel ends the sandbox as LOST_ON_DEVICE.
func TestAgentRejectedCreateUnresolvedClosesChannel(t *testing.T) {
	for _, tc := range []struct {
		name    string
		code    string
		budget  time.Duration
		retried bool
	}{
		{name: "permanent error", code: "23514", budget: 10 * time.Second},
		{name: "budget ends", code: "40001", budget: 400 * time.Millisecond, retried: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := testenv.New(t, createFailTiming(tc.budget, 20*time.Millisecond))
			fault := sandboxFailFault(t, env)
			fault.on(tc.code, 1<<30)
			org := env.NewOrg("alice")
			dev, agent := setupAgent(t, env, org, nil)
			rejectCreates(agent)
			conn := env.App.Service.Hub.Get(dev.ID)

			created, err := org.OwnerClient.Sandbox.CreateSandbox(context.Background(), connect.NewRequest(createReq(org.ID)))
			if err != nil {
				t.Fatal(err)
			}
			sandboxID := created.Msg.GetSandbox().GetSandboxId()
			requireClaimNotReleased(t, env, conn, "create_fail_transition_failed", tc.retried)
			requireCreatingRow(t, env, sandboxID)
			fault.off()
			requireLostOnReplacement(t, env, org, agent, sandboxID)
		})
	}
}

func importCreateTiming(importWait, budget, backoff time.Duration) testenv.Option {
	timing := api.Timing{HeartbeatTimeout: 45 * time.Second, CommandWait: time.Second, EventPoll: 50 * time.Millisecond, OfflineSweep: time.Hour, CleanupPoll: time.Hour,
		ImportWait: importWait, CreateFailTimeout: budget, ExportApplyRetryBackoff: backoff}
	return func(o *app.Options) { o.Timing = &timing }
}

// rejectImports makes the fake agent answer an import command with an error.
func rejectImports(agent *testenv.FakeAgent) {
	agent.SetOnCommand(func(fa *testenv.FakeAgent, cmd *ownsandv1.AgentCommand) *ownsandv1.AgentCommandResult {
		if cmd.GetImportSnapshot() != nil {
			return &ownsandv1.AgentCommandResult{Error: &ownsandv1.ErrorDetail{Code: ownsandv1.ErrorCode_ERROR_CODE_UNIMPLEMENTED, Message: "rejected by the test"}}
		}
		return fa.DefaultAnswer(cmd)
	})
}

// TestImportFailureCompensationRetriesAndCloses checks the two ends of the
// import flow, an import that the agent rejected and an import wait that
// expired: a transient database error is retried until the budget ends, and
// a transition that stays unresolved closes the channel, so that the
// replacement channel's state report ends the sandbox as LOST_ON_DEVICE.
func TestImportFailureCompensationRetriesAndCloses(t *testing.T) {
	for _, tc := range []struct {
		name    string
		trigger func(agent *testenv.FakeAgent)
		wait    time.Duration
		failLog string
	}{
		{name: "import rejected", trigger: rejectImports, wait: 10 * time.Second, failLog: "import_rejected_fail_create_failed"},
		{name: "import wait expired", trigger: func(a *testenv.FakeAgent) { withholdImportResult(a) }, wait: 300 * time.Millisecond, failLog: "import_timeout_fail_create_failed"},
	} {
		t.Run(tc.name+"/transient error recovers", func(t *testing.T) {
			env := testenv.New(t, importCreateTiming(tc.wait, 10*time.Second, 20*time.Millisecond))
			org := env.NewOrg("alice")
			snapID, devB, agentB := importFixture(t, env, org)
			fault := sandboxFailFault(t, env)
			fault.on("40001", 2)
			tc.trigger(agentB)
			conn := env.App.Service.Hub.Get(devB.ID)

			created, err := org.OwnerClient.Sandbox.CreateSandbox(context.Background(), connect.NewRequest(importCreateReq(org.ID, snapID, devB.ID)))
			if err != nil {
				t.Fatal(err)
			}
			assertImportCreateFailed(t, org, created.Msg.GetSandbox().GetSandboxId(), devB.ID)
			if !strings.Contains(env.Logs.String(), `"msg":"create_fail_transition_retry"`) {
				t.Fatalf("no retry logged:\n%s", env.Logs.String())
			}
			if strings.Contains(env.Logs.String(), `"msg":"`+tc.failLog+`"`) {
				t.Fatal("the compensation was reported as failed")
			}
			if got := fault.calls(); got != 3 {
				t.Fatalf("update calls = %d, want 3", got)
			}
			connStaysOpen(t, conn)
		})
		t.Run(tc.name+"/unresolved closes channel", func(t *testing.T) {
			env := testenv.New(t, importCreateTiming(tc.wait, 400*time.Millisecond, 20*time.Millisecond))
			org := env.NewOrg("alice")
			snapID, devB, agentB := importFixture(t, env, org)
			fault := sandboxFailFault(t, env)
			fault.on("40001", 1<<30)
			tc.trigger(agentB)
			conn := env.App.Service.Hub.Get(devB.ID)

			created, err := org.OwnerClient.Sandbox.CreateSandbox(context.Background(), connect.NewRequest(importCreateReq(org.ID, snapID, devB.ID)))
			if err != nil {
				t.Fatal(err)
			}
			sandboxID := created.Msg.GetSandbox().GetSandboxId()
			requireClaimNotReleased(t, env, conn, tc.failLog, true)
			requireCreatingRow(t, env, sandboxID)
			fault.off()
			requireLostOnReplacement(t, env, org, agentB, sandboxID)
		})
	}
}

// TestCreateSnapshotSendFailCompensation checks the transition that fails an
// unsent CreateSnapshot: a transient error is retried and leaves FAILED with
// the channel open and the unavailable response; an unresolved transition
// closes the channel, and the state report of the replacement channel (which
// does not list the snapshot) fails it.
func TestCreateSnapshotSendFailCompensation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		code    string
		max     int
		budget  time.Duration
		resolve bool
	}{
		{name: "transient error recovers", code: "40001", max: 2, budget: 10 * time.Second, resolve: true},
		{name: "permanent error", code: "23514", max: 1 << 30, budget: 10 * time.Second},
		{name: "budget ends", code: "40001", max: 1 << 30, budget: 400 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := testenv.New(t, createFailTiming(tc.budget, 20*time.Millisecond))
			env.App.Service.Hub.QueueFullWait = saturationQueueFullWait
			ctx := context.Background()
			org := env.NewOrg("alice")
			c := org.OwnerClient
			dev, agent := setupAgent(t, env, org, nil)
			created, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(createReq(org.ID)))
			if err != nil {
				t.Fatal(err)
			}
			sandboxID := created.Msg.GetSandbox().GetSandboxId()
			waitState(t, c, org.ID, sandboxID, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
			conn := env.App.Service.Hub.Get(dev.ID)
			fault := snapshotFailFault(t, env)
			fault.on(tc.code, tc.max)

			saturate(t, env, agent)
			rpcCtx, cancel := context.WithTimeout(ctx, enqueueFailureDeadline)
			defer cancel()
			_, err = c.Snapshot.CreateSnapshot(rpcCtx, connect.NewRequest(&ownsandv1.CreateSnapshotRequest{OrganizationId: org.ID, SandboxId: sandboxID}))
			if err == nil {
				t.Fatal("CreateSnapshot on a saturated channel succeeded")
			}
			list, err := c.Snapshot.ListSnapshots(ctx, connect.NewRequest(&ownsandv1.ListSnapshotsRequest{OrganizationId: org.ID}))
			if err != nil || len(list.Msg.GetSnapshots()) != 1 {
				t.Fatalf("snapshots = %v, %v", list, err)
			}
			snapID := list.Msg.GetSnapshots()[0].GetSnapshotId()

			if tc.resolve {
				testenv.Eventually(t, "snapshot FAILED", func() bool {
					return snapshotStateOf(t, c, org.ID, snapID) == ownsandv1.SnapshotState_SNAPSHOT_STATE_FAILED
				})
				waitLog(t, env, `"msg":"snapshot_create_send_failed"`)
				logs := env.Logs.String()
				for _, want := range []string{`"msg":"snapshot_create_fail_transition_retry"`, `"attempts":3`} {
					if !strings.Contains(logs, want) {
						t.Fatalf("logs lack %s:\n%s", want, logs)
					}
				}
				connStaysOpen(t, conn)
				return
			}
			requireClaimNotReleased(t, env, conn, "snapshot_create_fail_transition_failed", tc.name == "budget ends")
			if got := snapshotStateOf(t, c, org.ID, snapID); got != ownsandv1.SnapshotState_SNAPSHOT_STATE_CREATING {
				t.Fatalf("state while the compensation failed = %v", got)
			}
			fault.off()
			agent.ResumeReads()
			reconnect(agent)
			testenv.Eventually(t, "snapshot FAILED by the replacement report", func() bool {
				return snapshotStateOf(t, c, org.ID, snapID) == ownsandv1.SnapshotState_SNAPSHOT_STATE_FAILED
			})
		})
	}
}
