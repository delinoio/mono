// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/api"
	"github.com/delinoio/mono/servers/ownsand-api/internal/app"
	"github.com/delinoio/mono/servers/ownsand-api/internal/billing"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// The tests in this file cover the durable billing pause ownership
// (docs/servers-ownsand-api-foundation.md#suspension-and-resume): the pause
// mark of a sandbox stays for the rest of its life until a caller pauses it
// or it ends, because a pause command that billing sent can finish on the
// agent after the API stopped waiting for it. They run against a fake agent
// and a fake delino-accounts, with a short command wait, not against a real
// device.

// lateFixture is a marker fixture whose command wait is short and whose
// agent withholds the result of every pause command, as an agent whose pause
// is still waiting for its sandbox lock or runtime operation.
type lateFixture struct {
	*markerFixture
	mu     sync.Mutex
	pauses []string
}

func newLateFixture(t *testing.T) *lateFixture {
	t.Helper()
	f := &lateFixture{markerFixture: newMarkerFixtureWith(t, func(o *app.Options) { o.Timing = commandExpiryTiming() })}
	f.agent.SetOnCommand(f.hook)
	return f
}

// hook retains the ID of each pause command and sends no result for it, and
// answers every other command at once. It never blocks the receive loop of
// the fake agent.
func (f *lateFixture) hook(a *testenv.FakeAgent, cmd *ownsandv1.AgentCommand) *ownsandv1.AgentCommandResult {
	if cmd.GetPauseSandbox() == nil {
		return a.DefaultAnswer(cmd)
	}
	f.mu.Lock()
	f.pauses = append(f.pauses, cmd.GetCommandId())
	f.mu.Unlock()
	return nil
}

// waitPauses waits until n pause commands were captured and returns their
// IDs in dispatch order.
func (f *lateFixture) waitPauses(t *testing.T, n int) []string {
	t.Helper()
	testenv.Eventually(t, "the pause commands reach the agent", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.pauses) >= n
	})
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.pauses...)
}

// suspendedPass suspends the organization and runs a pass whose pause
// outlives the command wait: when it returns, the database still shows the
// sandbox RUNNING, billing has marked it, and the pause command is captured
// and unanswered.
func (f *lateFixture) suspendedPass(t *testing.T, sandboxID string) {
	t.Helper()
	setOrgPhase(t, f.env, f.org, billing.PhaseSuspended)
	enforceBilling(t, f.env)
	f.expectRunningMarked(t, sandboxID)
}

func (f *lateFixture) expectRunningMarked(t *testing.T, sandboxID string) {
	t.Helper()
	// A client of the current instance: the test can restart the control plane.
	if s := getSandbox(t, f.env.User(f.org.Owner), f.org.ID, sandboxID); s.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		t.Fatalf("the sandbox is %v, want RUNNING (the pause has not completed)", s.GetState())
	}
	if !pausedByBilling(t, f.env, sandboxID) {
		t.Fatal("billing did not keep its mark of the RUNNING sandbox")
	}
}

// recoverWhileRunning confirms the recovery and runs a pass while the sandbox is still
// RUNNING: it must leave the sandbox and its mark alone.
func (f *lateFixture) recoverWhileRunning(t *testing.T, sandboxID string) {
	t.Helper()
	setOrgPhase(t, f.env, f.org, billing.PhaseAllowed)
	resumes, pauses := resumeCommands(f.agent, sandboxID), pauseCommands(f.agent, sandboxID)
	enforceBilling(t, f.env)
	f.expectRunningMarked(t, sandboxID)
	if got := resumeCommands(f.agent, sandboxID); got != resumes {
		t.Fatalf("a recovery pass sent a resume to a RUNNING sandbox (%d -> %d)", resumes, got)
	}
	if got := pauseCommands(f.agent, sandboxID); got != pauses {
		t.Fatalf("a recovery pass sent a pause (%d -> %d)", pauses, got)
	}
}

// resumeLatePause runs the recovery that follows a late pause: the sandbox
// is PAUSED and marked, so one pass resumes it, and billing keeps its
// mark. Stable passes afterwards send nothing.
func (f *lateFixture) resumeLatePause(t *testing.T, c *testenv.Clients, sandboxID string, wantResumes int) {
	t.Helper()
	waitState(t, c, f.org.ID, sandboxID, ownsandv1.SandboxState_SANDBOX_STATE_PAUSED)
	if !pausedByBilling(t, f.env, sandboxID) {
		t.Fatal("the late pause left the sandbox without its billing mark")
	}
	enforceBilling(t, f.env)
	waitState(t, c, f.org.ID, sandboxID, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	if got := resumeCommands(f.agent, sandboxID); got != wantResumes {
		t.Fatalf("resume commands = %d, want %d", got, wantResumes)
	}
	for range 2 {
		enforceBilling(t, f.env)
	}
	if got := resumeCommands(f.agent, sandboxID); got != wantResumes {
		t.Fatalf("resume commands after stable passes = %d, want %d", got, wantResumes)
	}
	if !pausedByBilling(t, f.env, sandboxID) {
		t.Fatal("the automatic resume cleared the billing mark")
	}
}

// TestLateBillingPauseKeepsOwnershipAfterRecovery is the late completion:
// the billing pause outlives the command wait, the recovery pass sees the
// sandbox RUNNING, and the agent's pause then completes with a newer
// revision. The sandbox must stay owned by billing, so the next pass resumes
// it. The resume must not retire the ownership either.
func TestLateBillingPauseKeepsOwnershipAfterRecovery(t *testing.T) {
	f := newLateFixture(t)
	c := f.org.OwnerClient
	id := f.create(t)
	f.suspendedPass(t, id)
	pauseID := f.waitPauses(t, 1)[0]
	f.recoverWhileRunning(t, id)

	// The agent's pause completes now, with a newer valid revision.
	sendPauseResult(f.agent, pauseID, id)
	f.resumeLatePause(t, c, id, 1)

	// Stable marked RUNNING passes write no success log: one resume, and no
	// cleanup of a live sandbox.
	if got := countLogs(f.env, `"msg":"billing_sandbox_resumed"`); got != 1 {
		t.Fatalf("billing_sandbox_resumed logged %d times, want 1", got)
	}
	if got := countLogs(f.env, `"msg":"billing_sandbox_unmarked"`); got != 0 {
		t.Fatalf("billing_sandbox_unmarked logged %d times for a live sandbox", got)
	}
}

// TestLateBillingPauseUpdateAfterChannelReplacement completes the late pause
// through a sandbox update, not a command result, after the channel of the
// command was replaced and the record of the command expired.
func TestLateBillingPauseUpdateAfterChannelReplacement(t *testing.T) {
	f := newLateFixture(t)
	c := f.org.OwnerClient
	id := f.create(t)
	f.suspendedPass(t, id)
	f.waitPauses(t, 1)

	// The channel closes; the record of the unanswered command is kept for
	// the grace period and then gone.
	f.agent.Close()
	select {
	case <-f.agent.Closed():
	case <-time.After(5 * time.Second):
		t.Fatal("the channel stayed open")
	}
	testenv.Eventually(t, "the close marked the pause command", func() bool { return countLogs(f.env, `"commands_marked_for_expiry":1`) == 1 })
	svc := f.env.App.Service
	f.env.Clock.Advance(api.DefaultCommandGrace + time.Second)
	testenv.Eventually(t, "the unanswered pause command expired", func() bool { return svc.PendingCommands() == 0 })

	// The replacement channel reports RUNNING: the old operation has not
	// completed yet.
	replacement := f.env.NewFakeAgent(f.dev)
	replacement.SetSandbox(id, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING, ownsandv1.SandboxEndReason_SANDBOX_END_REASON_UNSPECIFIED)
	replacement.Connect()
	replacement.WaitFor("state report ack", func(e *ownsandv1.AgentEnvelope) bool { return e.GetStateAck().GetStateReport() != nil })
	f.agent = replacement
	waitState(t, c, f.org.ID, id, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	f.recoverWhileRunning(t, id)

	// The old operation completes and the agent reports the newer PAUSED.
	replacement.SendUpdate(replacement.SetSandbox(id, ownsandv1.SandboxState_SANDBOX_STATE_PAUSED, ownsandv1.SandboxEndReason_SANDBOX_END_REASON_UNSPECIFIED))
	f.resumeLatePause(t, c, id, 1)
}

// TestLateBillingPauseUpdateAfterControlPlaneReplacement recreates the
// service between the sent pause and the late update, so every in-memory
// command record is gone, and checks that the persisted mark alone makes the
// recovery resume the late PAUSED state.
func TestLateBillingPauseUpdateAfterControlPlaneReplacement(t *testing.T) {
	f := newLateFixture(t)
	id := f.create(t)
	f.suspendedPass(t, id)
	f.waitPauses(t, 1)

	f.env.Restart()
	select {
	case <-f.agent.Closed():
	case <-time.After(10 * time.Second):
		t.Fatal("the channel stayed open after the control plane restarted")
	}
	f.agent.Connect()
	c := f.env.User(f.org.Owner)
	f.agent.WaitFor("state report ack", func(e *ownsandv1.AgentEnvelope) bool { return e.GetStateAck().GetStateReport() != nil })
	if got := f.env.App.Service.PendingCommands(); got != 0 {
		t.Fatalf("the new service remembers %d commands", got)
	}
	f.expectRunningMarked(t, id)
	// The new instance's recovery pass finds the RUNNING sandbox marked.
	f.recoverWhileRunning(t, id)

	f.agent.SendUpdate(f.agent.SetSandbox(id, ownsandv1.SandboxState_SANDBOX_STATE_PAUSED, ownsandv1.SandboxEndReason_SANDBOX_END_REASON_UNSPECIFIED))
	f.resumeLatePause(t, c, id, 1)
}

// TestTwoInFlightBillingPausesKeepOwnership lets two suspended passes send
// two pauses while the sandbox stays RUNNING, and finishes them on opposite
// sides of the recovery's resume. Both resulting pauses are resumed.
func TestTwoInFlightBillingPausesKeepOwnership(t *testing.T) {
	f := newLateFixture(t)
	c := f.org.OwnerClient
	id := f.create(t)
	f.suspendedPass(t, id)
	// The second suspended pass dispatches again: the sandbox is still
	// RUNNING and the mark is already set.
	enforceBilling(t, f.env)
	f.expectRunningMarked(t, id)
	pauses := f.waitPauses(t, 2)
	if pauses[0] == pauses[1] {
		t.Fatalf("the two passes sent the same command %s", pauses[0])
	}
	f.recoverWhileRunning(t, id)

	// The first pause finishes; recovery resumes it, and the ownership stays.
	sendPauseResult(f.agent, pauses[0], id)
	f.resumeLatePause(t, c, id, 1)

	// The second pause finishes after the resume, with a newer revision. A
	// flag cleared by the first resume would strand the sandbox here.
	sendPauseResult(f.agent, pauses[1], id)
	f.resumeLatePause(t, c, id, 2)
}

// TestCallerPauseAfterRecoveryTakesOwnership checks that the retained mark
// of a recovered RUNNING sandbox is taken over by a caller's pause (the
// dispatching path), and that the sandbox then stays paused through later
// passes and a second suspension and recovery. A sandbox that a caller paused
// before the suspension never gets a mark.
func TestCallerPauseAfterRecoveryTakesOwnership(t *testing.T) {
	f := newMarkerFixture(t)
	c := f.org.OwnerClient
	owned, callerFirst := f.create(t), f.create(t)
	if _, err := pauseRPC(c, f.org.ID, callerFirst); err != nil {
		t.Fatal(err)
	}
	f.suspendAll(t, owned)
	if pausedByBilling(t, f.env, callerFirst) {
		t.Fatal("the suspension marked a caller-paused sandbox")
	}
	setOrgPhase(t, f.env, f.org, billing.PhaseAllowed)
	enforceBilling(t, f.env)
	waitState(t, c, f.org.ID, owned, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	expectMarkRetained(t, f, owned)

	pauses := pauseCommands(f.agent, owned)
	sb, err := pauseRPC(c, f.org.ID, owned)
	if err != nil || sb.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_PAUSED {
		t.Fatalf("PauseSandbox = %v, %v", sb.GetState(), err)
	}
	if pausedByBilling(t, f.env, owned) || pauseCommands(f.agent, owned) != pauses+1 {
		t.Fatalf("after the caller's pause: marked=%v pause commands=%d, want unmarked and %d", pausedByBilling(t, f.env, owned), pauseCommands(f.agent, owned), pauses+1)
	}
	resumes := resumeCommands(f.agent, owned)
	for range 2 {
		enforceBilling(t, f.env)
	}
	// A suspension and a second recovery leave both caller-paused sandboxes
	// alone: neither is RUNNING for the suspension, and neither is marked.
	setOrgPhase(t, f.env, f.org, billing.PhaseSuspended)
	enforceBilling(t, f.env)
	setOrgPhase(t, f.env, f.org, billing.PhaseAllowed)
	enforceBilling(t, f.env)
	for _, id := range []string{owned, callerFirst} {
		if pausedByBilling(t, f.env, id) {
			t.Fatalf("sandbox %s is marked", id)
		}
		if s := getSandbox(t, c, f.org.ID, id); s.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_PAUSED {
			t.Fatalf("sandbox %s is %v, want PAUSED", id, s.GetState())
		}
	}
	if got := resumeCommands(f.agent, owned); got != resumes {
		t.Fatalf("resume commands for the caller-paused sandbox = %d, want %d", got, resumes)
	}
	if got := resumeCommands(f.agent, callerFirst); got != 0 {
		t.Fatalf("resume commands for the sandbox paused before the suspension = %d, want 0", got)
	}
}

// TestCallerPauseOfLateBillingPauseTakesOwnership covers the caller pause
// that follows a late billing pause: the sandbox is marked and PAUSED, and
// the idempotent caller pause clears the mark without another command, so the
// recovery leaves it paused.
func TestCallerPauseOfLateBillingPauseTakesOwnership(t *testing.T) {
	f := newLateFixture(t)
	c := f.org.OwnerClient
	id := f.create(t)
	f.suspendedPass(t, id)
	pauseID := f.waitPauses(t, 1)[0]
	sendPauseResult(f.agent, pauseID, id)
	waitState(t, c, f.org.ID, id, ownsandv1.SandboxState_SANDBOX_STATE_PAUSED)

	pauses := pauseCommands(f.agent, id)
	if sb, err := pauseRPC(c, f.org.ID, id); err != nil || sb.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_PAUSED {
		t.Fatalf("PauseSandbox = %v, %v", sb.GetState(), err)
	}
	if pausedByBilling(t, f.env, id) || pauseCommands(f.agent, id) != pauses {
		t.Fatalf("after the idempotent pause: marked=%v pause commands=%d, want unmarked and %d", pausedByBilling(t, f.env, id), pauseCommands(f.agent, id), pauses)
	}
	setOrgPhase(t, f.env, f.org, billing.PhaseAllowed)
	enforceBilling(t, f.env)
	if got := resumeCommands(f.agent, id); got != 0 {
		t.Fatalf("resume commands = %d, want 0", got)
	}
}

// TestBillingOwnershipBoundaries checks, in one organization, how the
// recovery treats each kind of sandbox: a caller-paused sandbox stays
// unmarked and paused, a marked PAUSED sandbox with a pending delete keeps
// its mark and is not resumed, an ended sandbox loses its mark without a
// command (DELETED and FAILED, idempotently), and only a marked PAUSED
// sandbox without a pending delete resumes and keeps its mark. Nothing clears
// a mark while the organization stays suspended.
func TestBillingOwnershipBoundaries(t *testing.T) {
	f := newMarkerFixture(t)
	c := f.org.OwnerClient
	callerPaused, resumable, deleting, deleted, failed := f.create(t), f.create(t), f.create(t), f.create(t), f.create(t)
	if _, err := pauseRPC(c, f.org.ID, callerPaused); err != nil {
		t.Fatal(err)
	}
	f.suspendAll(t, resumable, deleting, deleted, failed)
	if _, err := f.env.DB.Pool.Exec(context.Background(), "UPDATE sandboxes SET delete_pending = true WHERE sandbox_id = $1", deleting); err != nil {
		t.Fatal(err)
	}
	f.agent.SendUpdate(f.agent.SetSandbox(deleted, ownsandv1.SandboxState_SANDBOX_STATE_DELETED, ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DELETED_BY_CALLER))
	f.agent.SendUpdate(f.agent.SetSandbox(failed, ownsandv1.SandboxState_SANDBOX_STATE_FAILED, ownsandv1.SandboxEndReason_SANDBOX_END_REASON_LOST_ON_DEVICE))
	waitState(t, c, f.org.ID, deleted, ownsandv1.SandboxState_SANDBOX_STATE_DELETED)
	waitState(t, c, f.org.ID, failed, ownsandv1.SandboxState_SANDBOX_STATE_FAILED)

	// While the organization is suspended, a pass changes no mark.
	enforceBilling(t, f.env)
	for _, id := range []string{resumable, deleting, deleted, failed} {
		if !pausedByBilling(t, f.env, id) {
			t.Fatalf("a suspended pass cleared the mark of %s", id)
		}
	}
	if pausedByBilling(t, f.env, callerPaused) {
		t.Fatal("the caller-paused sandbox was marked")
	}

	setOrgPhase(t, f.env, f.org, billing.PhaseAllowed)
	for pass := 0; pass < 3; pass++ {
		enforceBilling(t, f.env)
		if pass == 0 {
			waitState(t, c, f.org.ID, resumable, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
		}
	}
	for id, want := range map[string]bool{callerPaused: false, resumable: true, deleting: true, deleted: false, failed: false} {
		if got := pausedByBilling(t, f.env, id); got != want {
			t.Fatalf("mark of %s = %v, want %v", id, got, want)
		}
	}
	for id, want := range map[string]int{callerPaused: 0, resumable: 1, deleting: 0, deleted: 0, failed: 0} {
		if got := resumeCommands(f.agent, id); got != want {
			t.Fatalf("resume commands for %s = %d, want %d", id, got, want)
		}
	}
	var state int16
	if err := f.env.DB.Pool.QueryRow(context.Background(), "SELECT agent_state FROM sandboxes WHERE sandbox_id = $1", deleting).Scan(&state); err != nil || ownsandv1.SandboxState(state) != ownsandv1.SandboxState_SANDBOX_STATE_PAUSED {
		t.Fatalf("the marked sandbox with a pending delete is %v, %v, want PAUSED", ownsandv1.SandboxState(state), err)
	}
	// Terminal cleanup is logged once per ended sandbox, the resume once.
	if got := countLogs(f.env, `"msg":"billing_sandbox_unmarked"`); got != 2 {
		t.Fatalf("billing_sandbox_unmarked logged %d times, want 2", got)
	}
	if got := countLogs(f.env, `"msg":"billing_sandbox_resumed"`); got != 1 {
		t.Fatalf("billing_sandbox_resumed logged %d times, want 1", got)
	}
}

// TestBillingOwnershipSurvivesOfflineAndResuspension keeps a recovered
// sandbox marked while its device is offline (no command, no write), and
// pauses it again, with the mark it already has, when the organization is
// suspended again.
func TestBillingOwnershipSurvivesOfflineAndResuspension(t *testing.T) {
	f := newMarkerFixture(t)
	c := f.org.OwnerClient
	id := f.create(t)
	f.suspendAll(t, id)
	setOrgPhase(t, f.env, f.org, billing.PhaseAllowed)
	enforceBilling(t, f.env)
	waitState(t, c, f.org.ID, id, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	expectMarkRetained(t, f, id)

	f.agent.Close()
	testenv.Eventually(t, "the device is offline", func() bool {
		return getSandbox(t, c, f.org.ID, id).GetState() == ownsandv1.SandboxState_SANDBOX_STATE_UNREACHABLE
	})
	commands := len(f.agent.Commands())
	for range 2 {
		enforceBilling(t, f.env)
	}
	if !pausedByBilling(t, f.env, id) || len(f.agent.Commands()) != commands {
		t.Fatalf("an offline marked sandbox changed: marked=%v commands %d -> %d", pausedByBilling(t, f.env, id), commands, len(f.agent.Commands()))
	}

	f.agent.Connect()
	waitState(t, c, f.org.ID, id, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	pauses := pauseCommands(f.agent, id)
	setOrgPhase(t, f.env, f.org, billing.PhaseSuspended)
	enforceBilling(t, f.env)
	waitState(t, c, f.org.ID, id, ownsandv1.SandboxState_SANDBOX_STATE_PAUSED)
	if !pausedByBilling(t, f.env, id) || pauseCommands(f.agent, id) != pauses+1 {
		t.Fatalf("after the second suspension: marked=%v pause commands=%d, want marked and %d", pausedByBilling(t, f.env, id), pauseCommands(f.agent, id), pauses+1)
	}
	// A caller's pause with an unavailable dispatch keeps the mark.
	f.agent.Close()
	testenv.Eventually(t, "the device is offline again", func() bool {
		return getSandbox(t, c, f.org.ID, id).GetState() == ownsandv1.SandboxState_SANDBOX_STATE_UNREACHABLE
	})
	if _, err := pauseRPC(c, f.org.ID, id); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("PauseSandbox of an unreachable sandbox = %v, want unavailable", err)
	}
	if !pausedByBilling(t, f.env, id) {
		t.Fatal("an unavailable caller pause cleared the mark")
	}
}
