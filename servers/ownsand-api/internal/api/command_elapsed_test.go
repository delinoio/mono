// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"testing"
	"time"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/api"
	"github.com/delinoio/mono/servers/ownsand-api/internal/app"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// TestCommandGraceIgnoresForwardWallStep checks that a forward correction of
// the wall clock does not end the retention window early: the window of an
// unanswered command is measured in elapsed time, so a result that arrives
// 10 seconds before the window ends is applied although the wall clock
// jumped 10 minutes ahead.
func TestCommandGraceIgnoresForwardWallStep(t *testing.T) {
	timing := commandExpiryTiming()
	env := testenv.New(t, func(o *app.Options) { o.Timing = timing })
	org := env.NewOrg("alice")
	svc := env.App.Service
	agentB, commandID, sandboxID := pauseWithLostResult(t, env, org)

	env.Clock.Advance(api.DefaultCommandGrace - 10*time.Second)
	env.Clock.StepWall(10 * time.Minute)
	// Give the sweep several rounds to (wrongly) remove the record.
	time.Sleep(300 * time.Millisecond)
	if got := svc.PendingCommands(); got != 1 {
		t.Fatalf("retained commands after a forward wall step = %d, want 1", got)
	}
	if got := countLogs(env, `"msg":"agent_commands_expired"`); got != 0 {
		t.Fatalf("expiry log lines after a forward wall step = %d, want 0", got)
	}

	sendPauseResult(agentB, commandID, sandboxID)
	waitState(t, org.OwnerClient, org.ID, sandboxID, ownsandv1.SandboxState_SANDBOX_STATE_PAUSED)
	if got := svc.PendingCommands(); got != 0 {
		t.Fatalf("pending commands after the result = %d, want 0", got)
	}
	if got := countLogs(env, `"msg":"agent_command_result_unknown"`); got != 0 {
		t.Fatalf("unknown result log lines = %d, want 0", got)
	}
}

// TestCommandGraceIgnoresBackwardWallStep checks that a backward correction
// of the wall clock does not keep an expired record: once the window passed
// in elapsed time, the sweep removes the record and a late result is
// ignored, although the wall clock is still before the time the channel
// closed.
func TestCommandGraceIgnoresBackwardWallStep(t *testing.T) {
	timing := commandExpiryTiming()
	env := testenv.New(t, func(o *app.Options) { o.Timing = timing })
	org := env.NewOrg("alice")
	svc := env.App.Service
	agentB, commandID, sandboxID := pauseWithLostResult(t, env, org)

	env.Clock.StepWall(-10 * time.Minute)
	env.Clock.Advance(api.DefaultCommandGrace + time.Second)
	if now := env.Clock.Now(); !now.Before(time.Now().Add(-5 * time.Minute)) {
		t.Fatalf("the wall clock is not stepped back: %v", now)
	}
	testenv.Eventually(t, "the record expired after a backward wall step", func() bool { return svc.PendingCommands() == 0 })
	testenv.Eventually(t, "the sweep logged the expiry", func() bool { return countLogs(env, `"msg":"agent_commands_expired"`) == 1 })

	sendPauseResult(agentB, commandID, sandboxID)
	testenv.Eventually(t, "the late result was ignored", func() bool { return countLogs(env, `"msg":"agent_command_result_unknown"`) == 1 })
	time.Sleep(200 * time.Millisecond)
	if sb := getSandbox(t, org.OwnerClient, org.ID, sandboxID); sb.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		t.Fatalf("the ignored result changed the sandbox: %v", sb.GetState())
	}
}

// TestCommandGraceTakeRejectsExpiredBeforeSweep checks that a result that
// arrives after the window passed in elapsed time is ignored before the sweep
// removed the record, although a backward wall step keeps the wall clock
// before the time the channel closed. The sweep is slowed so only take can
// decide.
func TestCommandGraceTakeRejectsExpiredBeforeSweep(t *testing.T) {
	timing := commandExpiryTiming()
	timing.OfflineSweep = time.Hour
	env := testenv.New(t, func(o *app.Options) { o.Timing = timing })
	org := env.NewOrg("alice")
	svc := env.App.Service
	agentB, commandID, sandboxID := pauseWithLostResult(t, env, org)

	env.Clock.StepWall(-10 * time.Minute)
	env.Clock.Advance(api.DefaultCommandGrace + time.Second)
	if got := svc.PendingCommands(); got != 1 {
		t.Fatalf("the slowed sweep removed the record: %d", got)
	}
	sendPauseResult(agentB, commandID, sandboxID)
	testenv.Eventually(t, "the expired result was ignored by take", func() bool { return countLogs(env, `"msg":"agent_command_result_unknown"`) == 1 })
	if got := svc.PendingCommands(); got != 0 {
		t.Fatalf("take left the expired record: %d", got)
	}
}
