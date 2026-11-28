// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/app"
	"github.com/delinoio/mono/servers/ownsand-api/internal/billing"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// billingEnforceSlots is the worker limit for one enforcement pass
// (billingEnforceParallel in package api).
const billingEnforceSlots = 8

func resumeCommands(agent *testenv.FakeAgent, sandboxID string) int {
	n := 0
	for _, c := range agent.Commands() {
		if c.GetResumeSandbox().GetSandboxId() == sandboxID {
			n++
		}
	}
	return n
}

// slotHolder withholds the answers of the first n commands of one kind that
// a fake agent receives, so that the enforcement pass keeps its slots busy,
// and answers every later command as usual. Release answers the withheld
// commands.
type slotHolder struct {
	agent *testenv.FakeAgent
	n     int
	match func(*ownsandv1.AgentCommand) bool

	mu   sync.Mutex
	held []*ownsandv1.AgentCommand
}

func holdSlots(agent *testenv.FakeAgent, n int, match func(*ownsandv1.AgentCommand) bool) *slotHolder {
	h := &slotHolder{agent: agent, n: n, match: match}
	agent.SetOnCommand(func(a *testenv.FakeAgent, cmd *ownsandv1.AgentCommand) *ownsandv1.AgentCommandResult {
		if h.match(cmd) {
			h.mu.Lock()
			defer h.mu.Unlock()
			if len(h.held) < h.n {
				h.held = append(h.held, cmd)
				return nil
			}
		}
		return a.DefaultAnswer(cmd)
	})
	return h
}

func (h *slotHolder) heldCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.held)
}

// Release answers every withheld command with the default answer.
func (h *slotHolder) Release() {
	h.mu.Lock()
	held := h.held
	h.mu.Unlock()
	for _, cmd := range held {
		res := h.agent.DefaultAnswer(cmd)
		res.CommandId = cmd.GetCommandId()
		h.agent.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_CommandResult{CommandResult: res}})
	}
}

// staleFixture holds the organizations of a stale-target test: the hog
// organization fills the slots of the pass, and the victim organization's
// sandbox waits for a slot with its target selected earlier. The hog
// organization sorts first, so the pass starts its sandboxes first.
type staleFixture struct {
	env                   *testenv.Env
	hog, victim           *testenv.Org
	hogAgent, victimAgent *testenv.FakeAgent
	hogSandboxes          []string
	victimSandbox         string
	replyLimit            int
	passDone              chan error
}

func newStaleFixture(t *testing.T) *staleFixture {
	t.Helper()
	env, _ := billingEnvWith(t, func(o *app.Options) {
		// Keep replies pending through the 10-second Eventually window
		// and the database update that changes the victim's phase.
		o.Timing.CommandWait = 30 * time.Second
	})
	// Each sandbox ordering lock holds one connection. The ordering
	// limit leaves half the pool free for other work, including setPhase.
	replyLimit := min(billingEnforceSlots, max(1, int(env.DB.Pool.Config().MaxConns)/2))
	ctx := context.Background()
	a := env.NewOrg("alice")
	b := env.NewOrg("bob")
	f := &staleFixture{env: env, hog: a, victim: b, replyLimit: replyLimit}
	if b.ID < a.ID {
		f.hog, f.victim = b, a
	}
	big := &ownsandv1.DeviceLimits{VcpuCount: 64, MemoryMib: 65536, DiskGib: 1000, VcpuOvercommitPercent: 100}
	_, f.hogAgent = setupAgent(t, env, f.hog, big)
	_, f.victimAgent = setupAgent(t, env, f.victim, big)
	create := func(org *testenv.Org) string {
		created, err := org.OwnerClient.Sandbox.CreateSandbox(ctx, connect.NewRequest(createReq(org.ID)))
		if err != nil {
			t.Fatal(err)
		}
		id := created.Msg.GetSandbox().GetSandboxId()
		waitState(t, org.OwnerClient, org.ID, id, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
		return id
	}
	// Fill all worker slots even when the ordering limit is lower. The
	// extra control sandbox queues like the victim and must still be enforced.
	for i := 0; i < billingEnforceSlots+1; i++ {
		f.hogSandboxes = append(f.hogSandboxes, create(f.hog))
	}
	f.victimSandbox = create(f.victim)
	return f
}

// setPhase commits the billing phase of an organization the way the status
// worker stores it: SUSPENDED (non-payment for 4 days) or ALLOWED. The test
// writes the cache row directly instead of polling a fake delino-accounts,
// because a status change that the control plane applies also kicks a
// background enforcement pass, which would race with the pass under test.
func (f *staleFixture) setPhase(t *testing.T, org *testenv.Org, phase ownsandv1.BillingPhase) {
	t.Helper()
	var err error
	switch phase {
	case billing.PhaseSuspended:
		since := f.env.Clock.Now().Add(-4 * 24 * time.Hour)
		_, err = f.env.DB.Pool.Exec(context.Background(), `UPDATE organization_billing SET phase = $2, reason = $3, blocked_since = $4, suspend_time = $5
			WHERE organization_id = $1`, org.ID, int16(phase), int16(ownsandv1.BillingReason_BILLING_REASON_SUSPENDED), since, since.Add(billing.SuspendAfter))
	default:
		_, err = f.env.DB.Pool.Exec(context.Background(), `UPDATE organization_billing SET phase = $2, reason = $3, blocked_since = NULL, suspend_time = NULL
			WHERE organization_id = $1`, org.ID, int16(phase), int16(ownsandv1.BillingReason_BILLING_REASON_UNSPECIFIED))
	}
	if err != nil {
		t.Fatalf("set billing phase: %v", err)
	}
}

type stalePassTargets struct {
	pause, resume []string
}

// startPass starts an enforcement pass and waits for its target lists.
func (f *staleFixture) startPass(t *testing.T, targetList string) stalePassTargets {
	t.Helper()
	selected := make(chan stalePassTargets, 1)
	f.passDone = make(chan error, 1)
	go func() {
		defer close(selected)
		f.passDone <- f.env.App.Enforcer.PassWithTargetsForTest(t.Context(), func(pause, resume []string) {
			selected <- stalePassTargets{pause: pause, resume: resume}
		})
	}()
	var targets stalePassTargets
	select {
	case lists, ok := <-selected:
		if !ok {
			t.Fatalf("did not observe the %s target list for sandbox %s: enforcement pass returned %v", targetList, f.victimSandbox, <-f.passDone)
		}
		targets = lists
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for the %s target list for sandbox %s", targetList, f.victimSandbox)
	}
	return targets
}

func (f *staleFixture) waitForHeldReplies(t *testing.T, hold *slotHolder) {
	t.Helper()
	testenv.Eventually(t, "the reply barrier to fill", func() bool { return hold.heldCount() == f.replyLimit })
	t.Logf("reply barrier reached (held=%d, pool_max_conns=%d)", f.replyLimit, f.env.DB.Pool.Config().MaxConns)
}

func (f *staleFixture) finishPass(t *testing.T) {
	t.Helper()
	select {
	case err := <-f.passDone:
		if err != nil {
			t.Fatalf("billing enforcement pass: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the enforcement pass did not finish")
	}
}

// TestBillingPauseDroppedAfterRecoveryWhileQueued selects the pause targets
// of two suspended organizations, keeps every slot of the pass busy, commits
// the recovery of one organization while its sandbox waits for a slot and
// checks that the pass neither marks nor pauses that sandbox, while the
// sandboxes of the organization that stays suspended are enforced.
func TestBillingPauseDroppedAfterRecoveryWhileQueued(t *testing.T) {
	t.Parallel()
	f := newStaleFixture(t)
	c := f.victim.OwnerClient
	hold := holdSlots(f.hogAgent, f.replyLimit, func(cmd *ownsandv1.AgentCommand) bool { return cmd.GetPauseSandbox() != nil })
	f.setPhase(t, f.hog, billing.PhaseSuspended)
	f.setPhase(t, f.victim, billing.PhaseSuspended)

	targets := f.startPass(t, "pause")
	if !slices.Contains(targets.pause, f.victimSandbox) {
		t.Fatalf("pause target list %v is missing sandbox %s", targets.pause, f.victimSandbox)
	}
	f.waitForHeldReplies(t, hold)
	// The victim's target was selected while it was suspended; its recovery
	// commits before the sandbox reaches a slot.
	f.setPhase(t, f.victim, billing.PhaseAllowed)
	hold.Release()
	f.finishPass(t)

	if got := pauseCommands(f.victimAgent, f.victimSandbox); got != 0 {
		t.Fatalf("pause commands for the recovered organization's sandbox = %d, want 0", got)
	}
	if pausedByBilling(t, f.env, f.victimSandbox) {
		t.Fatal("the recovered organization's sandbox carries a stale billing mark")
	}
	if s := getSandbox(t, c, f.victim.ID, f.victimSandbox); s.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		t.Fatalf("the recovered organization's sandbox is %v, want RUNNING", s.GetState())
	}
	// Stable status: every sandbox of the organization that stays suspended,
	// including the one that waited for a slot, is paused and marked.
	for _, id := range f.hogSandboxes {
		waitState(t, f.hog.OwnerClient, f.hog.ID, id, ownsandv1.SandboxState_SANDBOX_STATE_PAUSED)
		if !pausedByBilling(t, f.env, id) {
			t.Fatalf("sandbox %s of the suspended organization has no billing mark", id)
		}
	}
}

// TestBillingResumeDroppedAfterSuspensionWhileQueued is the opposite
// direction: both organizations recovered, their billing-paused sandboxes
// are selected for a resume, and one organization is suspended again while
// its sandbox waits for a slot. The pass must not resume that sandbox, and
// it keeps its mark so the next recovery resumes it; the other sandboxes
// resume and keep theirs.
func TestBillingResumeDroppedAfterSuspensionWhileQueued(t *testing.T) {
	t.Parallel()
	f := newStaleFixture(t)
	c := f.victim.OwnerClient
	// First suspension: every sandbox is paused and marked.
	f.setPhase(t, f.hog, billing.PhaseSuspended)
	f.setPhase(t, f.victim, billing.PhaseSuspended)
	enforceBilling(t, f.env)
	for _, id := range append([]string{f.victimSandbox}, f.hogSandboxes...) {
		org := f.hog
		if id == f.victimSandbox {
			org = f.victim
		}
		waitState(t, org.OwnerClient, org.ID, id, ownsandv1.SandboxState_SANDBOX_STATE_PAUSED)
		if !pausedByBilling(t, f.env, id) {
			t.Fatalf("sandbox %s was not marked", id)
		}
	}
	// The resumes of the recovered hog organization occupy the available
	// ordering locks and keep the victim queued.
	hold := holdSlots(f.hogAgent, f.replyLimit, func(cmd *ownsandv1.AgentCommand) bool { return cmd.GetResumeSandbox() != nil })
	f.setPhase(t, f.hog, billing.PhaseAllowed)
	f.setPhase(t, f.victim, billing.PhaseAllowed)

	targets := f.startPass(t, "resume")
	if !slices.Contains(targets.resume, f.victimSandbox) {
		t.Fatalf("resume target list %v is missing sandbox %s", targets.resume, f.victimSandbox)
	}
	f.waitForHeldReplies(t, hold)
	f.setPhase(t, f.victim, billing.PhaseSuspended)
	hold.Release()
	f.finishPass(t)

	if got := resumeCommands(f.victimAgent, f.victimSandbox); got != 0 {
		t.Fatalf("resume commands for the re-suspended organization's sandbox = %d, want 0", got)
	}
	if s := getSandbox(t, c, f.victim.ID, f.victimSandbox); s.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_PAUSED {
		t.Fatalf("the re-suspended organization's sandbox is %v, want PAUSED", s.GetState())
	}
	if !pausedByBilling(t, f.env, f.victimSandbox) {
		t.Fatal("the re-suspended organization's sandbox lost its billing mark")
	}
	// Stable status: the organization that stayed recovered resumes every
	// sandbox, including the one that waited for a slot, and billing keeps
	// its mark (durable pause ownership).
	for _, id := range f.hogSandboxes {
		waitState(t, f.hog.OwnerClient, f.hog.ID, id, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
		if !pausedByBilling(t, f.env, id) {
			t.Fatalf("the resumed sandbox %s lost its billing mark", id)
		}
	}
	// The next pass does not resume the still-suspended organization's
	// sandbox either.
	enforceBilling(t, f.env)
	if got := resumeCommands(f.victimAgent, f.victimSandbox); got != 0 {
		t.Fatalf("resume commands after the next pass = %d, want 0", got)
	}
}
