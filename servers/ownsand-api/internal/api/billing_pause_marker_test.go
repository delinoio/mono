// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/api"
	"github.com/delinoio/mono/servers/ownsand-api/internal/app"
	"github.com/delinoio/mono/servers/ownsand-api/internal/billing"
	"github.com/delinoio/mono/servers/ownsand-api/internal/db"
	"github.com/delinoio/mono/servers/ownsand-api/internal/db/dbtest"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// The tests in this file cover the pause marker that billing sets on a
// sandbox it paused (docs/servers-ownsand-api-foundation.md#suspension-and-resume):
// a caller's successful PauseSandbox removes it, also when the sandbox is
// already PAUSED, so a confirmed recovery never resumes a sandbox that a
// caller paused. They run against a fake agent and a fake delino-accounts,
// not against a real device or a deployed billing account.

// setOrgPhase commits the billing phase of an organization the way the
// status worker stores it (SUSPENDED after 4 days of non-payment, or
// ALLOWED). The test writes the cache row directly, because a status change
// that the control plane applies also kicks a background enforcement pass,
// which would race with the pass under test.
func setOrgPhase(t *testing.T, env *testenv.Env, org *testenv.Org, phase ownsandv1.BillingPhase) {
	t.Helper()
	var err error
	switch phase {
	case billing.PhaseSuspended:
		since := env.Clock.Now().Add(-4 * 24 * time.Hour)
		_, err = env.DB.Pool.Exec(context.Background(), `UPDATE organization_billing SET phase = $2, reason = $3, blocked_since = $4, suspend_time = $5
			WHERE organization_id = $1`, org.ID, int16(phase), int16(ownsandv1.BillingReason_BILLING_REASON_SUSPENDED), since, since.Add(billing.SuspendAfter))
	default:
		_, err = env.DB.Pool.Exec(context.Background(), `UPDATE organization_billing SET phase = $2, reason = $3, blocked_since = NULL, suspend_time = NULL
			WHERE organization_id = $1`, org.ID, int16(phase), int16(ownsandv1.BillingReason_BILLING_REASON_UNSPECIFIED))
	}
	if err != nil {
		t.Fatalf("set billing phase: %v", err)
	}
}

// pauseRPC calls PauseSandbox with the caller's clients.
func pauseRPC(c *testenv.Clients, orgID, sandboxID string) (*ownsandv1.Sandbox, error) {
	return pauseRPCWithContext(context.Background(), c, orgID, sandboxID)
}

func pauseRPCWithContext(ctx context.Context, c *testenv.Clients, orgID, sandboxID string) (*ownsandv1.Sandbox, error) {
	resp, err := c.Sandbox.PauseSandbox(ctx, connect.NewRequest(&ownsandv1.PauseSandboxRequest{OrganizationId: orgID, SandboxId: sandboxID}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetSandbox(), nil
}

// markerFixture is an organization on a big device with a fake agent and
// sandboxes that billing paused and marked.
type markerFixture struct {
	env   *testenv.Env
	org   *testenv.Org
	dev   *testenv.Device
	agent *testenv.FakeAgent
}

func newMarkerFixture(t *testing.T) *markerFixture {
	t.Helper()
	return newMarkerFixtureWith(t)
}

// newMarkerFixtureWith is newMarkerFixture with more control plane options.
func newMarkerFixtureWith(t *testing.T, extra ...testenv.Option) *markerFixture {
	t.Helper()
	env, _ := billingEnvWith(t, extra...)
	org := env.NewOrg("alice")
	big := &ownsandv1.DeviceLimits{VcpuCount: 64, MemoryMib: 65536, DiskGib: 1000, VcpuOvercommitPercent: 100}
	dev, agent := setupAgent(t, env, org, big)
	return &markerFixture{env: env, org: org, dev: dev, agent: agent}
}

func (f *markerFixture) create(t *testing.T) string {
	t.Helper()
	created, err := f.org.OwnerClient.Sandbox.CreateSandbox(context.Background(), connect.NewRequest(createReq(f.org.ID)))
	if err != nil {
		t.Fatal(err)
	}
	id := created.Msg.GetSandbox().GetSandboxId()
	waitState(t, f.org.OwnerClient, f.org.ID, id, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	return id
}

// suspendAll suspends the organization and runs the pass that pauses and
// marks every running sandbox.
func (f *markerFixture) suspendAll(t *testing.T, ids ...string) {
	t.Helper()
	setOrgPhase(t, f.env, f.org, billing.PhaseSuspended)
	enforceBilling(t, f.env)
	for _, id := range ids {
		waitState(t, f.org.OwnerClient, f.org.ID, id, ownsandv1.SandboxState_SANDBOX_STATE_PAUSED)
		if !pausedByBilling(t, f.env, id) {
			t.Fatalf("sandbox %s was not marked by the suspension", id)
		}
	}
}

// TestCallerPauseAdoptsBillingPause is the sequential case: a caller's
// PauseSandbox on a sandbox that billing already paused succeeds without a
// command, removes the billing mark, and a later recovery resumes only the
// sandbox that no caller paused.
func TestCallerPauseAdoptsBillingPause(t *testing.T) {
	t.Run("fixture_pool", func(t *testing.T) {
		testCallerPauseAdoptsBillingPause(t, 0)
	})
	t.Run("two_connections", func(t *testing.T) {
		databaseURL, err := url.Parse(dbtest.URL(t))
		if err != nil {
			t.Fatal("invalid test database URL")
		}
		if databaseURL.RawQuery != "" {
			databaseURL.RawQuery += "&"
		}
		databaseURL.RawQuery += "pool_max_conns=2"
		t.Setenv(dbtest.EnvTestDatabaseURL, databaseURL.String())
		testCallerPauseAdoptsBillingPause(t, 2)
	})
}

func testCallerPauseAdoptsBillingPause(t *testing.T, poolMaxConns int32) {
	t.Helper()
	f := newMarkerFixture(t)
	if got := f.env.DB.Pool.Config().MaxConns; poolMaxConns != 0 && got != poolMaxConns {
		t.Fatalf("pool maximum = %d, want %d", got, poolMaxConns)
	}
	adopted, control := f.create(t), f.create(t)
	f.suspendAll(t, adopted, control)
	pauses := pauseCommands(f.agent, adopted)
	if pauses != 1 {
		t.Fatalf("pause commands of the billing pause = %d, want 1", pauses)
	}

	// Billing stays SUSPENDED: the caller's pause is still allowed, and it
	// answers PAUSED, clears the mark and sends no command, also when it is
	// repeated.
	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		resp, err := f.org.OwnerClient.Sandbox.PauseSandbox(ctx, connect.NewRequest(&ownsandv1.PauseSandboxRequest{
			OrganizationId: f.org.ID,
			SandboxId:      adopted,
		}))
		cancel()
		if err != nil {
			t.Fatalf("PauseSandbox %d: %v", i, err)
		}
		sb := resp.Msg.GetSandbox()
		if sb.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_PAUSED {
			t.Fatalf("PauseSandbox %d state = %v", i, sb.GetState())
		}
		if pausedByBilling(t, f.env, adopted) {
			t.Fatalf("PauseSandbox %d left the billing mark", i)
		}
		if got := pauseCommands(f.agent, adopted); got != pauses {
			t.Fatalf("pause commands after PauseSandbox %d = %d, want %d", i, got, pauses)
		}
	}
	if !pausedByBilling(t, f.env, control) {
		t.Fatal("the pause of another sandbox cleared the control sandbox's mark")
	}

	// A pass during the suspension does not mark the adopted sandbox again
	// (it is not RUNNING), and the recovery resumes only the control.
	enforceBilling(t, f.env)
	if pausedByBilling(t, f.env, adopted) {
		t.Fatal("a pass marked the caller-paused sandbox")
	}
	setOrgPhase(t, f.env, f.org, billing.PhaseAllowed)
	enforceBilling(t, f.env)
	waitState(t, f.org.OwnerClient, f.org.ID, control, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	expectMarkRetained(t, f, control)
	if got := resumeCommands(f.agent, adopted); got != 0 {
		t.Fatalf("resume commands for the caller-paused sandbox = %d, want 0", got)
	}
	if s := getSandbox(t, f.org.OwnerClient, f.org.ID, adopted); s.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_PAUSED {
		t.Fatalf("the caller-paused sandbox is %v, want PAUSED", s.GetState())
	}
	if got := resumeCommands(f.agent, control); got != 1 {
		t.Fatalf("resume commands for the control sandbox = %d, want 1", got)
	}
}

// TestCallerPauseAdoptionByEveryCaller checks the adoption for a member and
// for an API key of the organization, and that a caller of another
// organization neither adopts nor learns anything.
func TestCallerPauseAdoptionByEveryCaller(t *testing.T) {
	f := newMarkerFixture(t)
	byMember, byKey, other := f.create(t), f.create(t), f.create(t)
	f.suspendAll(t, byMember, byKey, other)
	stranger := f.env.NewOrg("mallory")

	// Another organization's user and API key get not_found for the marked
	// sandbox, and the mark stays.
	for name, c := range map[string]*testenv.Clients{"user": stranger.OwnerClient, "api key": f.env.Clients(stranger.APIKey)} {
		before := pauseCommands(f.agent, byMember)
		_, err := pauseRPC(c, f.org.ID, byMember)
		if code := connect.CodeOf(err); code != connect.CodeNotFound && code != connect.CodePermissionDenied {
			t.Fatalf("%s of another organization: PauseSandbox = %v", name, err)
		}
		// The caller's own organization does not own the sandbox either.
		_, err = pauseRPC(c, stranger.ID, byMember)
		if connect.CodeOf(err) != connect.CodeNotFound {
			t.Fatalf("%s: PauseSandbox with its own organization = %v, want not_found", name, err)
		}
		if !pausedByBilling(t, f.env, byMember) || pauseCommands(f.agent, byMember) != before {
			t.Fatalf("%s changed the marked sandbox", name)
		}
	}

	if _, err := pauseRPC(f.env.User(f.org.Member), f.org.ID, byMember); err != nil {
		t.Fatalf("member PauseSandbox: %v", err)
	}
	if _, err := pauseRPC(f.env.Clients(f.org.APIKey), f.org.ID, byKey); err != nil {
		t.Fatalf("api key PauseSandbox: %v", err)
	}
	for id, want := range map[string]bool{byMember: false, byKey: false, other: true} {
		if got := pausedByBilling(t, f.env, id); got != want {
			t.Fatalf("mark of %s = %v, want %v", id, got, want)
		}
	}
}

// TestCallerPauseKeepsRunningPathAndErrors checks the unchanged paths: a
// caller pause of a RUNNING sandbox still clears a mark when it dispatches,
// the worker's own pause keeps its mark, and a sandbox that is not
// RUNNING or PAUSED keeps its error and its mark.
func TestCallerPauseKeepsRunningPathAndErrors(t *testing.T) {
	f := newMarkerFixture(t)
	running, viaWorker, deleting := f.create(t), f.create(t), f.create(t)
	ctx := context.Background()

	// A RUNNING sandbox with a stale mark: the caller's pause dispatches
	// and clears it.
	if _, err := f.env.DB.Pool.Exec(ctx, "UPDATE sandboxes SET paused_by_billing = true WHERE sandbox_id = $1", running); err != nil {
		t.Fatal(err)
	}
	if _, err := pauseRPC(f.org.OwnerClient, f.org.ID, running); err != nil {
		t.Fatal(err)
	}
	if pausedByBilling(t, f.env, running) || pauseCommands(f.agent, running) != 1 {
		t.Fatalf("running pause: marked=%v commands=%d", pausedByBilling(t, f.env, running), pauseCommands(f.agent, running))
	}
	// An already PAUSED, unmarked sandbox succeeds without a command.
	if _, err := pauseRPC(f.org.OwnerClient, f.org.ID, running); err != nil || pauseCommands(f.agent, running) != 1 {
		t.Fatalf("repeated pause: %v, commands=%d", err, pauseCommands(f.agent, running))
	}

	// The worker's pause retains the mark.
	setOrgPhase(t, f.env, f.org, billing.PhaseSuspended)
	enforceBilling(t, f.env)
	waitState(t, f.org.OwnerClient, f.org.ID, viaWorker, ownsandv1.SandboxState_SANDBOX_STATE_PAUSED)
	if !pausedByBilling(t, f.env, viaWorker) {
		t.Fatal("the worker's pause did not keep its mark")
	}

	// A sandbox with a pending delete is DELETING: failed_precondition, and
	// the mark stays.
	if _, err := f.env.DB.Pool.Exec(ctx, "UPDATE sandboxes SET delete_pending = true WHERE sandbox_id = $1", deleting); err != nil {
		t.Fatal(err)
	}
	if _, err := pauseRPC(f.org.OwnerClient, f.org.ID, deleting); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("PauseSandbox of a deleting sandbox = %v, want failed_precondition", err)
	}
	if !pausedByBilling(t, f.env, deleting) {
		t.Fatal("a failed pause cleared the mark")
	}

	// An UNREACHABLE sandbox (its device went offline) keeps its error and
	// its mark.
	f.agent.Close()
	testenv.Eventually(t, "the device is offline", func() bool {
		return getSandbox(t, f.org.OwnerClient, f.org.ID, viaWorker).GetState() == ownsandv1.SandboxState_SANDBOX_STATE_UNREACHABLE
	})
	if _, err := pauseRPC(f.org.OwnerClient, f.org.ID, viaWorker); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("PauseSandbox of an unreachable sandbox = %v, want unavailable", err)
	}
	if !pausedByBilling(t, f.env, viaWorker) {
		t.Fatal("an unavailable pause cleared the mark")
	}
}

// TestCallerPauseFailsWhenTheMarkCannotBeCleared installs a trigger that
// rejects the clearing of one sandbox's mark: the pause must return
// internal, leave the mark and send no command, on the already-PAUSED path
// and on the dispatching path.
func TestCallerPauseFailsWhenTheMarkCannotBeCleared(t *testing.T) {
	f := newMarkerFixture(t)
	paused, running := f.create(t), f.create(t)
	ctx := context.Background()
	f.suspendAll(t, paused, running)
	// Make the second sandbox RUNNING with its mark, as it is between the
	// worker's mark and the pause command.
	f.agent.SendUpdate(f.agent.SetSandbox(running, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING, 0))
	waitState(t, f.org.OwnerClient, f.org.ID, running, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)

	for _, stmt := range []string{
		`CREATE FUNCTION reject_mark_clear() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN
				IF OLD.paused_by_billing AND NOT NEW.paused_by_billing THEN
					RAISE EXCEPTION 'test: the billing mark cannot be cleared';
				END IF;
				RETURN NEW;
			END $$`,
		`CREATE TRIGGER reject_mark_clear BEFORE UPDATE OF paused_by_billing ON sandboxes FOR EACH ROW EXECUTE FUNCTION reject_mark_clear()`,
	} {
		if _, err := f.env.DB.Pool.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = f.env.DB.Pool.Exec(context.Background(), "DROP TRIGGER IF EXISTS reject_mark_clear ON sandboxes")
	})

	for name, id := range map[string]string{"already paused": paused, "running": running} {
		before := pauseCommands(f.agent, id)
		if _, err := pauseRPC(f.org.OwnerClient, f.org.ID, id); connect.CodeOf(err) != connect.CodeInternal {
			t.Fatalf("PauseSandbox (%s) = %v, want internal", name, err)
		}
		if !pausedByBilling(t, f.env, id) {
			t.Fatalf("PauseSandbox (%s) changed the mark", name)
		}
		if got := pauseCommands(f.agent, id); got != before {
			t.Fatalf("PauseSandbox (%s) sent %d commands, want %d", name, got, before)
		}
	}
}

// resumeHold withholds the result of the first resume command for one
// sandbox and answers every other command at once.
type resumeHold struct {
	agent    *testenv.FakeAgent
	sandbox  string
	received chan struct{}

	mu       sync.Mutex
	held     *ownsandv1.AgentCommand
	released bool
}

func holdResume(agent *testenv.FakeAgent, sandboxID string) *resumeHold {
	h := &resumeHold{agent: agent, sandbox: sandboxID, received: make(chan struct{})}
	agent.SetOnCommand(func(a *testenv.FakeAgent, cmd *ownsandv1.AgentCommand) *ownsandv1.AgentCommandResult {
		if cmd.GetResumeSandbox().GetSandboxId() == h.sandbox {
			h.mu.Lock()
			defer h.mu.Unlock()
			if !h.released && h.held == nil {
				h.held = cmd
				close(h.received)
				return nil
			}
		}
		return a.DefaultAnswer(cmd)
	})
	return h
}

// Release answers the withheld resume as the agent would have: it resumes
// the sandbox and sends the result.
func (h *resumeHold) Release() {
	h.mu.Lock()
	cmd := h.held
	h.held = nil
	h.released = true
	h.mu.Unlock()
	if cmd == nil {
		return
	}
	h.sendResult(cmd.GetCommandId(), h.agent.DefaultAnswer(cmd))
}

func (h *resumeHold) command() *ownsandv1.AgentCommand {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.held
}

func (h *resumeHold) sendResult(id string, res *ownsandv1.AgentCommandResult) {
	res.CommandId = id
	h.agent.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_CommandResult{CommandResult: res}})
}

// TestCallerPauseCancelsSelectedResumeAcrossInstances selects the resume
// targets of a confirmed recovery, keeps every slot of the pass busy so the
// target waits between its selection and its dispatch, and lets a caller
// adopt the pause through a second API instance. The selected recovery must
// then send no resume, while the billing-only paused control sandbox
// resumes.
func TestCallerPauseCancelsSelectedResumeAcrossInstances(t *testing.T) {
	f := newMarkerFixture(t)
	// The sandbox IDs are time ordered and the pass starts its targets in ID
	// order, so the hogs take the 8 slots and the adopted sandbox and the
	// control wait for a slot.
	var hogs []string
	for i := 0; i < billingEnforceSlots; i++ {
		hogs = append(hogs, f.create(t))
	}
	adopted, control := f.create(t), f.create(t)
	f.suspendAll(t, append(append([]string{}, hogs...), adopted, control)...)

	hold := holdSlots(f.agent, billingEnforceSlots, func(cmd *ownsandv1.AgentCommand) bool {
		for _, id := range hogs {
			if cmd.GetResumeSandbox().GetSandboxId() == id {
				return true
			}
		}
		return false
	})
	setOrgPhase(t, f.env, f.org, billing.PhaseAllowed)
	passDone := make(chan error, 1)
	go func() { passDone <- f.env.App.Enforcer.Pass(context.Background()) }()
	testenv.Eventually(t, "every slot of the pass holds an unanswered resume", func() bool { return hold.heldCount() == billingEnforceSlots })

	// The caller adopts the pause through the other instance while billing
	// stays ALLOWED and the resume of the adopted sandbox is selected.
	peer := f.env.StartPeer()
	sb, err := pauseRPC(peer.User(f.org.Owner), f.org.ID, adopted)
	if err != nil {
		t.Fatalf("PauseSandbox through the second instance: %v", err)
	}
	if sb.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_PAUSED || pausedByBilling(t, f.env, adopted) {
		t.Fatalf("adoption: state=%v marked=%v", sb.GetState(), pausedByBilling(t, f.env, adopted))
	}
	hold.Release()
	select {
	case err := <-passDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the enforcement pass did not finish")
	}

	if got := resumeCommands(f.agent, adopted); got != 0 {
		t.Fatalf("resume commands for the adopted sandbox = %d, want 0", got)
	}
	if s := getSandbox(t, f.org.OwnerClient, f.org.ID, adopted); s.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_PAUSED {
		t.Fatalf("the adopted sandbox is %v, want PAUSED", s.GetState())
	}
	waitState(t, f.org.OwnerClient, f.org.ID, control, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	expectMarkRetained(t, f, control)
	// A later pass does not resume the adopted sandbox either.
	enforceBilling(t, f.env)
	if got := resumeCommands(f.agent, adopted); got != 0 || pausedByBilling(t, f.env, adopted) {
		t.Fatalf("after the next pass: resume commands=%d marked=%v", got, pausedByBilling(t, f.env, adopted))
	}
}

// expectMarkRetained checks that a sandbox that billing resumed keeps its
// mark (durable billing pause ownership) and that a further pass sends it no
// command.
func expectMarkRetained(t *testing.T, f *markerFixture, sandboxID string) {
	t.Helper()
	if !pausedByBilling(t, f.env, sandboxID) {
		t.Fatalf("the resumed sandbox %s lost its billing mark", sandboxID)
	}
	resumes, pauses := resumeCommands(f.agent, sandboxID), pauseCommands(f.agent, sandboxID)
	enforceBilling(t, f.env)
	if !pausedByBilling(t, f.env, sandboxID) || resumeCommands(f.agent, sandboxID) != resumes || pauseCommands(f.agent, sandboxID) != pauses {
		t.Fatalf("a pass changed the marked RUNNING sandbox %s", sandboxID)
	}
}

func newResumeInFlightFixture(t *testing.T) *markerFixture {
	t.Helper()
	return newMarkerFixtureWith(t, func(o *app.Options) {
		// Keep the withheld command from timing out during the lock observation.
		o.Timing.CommandWait = 3 * sandboxOrderObservationTimeout
	})
}

// startResumeInFlight suspends the organization, then recovers it and runs a
// pass in which the agent withholds the result of the resume of the returned
// adopted sandbox, until the resume is sent. The control sandbox is a second
// billing-paused sandbox that the pass resumes at once.
func startResumeInFlight(t *testing.T, f *markerFixture) (adopted, control string, hold *resumeHold, passDone chan error) {
	t.Helper()
	adopted, control = f.create(t), f.create(t)
	f.suspendAll(t, adopted, control)
	hold = holdResume(f.agent, adopted)
	t.Cleanup(hold.Release)
	setOrgPhase(t, f.env, f.org, billing.PhaseAllowed)
	passDone = make(chan error, 1)
	go func() { passDone <- f.env.App.Enforcer.Pass(t.Context()) }()
	select {
	case <-hold.received:
	case err := <-passDone:
		t.Fatalf("the enforcement pass finished before sending the held resume: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the resume of the adopted sandbox was not sent")
	}
	return adopted, control, hold, passDone
}

func waitPass(t *testing.T, passDone chan error) {
	t.Helper()
	select {
	case err := <-passDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the enforcement pass did not finish")
	}
}

type pauseOutcome struct {
	sb  *ownsandv1.Sandbox
	err error
}

func pauseAsync(ctx context.Context, c *testenv.Clients, orgID, sandboxID string) chan pauseOutcome {
	out := make(chan pauseOutcome, 1)
	go func() {
		sb, err := pauseRPCWithContext(ctx, c, orgID, sandboxID)
		out <- pauseOutcome{sb, err}
	}()
	return out
}

func waitOutcome(t *testing.T, name string, out chan pauseOutcome) pauseOutcome {
	t.Helper()
	select {
	case o := <-out:
		return o
	case <-time.After(30 * time.Second):
		t.Fatalf("the pause %s did not finish after the resume result", name)
		return pauseOutcome{}
	}
}

// TestCallerPauseWaitsForResumeInFlight holds the agent's result of a
// resume that the worker already sent. A caller's pause must wait for the
// reconciliation instead of answering PAUSED from the stale row; because the
// sandbox actually resumed, it then pauses it through the normal pause
// path, and no recovery command can undo the pause afterwards.
func TestCallerPauseWaitsForResumeInFlight(t *testing.T) {
	f := newResumeInFlightFixture(t)
	adopted, control, hold, passDone := startResumeInFlight(t, f)

	holderPID := sandboxOrderLockHolder(t, f.env, adopted)
	out := pauseAsync(t.Context(), f.org.OwnerClient, f.org.ID, adopted)
	waitSandboxOrderBlocked(t, f.env, adopted, holderPID, "caller pause", out)
	hold.Release()
	o := waitOutcome(t, "", out)
	if o.err != nil {
		t.Fatalf("PauseSandbox: %v", o.err)
	}
	waitPass(t, passDone)

	if o.sb.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_PAUSED {
		t.Fatalf("PauseSandbox answered %v, want PAUSED", o.sb.GetState())
	}
	if got := resumeCommands(f.agent, adopted); got != 1 {
		t.Fatalf("resume commands = %d, want 1", got)
	}
	// The billing pause and the caller's pause after the actual resume.
	if got := pauseCommands(f.agent, adopted); got != 2 {
		t.Fatalf("pause commands = %d, want 2", got)
	}
	if pausedByBilling(t, f.env, adopted) {
		t.Fatal("the adopted sandbox kept its billing mark")
	}
	waitState(t, f.org.OwnerClient, f.org.ID, control, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	// Drain every later pass: the adopted sandbox stays paused.
	enforceBilling(t, f.env)
	if got := resumeCommands(f.agent, adopted); got != 1 {
		t.Fatalf("resume commands after the next pass = %d, want 1", got)
	}
	if s := getSandbox(t, f.org.OwnerClient, f.org.ID, adopted); s.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_PAUSED {
		t.Fatalf("the adopted sandbox is %v, want PAUSED", s.GetState())
	}
}

// TestCallerPauseThroughSecondInstanceWaitsForResumeInFlight is the same
// in-flight resume with the caller on an instance that does not hold the
// agent channel. Its pause waits for the resume's reconciliation, through the
// database lock. It cannot send the pause command for the RUNNING sandbox
// that the resume produced, so it reports unavailable and never a success
// that a resume undid; the retry on the instance with the channel pauses.
func TestCallerPauseThroughSecondInstanceWaitsForResumeInFlight(t *testing.T) {
	f := newResumeInFlightFixture(t)
	adopted, _, hold, passDone := startResumeInFlight(t, f)
	peer := f.env.StartPeer()

	holderPID := sandboxOrderLockHolder(t, f.env, adopted)
	out := pauseAsync(t.Context(), peer.User(f.org.Owner), f.org.ID, adopted)
	waitSandboxOrderBlocked(t, f.env, adopted, holderPID, "caller pause through the second instance", out)
	hold.Release()
	o := waitOutcome(t, "through the second instance", out)
	if connect.CodeOf(o.err) != connect.CodeUnavailable {
		t.Fatalf("PauseSandbox through the second instance = %v, %v, want unavailable", o.sb.GetState(), o.err)
	}
	waitPass(t, passDone)
	if s := getSandbox(t, f.org.OwnerClient, f.org.ID, adopted); s.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		t.Fatalf("the resumed sandbox is %v, want RUNNING", s.GetState())
	}
	// The failed pause did not take ownership: billing keeps its mark of the
	// resumed sandbox.
	if !pausedByBilling(t, f.env, adopted) {
		t.Fatal("the failed PauseSandbox cleared the billing mark")
	}
	sb, err := pauseRPC(f.org.OwnerClient, f.org.ID, adopted)
	if err != nil || sb.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_PAUSED {
		t.Fatalf("retried PauseSandbox = %v, %v", sb.GetState(), err)
	}
	if pausedByBilling(t, f.env, adopted) {
		t.Fatal("the retried PauseSandbox left the billing mark")
	}
}

// TestCallerPauseSurvivesControlPlaneReplacement adopts the pause of a
// billing-paused sandbox, replaces the control plane instance (a lease
// holder replacement: the new instance has no memory of the adoption) and
// recovers billing. The adopted sandbox stays paused, and the control
// sandbox that only billing paused resumes.
func TestCallerPauseSurvivesControlPlaneReplacement(t *testing.T) {
	f := newMarkerFixture(t)
	adopted, control := f.create(t), f.create(t)
	f.suspendAll(t, adopted, control)
	if _, err := pauseRPC(f.env.StartPeer().User(f.org.Owner), f.org.ID, adopted); err != nil {
		t.Fatal(err)
	}

	f.env.Restart()
	select {
	case <-f.agent.Closed():
	case <-time.After(10 * time.Second):
		t.Fatal("the channel stayed open after the control plane restarted")
	}
	f.agent.Connect()
	c := f.env.User(f.org.Owner)
	waitState(t, c, f.org.ID, adopted, ownsandv1.SandboxState_SANDBOX_STATE_PAUSED)
	waitState(t, c, f.org.ID, control, ownsandv1.SandboxState_SANDBOX_STATE_PAUSED)
	setOrgPhase(t, f.env, f.org, billing.PhaseAllowed)
	enforceBilling(t, f.env)
	waitState(t, c, f.org.ID, control, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	expectMarkRetained(t, f, control)
	enforceBilling(t, f.env)
	if got := resumeCommands(f.agent, adopted); got != 0 {
		t.Fatalf("resume commands for the adopted sandbox = %d, want 0", got)
	}
	if s := getSandbox(t, c, f.org.ID, adopted); s.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_PAUSED {
		t.Fatalf("the adopted sandbox is %v, want PAUSED", s.GetState())
	}
}

func TestCallerPauseRejectsUnresolvedBillingResume(t *testing.T) {
	for _, test := range []struct {
		name    string
		restart bool
		fence   bool
	}{
		{name: "late result"},
		{name: "legacy agent after API restart", restart: true},
		{name: "lost result after API restart", restart: true, fence: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newMarkerFixtureWith(t, func(o *app.Options) {
				timing := api.DefaultTiming()
				timing.CommandWait = 300 * time.Millisecond
				o.Timing = &timing
			})
			adopted, control, hold, passDone := startResumeInFlight(t, f)
			waitPass(t, passDone)
			waitState(t, f.org.OwnerClient, f.org.ID, control, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
			peer := f.env.StartPeer()
			pauses := pauseCommands(f.agent, adopted)
			assertUnresolved := func(c *testenv.Clients) {
				t.Helper()
				if _, err := pauseRPC(c, f.org.ID, adopted); connect.CodeOf(err) != connect.CodeUnavailable || !strings.Contains(err.Error(), "automatic billing resume") {
					t.Fatalf("PauseSandbox = %v, want unresolved billing resume", err)
				}
				if !pausedByBilling(t, f.env, adopted) || pauseCommands(f.agent, adopted) != pauses {
					t.Fatal("the unresolved pause cleared the billing mark or sent a pause")
				}
			}
			assertUnresolved(f.org.OwnerClient)
			assertUnresolved(peer.User(f.org.Owner))

			if test.restart {
				f.env.Restart()
				select {
				case <-f.agent.Closed():
				case <-time.After(10 * time.Second):
					t.Fatal("the original channel is still open")
				}
				f.agent.ResumeFenceSupported = test.fence
				f.agent.Connect()
				// The replacement channel's PAUSED report is not evidence
				// that the earlier resume finished.
				assertUnresolved(f.env.User(f.org.Owner))
			}

			if test.fence {
				fenceReceived := make(chan *ownsandv1.AgentCommand, 1)
				f.agent.SetOnCommand(func(a *testenv.FakeAgent, cmd *ownsandv1.AgentCommand) *ownsandv1.AgentCommandResult {
					if cmd.GetFenceResume().GetSandboxId() == adopted {
						fenceReceived <- cmd
						return nil
					}
					return a.DefaultAnswer(cmd)
				})
				enforceBilling(t, f.env)
				var fence *ownsandv1.AgentCommand
				select {
				case fence = <-fenceReceived:
				case <-time.After(10 * time.Second):
					t.Fatal("the replacement API did not send the fence")
				}
				if got := fence.GetFenceResume().GetResumeCommandId(); got != hold.command().GetCommandId() {
					t.Fatalf("fence refers to %s, want %s", got, hold.command().GetCommandId())
				}
				assertUnresolved(f.env.User(f.org.Owner))
				// Apply the resume on the device and drop its original
				// result. The fence supplies the only RUNNING entry.
				entry := f.agent.DefaultAnswer(hold.command()).GetSandbox().GetSandbox()
				hold.sendResult(fence.GetCommandId(), &ownsandv1.AgentCommandResult{
					Result: &ownsandv1.AgentCommandResult_FenceResume{FenceResume: &ownsandv1.FenceResumeResult{
						ResumeCommandId: fence.GetFenceResume().GetResumeCommandId(), Sandbox: entry,
					}},
				})
			} else {
				enforceBilling(t, f.env)
				assertUnresolved(f.env.User(f.org.Owner))
				for _, cmd := range f.agent.Commands() {
					if cmd.GetFenceResume() != nil {
						t.Fatal("the API sent a fence to a legacy agent")
					}
				}
				hold.Release()
			}

			c := f.env.User(f.org.Owner)
			waitState(t, c, f.org.ID, adopted, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
			sb, err := pauseRPC(c, f.org.ID, adopted)
			if err != nil || sb.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_PAUSED {
				t.Fatalf("retried PauseSandbox = %v, %v", sb.GetState(), err)
			}
			enforceBilling(t, f.env)
			if n := resumeCommands(f.agent, adopted); n != 1 {
				t.Fatalf("automatic resumes for the adopted sandbox = %d, want 1", n)
			}
			if f.agent.Sandbox(adopted).GetState() != ownsandv1.SandboxState_SANDBOX_STATE_PAUSED ||
				getSandbox(t, c, f.org.ID, adopted).GetState() != ownsandv1.SandboxState_SANDBOX_STATE_PAUSED ||
				pausedByBilling(t, f.env, adopted) {
				t.Fatal("the adopted sandbox did not stay PAUSED with its billing mark cleared")
			}
		})
	}
}

func TestBillingResumeQueueRejectionClearsAttempt(t *testing.T) {
	f := newMarkerFixture(t)
	id := f.create(t)
	f.suspendAll(t, id)
	setOrgPhase(t, f.env, f.org, billing.PhaseAllowed)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Hold the row so the resume selects a live channel, but has not
	// committed its tracking record or called Send yet.
	lock, err := f.env.DB.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	tx, err := lock.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, "SELECT 1 FROM sandboxes WHERE sandbox_id = $1 FOR UPDATE", id); err != nil {
		t.Fatal(err)
	}
	passDone := make(chan error, 1)
	go func() { passDone <- f.env.App.Enforcer.Pass(ctx) }()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		var blocked bool
		if err := f.env.DB.Pool.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity WHERE datname = current_database()
			AND query LIKE 'UPDATE sandboxes s SET billing_resume_command_id =%'
			AND $1 = ANY(pg_blocking_pids(pid)))`, int32(lock.Conn().PgConn().PID())).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case err := <-passDone:
			t.Fatalf("the billing pass returned before persisting the attempt: %v", err)
		case <-ctx.Done():
			t.Fatal("the resume did not wait for the sandbox row")
		case <-tick.C:
		}
	}

	f.env.App.Service.Hub.Live(f.dev.ID).Close(errors.New("test closed the resume channel before queueing"))
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	waitPass(t, passDone)
	var pending bool
	if err := f.env.DB.Pool.QueryRow(ctx, "SELECT billing_resume_command_id IS NOT NULL FROM sandboxes WHERE sandbox_id = $1", id).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending || !pausedByBilling(t, f.env, id) || resumeCommands(f.agent, id) != 0 {
		t.Fatalf("rejected resume changed ownership, sent a command or left a pending attempt (%v)", pending)
	}
	select {
	case <-f.agent.Closed():
	case <-ctx.Done():
		t.Fatal("the original agent channel is still open")
	}
	f.agent.Connect()
	c := f.env.User(f.org.Owner)
	enforceBilling(t, f.env)
	waitState(t, c, f.org.ID, id, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
}

const sandboxOrderLockTestKey = "hashtextextended('ownsand.sandbox-order:' || $1::text, 0)"
const sandboxOrderObservationTimeout = 10 * time.Second

// PostgreSQL stores the high and low halves of a bigint advisory key in
// classid and objid. objsubid distinguishes it from the two-integer key form.
const sandboxOrderTestLocks = `
WITH target_locks AS (
    SELECT pid, granted
    FROM pg_locks, (SELECT ` + sandboxOrderLockTestKey + ` AS key) AS target
    WHERE locktype = 'advisory'
      AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
      AND classid::bigint = ((target.key >> 32) & 4294967295)
      AND objid::bigint = (target.key & 4294967295)
      AND objsubid = 1
      AND mode = 'ExclusiveLock'
)
`

// sandboxOrderLockHolder identifies the worker's backend after the fake agent
// receives the target resume and before a caller can try to take the lock.
func sandboxOrderLockHolder(t *testing.T, env *testenv.Env, sandboxID string) int32 {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), sandboxOrderObservationTimeout)
	defer cancel()
	var pid int32
	if err := env.DB.Pool.QueryRow(ctx, sandboxOrderTestLocks+"SELECT pid FROM target_locks WHERE granted", sandboxID).Scan(&pid); err != nil {
		t.Fatalf("find the billing resume's ordering lock holder for sandbox %s: %v", sandboxID, err)
	}
	return pid
}

// waitSandboxOrderBlocked observes the PostgreSQL wait from a separate
// connection. Each phase starts only one operation that can wait for this key.
func waitSandboxOrderBlocked[T any](t *testing.T, env *testenv.Env, sandboxID string, holderPID int32, operation string, done <-chan T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), sandboxOrderObservationTimeout)
	defer cancel()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()

	const query = sandboxOrderTestLocks + `
SELECT pid FROM target_locks
WHERE NOT granted AND $2 = ANY(pg_blocking_pids(pid))
  AND EXISTS (SELECT 1 FROM target_locks WHERE granted AND pid = $2)
`
	for ctx.Err() == nil {
		select {
		case outcome := <-done:
			t.Fatalf("%s finished before waiting on the ordering lock for sandbox %s: %+v", operation, sandboxID, outcome)
		case <-ctx.Done():
			continue
		case <-tick.C:
		}

		var waiterPID int32
		err := env.DB.Pool.QueryRow(ctx, query, sandboxID, holderPID).Scan(&waiterPID)
		switch {
		case err == nil:
			t.Logf("%s waits on the ordering lock for sandbox %s (backend %d blocked by %d)", operation, sandboxID, waiterPID, holderPID)
			return
		case ctx.Err() == nil && !errors.Is(err, pgx.ErrNoRows):
			t.Fatalf("observe the ordering lock wait for %s on sandbox %s: %v", operation, sandboxID, err)
		}
	}
	t.Fatalf("timed out waiting for %s to block on the ordering lock for sandbox %s (holder %d): %v", operation, sandboxID, holderPID, ctx.Err())
}

// holdSandboxOrderLock takes the Postgres advisory lock that serializes the
// caller pause and the billing worker of one sandbox, as another API
// instance in the middle of its own critical section would hold it, and
// returns the holder PID and the function that releases it.
func holdSandboxOrderLock(t *testing.T, env *testenv.Env, sandboxID string) (int32, func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), sandboxOrderObservationTimeout)
	defer cancel()
	conn, err := env.DB.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock("+sandboxOrderLockTestKey+")", sandboxID); err != nil {
		// PostgreSQL may have granted the lock before the query failed.
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = conn.Hijack().Close(closeCtx)
		t.Fatal(err)
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var unlocked bool
			if err := conn.QueryRow(ctx, "SELECT pg_advisory_unlock("+sandboxOrderLockTestKey+")", sandboxID).Scan(&unlocked); err != nil || !unlocked {
				closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer closeCancel()
				_ = conn.Hijack().Close(closeCtx)
				t.Errorf("release the sandbox ordering lock: unlocked=%v err=%v", unlocked, err)
				return
			}
			conn.Release()
		})
	}
	t.Cleanup(release)
	return int32(conn.Conn().PgConn().PID()), release
}

func TestSandboxOrderLockQueryFailureClosesConnection(t *testing.T) {
	const (
		childEnv   = "OWNSAND_TEST_SANDBOX_ORDER_LOCK_FAILURE"
		poolClosed = "sandbox ordering lock fixture pool closed"
		sandboxID  = "lock-query-failure"
	)
	if os.Getenv(childEnv) == "1" {
		config, err := pgxpool.ParseConfig(dbtest.URL(t))
		if err != nil {
			t.Fatal("parse sandbox ordering lock test database configuration")
		}
		config.ConnConfig.RuntimeParams["lock_timeout"] = "100ms"
		var checkout *pgx.Conn
		config.PrepareConn = func(_ context.Context, conn *pgx.Conn) (bool, error) {
			checkout = conn
			return true, nil
		}
		pool, err := pgxpool.NewWithConfig(t.Context(), config)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			sessionClosed := checkout != nil && checkout.IsClosed()
			pool.Close()
			if !sessionClosed {
				t.Error("the failed lock query left its session open")
				return
			}
			t.Log(poolClosed)
		})
		holdSandboxOrderLock(t, &testenv.Env{DB: &db.DB{Pool: pool}}, sandboxID)
		t.Fatal("the sandbox ordering lock query succeeded while another session held the lock")
	}

	// The parent owns the database and holding session so it can clean up even
	// if the child leaks its pool checkout and hangs during teardown.
	databaseURL := dbtest.NewURL(t)
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal("parse sandbox ordering lock test database configuration")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	holder, err := pgx.ConnectConfig(ctx, config.ConnConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Close(ctx) }()
	if _, err := holder.Exec(ctx, "SELECT pg_advisory_lock("+sandboxOrderLockTestKey+")", sandboxID); err != nil {
		t.Fatal(err)
	}

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, executable,
		"-test.run=^TestSandboxOrderLockQueryFailureClosesConnection$", "-test.v", "-test.timeout=30s")
	cmd.Env = append(cmd.Environ(), childEnv+"=1", dbtest.EnvTestDatabaseURL+"="+databaseURL)
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("lock query subprocess timed out: %s", output)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("lock query subprocess should fail with exit code 1, got %v: %s", err, output)
	}
	for _, want := range []string{"SQLSTATE 55P03", poolClosed} {
		if !strings.Contains(string(output), want) {
			t.Fatalf("lock query subprocess output is missing %q: %s", want, output)
		}
	}
}

func TestSandboxOrderLockQuerySuccessReleasesConnection(t *testing.T) {
	database := dbtest.New(t)
	env := &testenv.Env{DB: database}
	const sandboxID = "lock-query-success"

	observer, err := database.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(observer.Release)
	assertLockAvailable := func(t *testing.T, want bool) {
		t.Helper()
		var available bool
		err := observer.QueryRow(t.Context(), "SELECT pg_try_advisory_xact_lock("+sandboxOrderLockTestKey+")", sandboxID).Scan(&available)
		if err != nil || available != want {
			t.Fatalf("sandbox ordering lock available = %v, error = %v, want %v", available, err, want)
		}
	}
	t.Run("explicit release", func(t *testing.T) {
		_, release := holdSandboxOrderLock(t, env, sandboxID)
		assertLockAvailable(t, false)
		release()
		assertLockAvailable(t, true)
	})
	t.Run("test cleanup", func(t *testing.T) {
		holdSandboxOrderLock(t, env, sandboxID)
		assertLockAvailable(t, false)
	})
	assertLockAvailable(t, true)
}

// TestCallerPauseWaitsForTheSandboxOrderLock checks that the caller pause
// and the billing worker's resume use the shared database lock: while
// another holder has it, neither completes, and a resume that waited for it
// rechecks the mark afterwards and sends nothing when a caller pause cleared
// the mark in the meantime.
func TestCallerPauseWaitsForTheSandboxOrderLock(t *testing.T) {
	f := newMarkerFixture(t)
	adopted, control := f.create(t), f.create(t)
	f.suspendAll(t, adopted, control)

	holderPID, release := holdSandboxOrderLock(t, f.env, adopted)
	out := pauseAsync(t.Context(), f.org.OwnerClient, f.org.ID, adopted)
	waitSandboxOrderBlocked(t, f.env, adopted, holderPID, "caller pause", out)
	release()
	if o := waitOutcome(t, "after the lock was released", out); o.err != nil || o.sb.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_PAUSED {
		t.Fatalf("PauseSandbox = %v, %v", o.sb.GetState(), o.err)
	}
	if pausedByBilling(t, f.env, adopted) {
		t.Fatal("the pause left the billing mark")
	}

	// A resume that waits for the lock of a sandbox whose mark another
	// instance clears meanwhile sends nothing.
	if _, err := f.env.DB.Pool.Exec(context.Background(), "UPDATE sandboxes SET paused_by_billing = true WHERE sandbox_id = $1", adopted); err != nil {
		t.Fatal(err)
	}
	holderPID, release = holdSandboxOrderLock(t, f.env, adopted)
	setOrgPhase(t, f.env, f.org, billing.PhaseAllowed)
	passDone := make(chan error, 1)
	go func() { passDone <- f.env.App.Enforcer.Pass(t.Context()) }()
	waitSandboxOrderBlocked(t, f.env, adopted, holderPID, "billing resume", passDone)
	waitState(t, f.org.OwnerClient, f.org.ID, control, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	if _, err := f.env.DB.Pool.Exec(context.Background(), "UPDATE sandboxes SET paused_by_billing = false WHERE sandbox_id = $1", adopted); err != nil {
		t.Fatal(err)
	}
	release()
	waitPass(t, passDone)
	if got := resumeCommands(f.agent, adopted); got != 0 {
		t.Fatalf("resume commands for the sandbox whose mark was cleared = %d, want 0", got)
	}
}
