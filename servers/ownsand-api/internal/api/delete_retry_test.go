// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/app"
	"github.com/delinoio/mono/servers/ownsand-api/internal/events"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

func setupDeleteRetry(t *testing.T) (*testenv.Env, *testenv.Org, *testenv.Device, *testenv.FakeAgent, string) {
	t.Helper()
	timing := commandExpiryTiming()
	timing.CommandWait = 2 * time.Second
	env := testenv.New(t, func(o *app.Options) { o.Timing = timing })
	org := env.NewOrg("alice")
	device, agent := setupAgent(t, env, org, nil)
	created, err := org.OwnerClient.Sandbox.CreateSandbox(t.Context(), connect.NewRequest(createReq(org.ID)))
	if err != nil {
		t.Fatal(err)
	}
	id := created.Msg.GetSandbox().GetSandboxId()
	waitState(t, org.OwnerClient, org.ID, id, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	agent.SendUpdate(agent.Sandbox(id))
	agent.WaitFor("the running revision acknowledgement", func(e *ownsandv1.AgentEnvelope) bool {
		return e.GetStateAck().GetSandboxUpdate().GetSandboxId() == id
	})
	return env, org, device, agent, id
}

func callDeleteSandbox(ctx context.Context, client *testenv.Clients, orgID, sandboxID string) (*ownsandv1.Sandbox, error) {
	res, err := client.Sandbox.DeleteSandbox(ctx, connect.NewRequest(&ownsandv1.DeleteSandboxRequest{
		OrganizationId: orgID, SandboxId: sandboxID,
	}))
	if err != nil {
		return nil, err
	}
	return res.Msg.GetSandbox(), nil
}

func startDeleteSandbox(t *testing.T, client *testenv.Clients, orgID, sandboxID string) func(ownsandv1.SandboxState) {
	t.Helper()
	type outcome struct {
		sandbox *ownsandv1.Sandbox
		err     error
	}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	t.Cleanup(cancel)
	done := make(chan outcome, 1)
	go func() {
		sb, err := callDeleteSandbox(ctx, client, orgID, sandboxID)
		done <- outcome{sb, err}
	}()
	return func(want ownsandv1.SandboxState) {
		t.Helper()
		defer cancel()
		select {
		case got := <-done:
			if got.err != nil || got.sandbox.GetState() != want {
				t.Fatalf("DeleteSandbox = %v, %v, want %v", got.sandbox, got.err, want)
			}
		case <-ctx.Done():
			t.Fatalf("DeleteSandbox did not finish: %v", ctx.Err())
		}
	}
}

func deleteCommands(agent *testenv.FakeAgent, sandboxID string) []*ownsandv1.AgentCommand {
	var commands []*ownsandv1.AgentCommand
	for _, e := range agent.Received() {
		if cmd := e.GetCommand(); cmd.GetDeleteSandbox().GetSandboxId() == sandboxID {
			commands = append(commands, cmd)
		}
	}
	return commands
}

func waitForDeleteEvents(t *testing.T, env *testenv.Env, sandboxID string, want int) {
	t.Helper()
	testenv.Eventually(t, "the lease holder processed the delete requests", func() bool {
		var count int
		var last int64
		err := env.DB.Pool.QueryRow(t.Context(), `SELECT count(*), COALESCE(max(delivery_id), 0)
			FROM control_events WHERE sandbox_id = $1 AND kind = $2`, sandboxID, int16(events.KindSandboxDeleteRequested)).Scan(&count, &last)
		if err != nil {
			t.Fatal(err)
		}
		cursor, ok := env.App.Control.ProcessedCursor()
		return count == want && ok && cursor >= last
	})
}

func sendDeleteResult(agent *testenv.FakeAgent, cmd *ownsandv1.AgentCommand, res *ownsandv1.AgentCommandResult) {
	res.CommandId = cmd.GetCommandId()
	agent.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_CommandResult{CommandResult: res}})
}

func deleteInternalError() *ownsandv1.AgentCommandResult {
	return &ownsandv1.AgentCommandResult{
		Error: &ownsandv1.ErrorDetail{Code: ownsandv1.ErrorCode_ERROR_CODE_INTERNAL},
	}
}

func TestDeleteSandboxRetryAfterAgentError(t *testing.T) {
	t.Parallel()
	env, org, _, agent, id := setupDeleteRetry(t)
	deleting := ownsandv1.SandboxState_SANDBOX_STATE_DELETING
	deleted := ownsandv1.SandboxState_SANDBOX_STATE_DELETED
	reason := ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DELETED_BY_CALLER

	agent.SetOnCommand(func(*testenv.FakeAgent, *ownsandv1.AgentCommand) *ownsandv1.AgentCommandResult {
		return deleteInternalError()
	})
	startDeleteSandbox(t, org.OwnerClient, org.ID, id)(deleting)
	waitForDeleteEvents(t, env, id, 1)
	first := deleteCommands(agent, id)
	if len(first) != 1 || env.App.Service.PendingCommands() != 0 {
		t.Fatalf("after the agent error: %d delete commands, %d pending commands", len(first), env.App.Service.PendingCommands())
	}

	withholdAll(agent)
	member := env.User(org.Member)
	retry := startDeleteSandbox(t, member, org.ID, id)
	var second *ownsandv1.AgentCommand
	testenv.Eventually(t, "an explicit retry dispatched a second delete on the same channel", func() bool {
		cmds := deleteCommands(agent, id)
		if len(cmds) < 2 {
			return false
		}
		second = cmds[1]
		return true
	})
	if second.GetCommandId() == first[0].GetCommandId() || second.GetDeleteSandbox().GetEndReason() != reason {
		t.Fatalf("retry command = %v", second)
	}

	concurrent := startDeleteSandbox(t, env.User(org.Admin), org.ID, id)
	waitForDeleteEvents(t, env, id, 3)
	agent.Heartbeat(101)
	agent.WaitFor("heartbeat acknowledgement after delete events", func(e *ownsandv1.AgentEnvelope) bool {
		return e.GetHeartbeatAck().GetSequence() == 101
	})
	if got := len(deleteCommands(agent, id)); got != 2 {
		t.Fatalf("delete commands while the retry is outstanding = %d, want 2 total", got)
	}

	var pending bool
	var storedReason, actorKind int16
	var actor string
	err := env.DB.Pool.QueryRow(t.Context(), "SELECT delete_pending, delete_end_reason, delete_actor_kind, delete_actor_id FROM sandboxes WHERE sandbox_id = $1", id).
		Scan(&pending, &storedReason, &actorKind, &actor)
	if err != nil || !pending || storedReason != int16(reason) ||
		actorKind != int16(ownsandv1.PrincipalKind_PRINCIPAL_KIND_USER) || actor != org.Owner {
		t.Fatalf("delete intent = %v, %v, %v, %q, %v", pending, storedReason, actorKind, actor, err)
	}

	sendDeleteResult(agent, second, agent.DefaultAnswer(second))
	retry(deleted)
	concurrent(deleted)
	if err := env.DB.Pool.QueryRow(t.Context(), "SELECT delete_pending FROM sandboxes WHERE sandbox_id = $1", id).Scan(&pending); err != nil || pending {
		t.Fatalf("delete_pending after terminal result = %v, %v", pending, err)
	}

	startDeleteSandbox(t, member, org.ID, id)(deleted)
	var eventCount int
	if err := env.DB.Pool.QueryRow(t.Context(), "SELECT count(*) FROM control_events WHERE sandbox_id = $1 AND kind = $2",
		id, int16(events.KindSandboxDeleteRequested)).Scan(&eventCount); err != nil || eventCount != 3 {
		t.Fatalf("delete events after terminal retry = %d, %v", eventCount, err)
	}
	if got := len(deleteCommands(agent, id)); got != 2 {
		t.Fatalf("delete commands after terminal retry = %d, want 2 total", got)
	}
}

func TestDeleteSandboxRetryKeepsReplacementCommand(t *testing.T) {
	t.Parallel()
	env, org, device, original, id := setupDeleteRetry(t)
	running := ownsandv1.SandboxState_SANDBOX_STATE_RUNNING
	deleting := ownsandv1.SandboxState_SANDBOX_STATE_DELETING
	deleted := ownsandv1.SandboxState_SANDBOX_STATE_DELETED

	withholdAll(original)
	startDeleteSandbox(t, org.OwnerClient, org.ID, id)(deleting)
	old := original.WaitFor("original delete", func(e *ownsandv1.AgentEnvelope) bool {
		return e.GetCommand().GetDeleteSandbox().GetSandboxId() == id
	}).GetCommand()

	replacement := env.NewFakeAgent(device)
	replacement.SetSandbox(id, running, 0)
	withholdAll(replacement)
	replacement.Connect()
	current := replacement.WaitFor("reconciled delete on the replacement channel", func(e *ownsandv1.AgentEnvelope) bool {
		return e.GetCommand().GetDeleteSandbox().GetSandboxId() == id
	}).GetCommand()
	if current.GetCommandId() == old.GetCommandId() {
		t.Fatal("the replacement reused the original command ID")
	}

	// The agent can report a result from its original channel on its replacement.
	sendDeleteResult(replacement, old, deleteInternalError())
	testenv.Eventually(t, "the old result left the replacement command pending", func() bool {
		return env.App.Service.PendingCommands() == 1
	})
	retry := startDeleteSandbox(t, org.OwnerClient, org.ID, id)
	waitForDeleteEvents(t, env, id, 2)
	replacement.Heartbeat(102)
	replacement.WaitFor("heartbeat acknowledgement after the retry event", func(e *ownsandv1.AgentEnvelope) bool {
		return e.GetHeartbeatAck().GetSequence() == 102
	})
	if got := len(deleteCommands(replacement, id)); got != 1 {
		t.Fatalf("replacement delete commands after the old result and retry = %d, want 1", got)
	}
	sendDeleteResult(replacement, current, replacement.DefaultAnswer(current))
	retry(deleted)
}

func TestIncrementalDeletingProgressDoesNotRedispatch(t *testing.T) {
	env, org, device, agent, id := setupDeleteRetry(t)
	running := ownsandv1.SandboxState_SANDBOX_STATE_RUNNING
	deleting := ownsandv1.SandboxState_SANDBOX_STATE_DELETING
	deleted := ownsandv1.SandboxState_SANDBOX_STATE_DELETED
	reason := ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DELETED_BY_CALLER

	waitDelete := func(a *testenv.FakeAgent, sandboxID string, previous *ownsandv1.AgentCommand) *ownsandv1.AgentCommand {
		t.Helper()
		cmd := a.WaitFor("delete command", func(e *ownsandv1.AgentEnvelope) bool {
			cmd := e.GetCommand()
			return cmd.GetDeleteSandbox().GetSandboxId() == sandboxID && cmd.GetCommandId() != previous.GetCommandId()
		}).GetCommand()
		if got := cmd.GetDeleteSandbox().GetEndReason(); got != reason {
			t.Fatalf("delete reason = %v, want %v", got, reason)
		}
		return cmd
	}
	flush := func(a *testenv.FakeAgent, sequence uint64) {
		t.Helper()
		a.Heartbeat(sequence)
		a.WaitFor("heartbeat acknowledgement", func(e *ownsandv1.AgentEnvelope) bool {
			return e.GetHeartbeatAck().GetSequence() == sequence
		})
	}
	checkAllocation := func(want int32) {
		t.Helper()
		dev, err := org.OwnerClient.Device.GetDevice(t.Context(), connect.NewRequest(&ownsandv1.GetDeviceRequest{
			OrganizationId: org.ID, DeviceId: device.ID,
		}))
		if err != nil {
			t.Fatal(err)
		}
		if got := dev.Msg.GetDevice().GetAllocation().GetVcpuCount(); got != want {
			t.Fatalf("allocated vCPUs = %d, want %d", got, want)
		}
	}

	withholdAll(agent)
	wait := startDeleteSandbox(t, org.OwnerClient, org.ID, id)
	first := waitDelete(agent, id, nil)
	sendDeleteResult(agent, first, deleteInternalError())
	flush(agent, 101)
	if got := env.App.Service.PendingCommands(); got != 0 {
		t.Fatalf("pending commands after initial delete error = %d, want 0", got)
	}
	wait(deleting)

	progress := agent.SetSandbox(id, deleting, 0)
	rev := progress.GetRevision()
	idle := progress.GetIdleDeadline().AsTime().Truncate(time.Microsecond)
	for i := range 2 {
		if i == 1 {
			// The repeated revision must leave the stored deadline intact.
			progress.IdleDeadline = timestamppb.New(idle.Add(time.Minute))
		}
		agent.SendUpdate(progress)
		flush(agent, 102+uint64(i))
		var acks int
		for _, e := range agent.Received() {
			ack := e.GetStateAck().GetSandboxUpdate()
			if ack.GetSandboxId() == id && ack.GetRevision().GetEpoch() == rev.GetEpoch() && ack.GetRevision().GetCounter() == rev.GetCounter() {
				acks++
			}
		}
		if acks != i+1 || len(deleteCommands(agent, id)) != 1 {
			t.Fatalf("progress update %d: %d matching acknowledgements, %d total delete commands, want %d and 1",
				i+1, acks, len(deleteCommands(agent, id)), i+1)
		}

		var state, storedReason, actorKind int16
		var counter uint64
		var pending bool
		var epoch, actor string
		var deadline time.Time
		err := env.DB.Pool.QueryRow(t.Context(), `SELECT agent_state, revision_epoch, revision_counter, idle_deadline,
			delete_pending, delete_end_reason, delete_actor_kind, delete_actor_id FROM sandboxes WHERE sandbox_id = $1`, id).
			Scan(&state, &epoch, &counter, &deadline, &pending, &storedReason, &actorKind, &actor)
		if err != nil {
			t.Fatal(err)
		}
		if state != int16(deleting) || epoch != rev.GetEpoch() || counter != rev.GetCounter() {
			t.Fatalf("progress state and revision = %d, %s/%d, want %v and %v", state, epoch, counter, deleting, rev)
		}
		if !deadline.Equal(idle) {
			t.Fatalf("stored idle deadline = %v, want %v", deadline, idle)
		}
		if !pending || storedReason != int16(reason) || actorKind != int16(ownsandv1.PrincipalKind_PRINCIPAL_KIND_USER) || actor != org.Owner {
			t.Fatalf("delete intent = %v, %d, %d, %q", pending, storedReason, actorKind, actor)
		}
		checkAllocation(2)
	}

	agent.SendUpdate(agent.SetSandbox(id, running, 0))
	second := waitDelete(agent, id, first)
	sendDeleteResult(agent, second, deleteInternalError())
	flush(agent, 104)

	replacement := env.NewFakeAgent(device)
	replacement.SetSandbox(id, deleting, 0)
	withholdAll(replacement)
	replacement.Connect()
	third := waitDelete(replacement, id, second)
	sendDeleteResult(replacement, third, deleteInternalError())
	flush(replacement, 105)

	// Keep the same epoch and continue past the original agent's revision.
	terminal := agent.SetSandbox(id, deleted, 0)
	replacement.SendUpdate(terminal)
	flush(replacement, 106)
	if got := getSandbox(t, org.OwnerClient, org.ID, id); got.GetState() != deleted || got.GetEndReason() != reason {
		t.Fatalf("terminal sandbox = %v", got)
	}
	var pending bool
	if err := env.DB.Pool.QueryRow(t.Context(), "SELECT delete_pending FROM sandboxes WHERE sandbox_id = $1", id).Scan(&pending); err != nil || pending {
		t.Fatalf("delete_pending after terminal update = %v, %v", pending, err)
	}
	checkAllocation(0)

	unknown, err := uuidv7.NewString()
	if err != nil {
		t.Fatal(err)
	}
	replacement.SendUpdate(agent.SetSandbox(unknown, deleting, 0))
	waitDelete(replacement, unknown, nil)
	replacement.SendUpdate(agent.SetSandbox(id, deleting, 0))
	waitDelete(replacement, id, third)
}
