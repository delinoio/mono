// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/api"
	"github.com/delinoio/mono/servers/ownsand-api/internal/app"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// withholdAll makes the fake agent keep every command result to itself, as
// an agent whose connection dies before it answers.
func withholdAll(a *testenv.FakeAgent) {
	a.SetOnCommand(func(*testenv.FakeAgent, *ownsandv1.AgentCommand) *ownsandv1.AgentCommandResult { return nil })
}

// waitPauseCommandID waits for the pause command of the sandbox on the fake
// agent and returns its command ID. PauseSandbox can return before the fake
// agent's read loop recorded the command (the API stops waiting once its
// command wait ends or the command is queued), so the return of the RPC is
// not proof of receipt. A missing command fails through the bounded wait of
// WaitFor.
func waitPauseCommandID(t *testing.T, agent *testenv.FakeAgent, sandboxID string) string {
	t.Helper()
	env := agent.WaitFor("pause command of sandbox "+sandboxID, func(e *ownsandv1.AgentEnvelope) bool {
		return e.GetCommand().GetPauseSandbox().GetSandboxId() == sandboxID
	})
	id := env.GetCommand().GetCommandId()
	if id == "" {
		t.Fatal("the pause command has no command ID")
	}
	return id
}

func commandExpiryTiming() *api.Timing {
	return &api.Timing{HeartbeatTimeout: 45 * time.Second, CommandWait: 300 * time.Millisecond, EventPoll: 50 * time.Millisecond, OfflineSweep: 50 * time.Millisecond,
		CleanupPoll: time.Hour, ImportWait: time.Second, CommandGrace: api.DefaultCommandGrace}
}

func countLogs(env *testenv.Env, needle string) int { return strings.Count(env.Logs.String(), needle) }

// TestUnansweredCommandsExpireAfterChannelClose removes devices whose agent
// never answers the clean-up command: the record of each command is kept for
// the grace period after the channel closed and is gone after it, and a
// repeated sequence does not grow the retained count.
func TestUnansweredCommandsExpireAfterChannelClose(t *testing.T) {
	timing := commandExpiryTiming()
	env := testenv.New(t, func(o *app.Options) { o.Timing = timing })
	ctx := context.Background()
	org := env.NewOrg("alice")
	svc := env.App.Service
	for i := 0; i < 5; i++ {
		dev, agent := setupAgent(t, env, org, nil)
		withholdAll(agent)
		if _, err := org.OwnerClient.Device.RemoveDevice(ctx, connect.NewRequest(&ownsandv1.RemoveDeviceRequest{OrganizationId: org.ID, DeviceId: dev.ID})); err != nil {
			t.Fatal(err)
		}
		agent.WaitFor("clean-up command", func(e *ownsandv1.AgentEnvelope) bool { return e.GetCommand().GetCleanUp() != nil })
		select {
		case <-agent.Closed():
		case <-time.After(5 * time.Second):
			t.Fatal("the channel stayed open after the removal")
		}
		testenv.Eventually(t, "the disconnect marked the clean-up record", func() bool {
			return countLogs(env, `"commands_marked_for_expiry":1`) == i+1
		})
		// The record is still there inside the grace period.
		env.Clock.Advance(api.DefaultCommandGrace - time.Second)
		time.Sleep(200 * time.Millisecond)
		if got := svc.PendingCommands(); got != 1 {
			t.Fatalf("round %d: retained commands inside the grace period = %d, want 1", i, got)
		}
		// It is gone after the grace period, so the count does not grow.
		env.Clock.Advance(2 * time.Second)
		testenv.Eventually(t, "the unanswered command expired", func() bool { return svc.PendingCommands() == 0 })
	}
	// expireCommands removes the records before it writes the log line, so
	// the line of the last round can follow the zero count above.
	testenv.Eventually(t, "five expiry log lines", func() bool {
		return countLogs(env, `"msg":"agent_commands_expired"`) == 5
	})
}

// TestClosingOldGenerationKeepsReplacementCommands checks that the close of
// an older channel marks only its own commands: a command of the
// replacement channel stays eligible for its result after the grace period
// of the older commands passed.
func TestClosingOldGenerationKeepsReplacementCommands(t *testing.T) {
	timing := commandExpiryTiming()
	env := testenv.New(t, func(o *app.Options) { o.Timing = timing })
	ctx := context.Background()
	org := env.NewOrg("alice")
	svc := env.App.Service
	c := org.OwnerClient
	dev, agentA := setupAgent(t, env, org, nil)
	created, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(createReq(org.ID)))
	if err != nil {
		t.Fatal(err)
	}
	sandboxID := created.Msg.GetSandbox().GetSandboxId()
	waitState(t, c, org.ID, sandboxID, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)

	// A command of channel A that A never answers.
	withholdAll(agentA)
	if _, err := c.Sandbox.PauseSandbox(ctx, connect.NewRequest(&ownsandv1.PauseSandboxRequest{OrganizationId: org.ID, SandboxId: sandboxID})); err != nil {
		t.Fatal(err)
	}
	if got := svc.PendingCommands(); got != 1 {
		t.Fatalf("pending commands after the pause = %d, want 1", got)
	}

	// Replacement channel B closes A.
	agentB := env.NewFakeAgent(dev)
	agentB.SetSandbox(sandboxID, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING, ownsandv1.SandboxEndReason_SANDBOX_END_REASON_UNSPECIFIED)
	withholdAll(agentB)
	agentB.Connect()
	select {
	case <-agentA.Closed():
	case <-time.After(5 * time.Second):
		t.Fatal("the replaced channel stayed open")
	}
	testenv.Eventually(t, "the close of A marked its command", func() bool { return countLogs(env, `"commands_marked_for_expiry":1`) == 1 })
	// Connect can return on the live channel A before B registered, so wait
	// for the acknowledgement of B's state report.
	agentB.WaitFor("state report ack", func(e *ownsandv1.AgentEnvelope) bool { return e.GetStateAck().GetStateReport() != nil })

	// A command of channel B.
	if _, err := c.Sandbox.PauseSandbox(ctx, connect.NewRequest(&ownsandv1.PauseSandboxRequest{OrganizationId: org.ID, SandboxId: sandboxID})); err != nil {
		t.Fatal(err)
	}
	if got := svc.PendingCommands(); got != 2 {
		t.Fatalf("pending commands with both channels = %d, want 2", got)
	}
	commandB := waitPauseCommandID(t, agentB, sandboxID)

	// A's command expires; B's stays.
	env.Clock.Advance(api.DefaultCommandGrace + time.Second)
	testenv.Eventually(t, "the command of A expired", func() bool { return svc.PendingCommands() == 1 })
	time.Sleep(200 * time.Millisecond)
	if got := svc.PendingCommands(); got != 1 {
		t.Fatalf("pending commands after the expiry = %d, want 1", got)
	}
	// B's result is applied.
	paused := agentB.SetSandbox(sandboxID, ownsandv1.SandboxState_SANDBOX_STATE_PAUSED, ownsandv1.SandboxEndReason_SANDBOX_END_REASON_UNSPECIFIED)
	agentB.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_CommandResult{CommandResult: &ownsandv1.AgentCommandResult{
		CommandId: commandB, Result: &ownsandv1.AgentCommandResult_Sandbox{Sandbox: &ownsandv1.SandboxCommandResult{Sandbox: paused}},
	}}})
	waitState(t, c, org.ID, sandboxID, ownsandv1.SandboxState_SANDBOX_STATE_PAUSED)
	testenv.Eventually(t, "the result removed the record", func() bool { return svc.PendingCommands() == 0 })
}

// pauseWithLostResult pauses a running sandbox on a channel whose agent
// withholds the result, then closes that channel and connects a replacement.
// It returns the replacement agent, the pause command ID and the sandbox ID.
func pauseWithLostResult(t *testing.T, env *testenv.Env, org *testenv.Org) (*testenv.FakeAgent, string, string) {
	t.Helper()
	ctx := context.Background()
	c := org.OwnerClient
	dev, agentA := setupAgent(t, env, org, nil)
	created, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(createReq(org.ID)))
	if err != nil {
		t.Fatal(err)
	}
	sandboxID := created.Msg.GetSandbox().GetSandboxId()
	waitState(t, c, org.ID, sandboxID, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	withholdAll(agentA)
	if _, err := c.Sandbox.PauseSandbox(ctx, connect.NewRequest(&ownsandv1.PauseSandboxRequest{OrganizationId: org.ID, SandboxId: sandboxID})); err != nil {
		t.Fatal(err)
	}
	commandID := waitPauseCommandID(t, agentA, sandboxID)
	agentA.Close()
	testenv.Eventually(t, "the close marked the pause command", func() bool { return countLogs(env, `"commands_marked_for_expiry":1`) == 1 })
	agentB := env.NewFakeAgent(dev)
	agentB.SetSandbox(sandboxID, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING, ownsandv1.SandboxEndReason_SANDBOX_END_REASON_UNSPECIFIED)
	agentB.Connect()
	return agentB, commandID, sandboxID
}

func sendPauseResult(agent *testenv.FakeAgent, commandID, sandboxID string) {
	paused := agent.SetSandbox(sandboxID, ownsandv1.SandboxState_SANDBOX_STATE_PAUSED, ownsandv1.SandboxEndReason_SANDBOX_END_REASON_UNSPECIFIED)
	agent.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_CommandResult{CommandResult: &ownsandv1.AgentCommandResult{
		CommandId: commandID, Result: &ownsandv1.AgentCommandResult_Sandbox{Sandbox: &ownsandv1.SandboxCommandResult{Sandbox: paused}},
	}}})
}

// TestLateResultWithinGraceAppliesOnce checks that a result that arrives on
// a replacement channel of the same device inside the grace period is
// applied, and that a repeat of it is ignored.
func TestLateResultWithinGraceAppliesOnce(t *testing.T) {
	timing := commandExpiryTiming()
	env := testenv.New(t, func(o *app.Options) { o.Timing = timing })
	org := env.NewOrg("alice")
	agentB, commandID, sandboxID := pauseWithLostResult(t, env, org)
	env.Clock.Advance(api.DefaultCommandGrace - 10*time.Second)

	sendPauseResult(agentB, commandID, sandboxID)
	waitState(t, org.OwnerClient, org.ID, sandboxID, ownsandv1.SandboxState_SANDBOX_STATE_PAUSED)
	if got := env.App.Service.PendingCommands(); got != 0 {
		t.Fatalf("pending commands after the result = %d, want 0", got)
	}
	sendPauseResult(agentB, commandID, sandboxID)
	testenv.Eventually(t, "the repeat was ignored", func() bool { return countLogs(env, `"msg":"agent_command_result_unknown"`) == 1 })
	// The create result and the first pause result are the only applied ones.
	if got := countLogs(env, `"msg":"agent_command_result"`); got != 2 {
		t.Fatalf("applied results = %d, want 2", got)
	}
}

// TestLostResultAfterGraceChangesNothing checks that a result that arrives
// after the grace period is ignored: no command failure is synthesized and
// the durable sandbox state does not change.
func TestLostResultAfterGraceChangesNothing(t *testing.T) {
	timing := commandExpiryTiming()
	env := testenv.New(t, func(o *app.Options) { o.Timing = timing })
	org := env.NewOrg("alice")
	svc := env.App.Service
	agentB, commandID, sandboxID := pauseWithLostResult(t, env, org)
	before := getSandbox(t, org.OwnerClient, org.ID, sandboxID)

	env.Clock.Advance(api.DefaultCommandGrace + time.Second)
	testenv.Eventually(t, "the record expired", func() bool { return svc.PendingCommands() == 0 })
	after := getSandbox(t, org.OwnerClient, org.ID, sandboxID)
	if after.GetState() != before.GetState() || after.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING || after.GetEndReason() != 0 {
		t.Fatalf("the expiry changed the sandbox: before %v, after %v", before, after)
	}

	sendPauseResult(agentB, commandID, sandboxID)
	testenv.Eventually(t, "the late result was ignored", func() bool { return countLogs(env, `"msg":"agent_command_result_unknown"`) == 1 })
	time.Sleep(200 * time.Millisecond)
	if sb := getSandbox(t, org.OwnerClient, org.ID, sandboxID); sb.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		t.Fatalf("the ignored result changed the sandbox: %v", sb.GetState())
	}
}

// TestWrongDeviceResultKeepsPendingCommand checks that a result for a
// command that another device reports is rejected without discarding the
// pending command: the device that received the command still completes it
// exactly once and its sandbox state is applied.
func TestWrongDeviceResultKeepsPendingCommand(t *testing.T) {
	timing := commandExpiryTiming()
	env := testenv.New(t, func(o *app.Options) { o.Timing = timing })
	ctx := context.Background()
	org := env.NewOrg("alice")
	svc := env.App.Service
	c := org.OwnerClient
	_, agentA := setupAgent(t, env, org, nil)
	created, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(createReq(org.ID)))
	if err != nil {
		t.Fatal(err)
	}
	sandboxID := created.Msg.GetSandbox().GetSandboxId()
	waitState(t, c, org.ID, sandboxID, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	// A second device of the organization connects after the sandbox is placed.
	_, agentB := setupAgent(t, env, org, nil)
	agentB.WaitFor("state report ack", func(e *ownsandv1.AgentEnvelope) bool { return e.GetStateAck().GetStateReport() != nil })

	withholdAll(agentA)
	if _, err := c.Sandbox.PauseSandbox(ctx, connect.NewRequest(&ownsandv1.PauseSandboxRequest{OrganizationId: org.ID, SandboxId: sandboxID})); err != nil {
		t.Fatal(err)
	}
	commandID := waitPauseCommandID(t, agentA, sandboxID)
	if got := svc.PendingCommands(); got != 1 {
		t.Fatalf("pending commands after the pause = %d, want 1", got)
	}

	// Device B reports a result for the command of device A.
	agentB.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_CommandResult{CommandResult: &ownsandv1.AgentCommandResult{CommandId: commandID}}})
	testenv.Eventually(t, "the wrong-device result was logged", func() bool { return countLogs(env, "agent_command_result_wrong_device") == 1 })
	if got := svc.PendingCommands(); got != 1 {
		t.Fatalf("pending commands after the wrong device's result = %d, want 1", got)
	}

	// Device A completes the command, and its sandbox state is applied.
	paused := agentA.SetSandbox(sandboxID, ownsandv1.SandboxState_SANDBOX_STATE_PAUSED, ownsandv1.SandboxEndReason_SANDBOX_END_REASON_UNSPECIFIED)
	result := &ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_CommandResult{CommandResult: &ownsandv1.AgentCommandResult{
		CommandId: commandID, Result: &ownsandv1.AgentCommandResult_Sandbox{Sandbox: &ownsandv1.SandboxCommandResult{Sandbox: paused}},
	}}}
	agentA.Send(result)
	waitState(t, c, org.ID, sandboxID, ownsandv1.SandboxState_SANDBOX_STATE_PAUSED)
	testenv.Eventually(t, "the result removed the record", func() bool { return svc.PendingCommands() == 0 })

	// A repeated result finds no record.
	agentA.Send(result)
	testenv.Eventually(t, "the repeated result was unknown", func() bool { return countLogs(env, "agent_command_result_unknown") == 1 })
	applied := 0
	for _, line := range strings.Split(env.Logs.String(), "\n") {
		if strings.Contains(line, `"msg":"agent_command_result"`) && strings.Contains(line, commandID) {
			applied++
		}
	}
	if applied != 1 {
		t.Fatalf("applied results of the pause command = %d, want 1", applied)
	}
}
