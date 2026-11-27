// SPDX-License-Identifier: FSL-1.1-ALv2

package layers_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/servers/sandrun-api/internal/agent"
	"github.com/delinoio/mono/servers/sandrun-api/internal/agent/devin"
	"github.com/delinoio/mono/servers/sandrun-api/internal/agent/devin/devinfake"
	"github.com/delinoio/mono/servers/sandrun-api/internal/app"
	"github.com/delinoio/mono/servers/sandrun-api/internal/layers"
	"github.com/delinoio/mono/servers/sandrun-api/internal/ownsandclient/ownsandfake"
	"github.com/delinoio/mono/servers/sandrun-api/internal/testenv"
	"github.com/delinoio/mono/servers/sandrun-api/internal/work"
)

const (
	devinAPIToken     = "apk_devin_service_user_token_0123"
	devinOutpostToken = "dot_devin_outpost_token_4567"
)

type devinWorld struct {
	*agentWorld
	devin     *devinfake.Fake
	profileID string
}

func newDevinWorld(t *testing.T, guest sandrunv1.GuestOs) *devinWorld {
	t.Helper()
	fake := devinfake.New(t, devinAPIToken, "sandrun-outpost")
	w := createDevinWorld(t, guest, fake)
	eventually(t, "the Devin watch", func() bool { return fake.Streams() == 1 })
	return w
}

func createDevinWorld(t *testing.T, guest sandrunv1.GuestOs, fake *devinfake.Fake, extra ...testenv.Option) *devinWorld {
	t.Helper()
	return createDevinWorldWithOptions(t, guest, fake,
		layers.Options{Devin: devin.Options{BaseURL: fake.Server.URL, ReleaseGrace: time.Minute}}, extra...)
}

func createDevinWorldWithOptions(t *testing.T, guest sandrunv1.GuestOs, fake *devinfake.Fake, opts layers.Options, extra ...testenv.Option) *devinWorld {
	t.Helper()
	w := newAgentWorld(t, opts, extra...)
	dw := &devinWorld{agentWorld: w, devin: fake}
	id := w.createIntegration(t, &sandrunv1.CreateAgentIntegrationRequest{
		Vendor: sandrunv1.AgentVendor_AGENT_VENDOR_DEVIN, Name: "devin",
		Settings: &sandrunv1.CreateAgentIntegrationRequest_Devin{Devin: &sandrunv1.DevinSettings{OutpostName: "sandrun-outpost"}},
		Credentials: &sandrunv1.CreateAgentIntegrationRequest_DevinCredentials{DevinCredentials: &sandrunv1.DevinCredentials{
			ApiToken: devinAPIToken, OutpostToken: devinOutpostToken}},
	})
	dw.profileID = w.createProfile(t, agentProfileProto("devin-agents", id, guest, time.Hour))
	return dw
}

func (w *devinWorld) full() {
	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) {
		o.CreateError = connect.NewWireError(connect.CodeUnavailable, errors.New("full"))
	})
}

func TestDevinOrganizationDeletionRecovery(t *testing.T) {
	w := newDevinWorld(t, sandrunv1.GuestOs_GUEST_OS_LINUX)
	w.full()
	w.devin.AddSession("ses-org", true)
	req := w.requestBySource(t, "ses-org")
	w.devin.FailReleases(true)
	ctx := context.Background()
	owner := w.env.User(w.org.Owner)
	if _, err := owner.Organization.DeleteOrganization(ctx, connect.NewRequest(&sandrunv1.DeleteOrganizationRequest{OrganizationId: w.org.ID})); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("DeleteOrganization error = %v, want internal", err)
	}
	if c, err := w.l.Agents.Claim(ctx, req.GetRequestId()); err != nil || c.State != agent.ClaimHeld {
		t.Fatalf("claim %+v %v", c, err)
	}
	if phase, acceptor := w.devin.State("ses-org"); phase != "claimed" || acceptor != "sandrun-"+req.GetRequestId() {
		t.Fatalf("queue entry %s %s", phase, acceptor)
	}
	eventually(t, "the Devin watch resumes after the failed release", func() bool { return w.devin.Streams() == 1 })
	w.devin.FailReleases(false)
	w.devin.AddSession("ses-after", true)
	w.requestBySource(t, "ses-after")
}

// TestDevinSessionLifecycle: a pending session is claimed with a SandRun
// acceptor ID; the runner gets only the outpost token (in its credential
// file), starts `devin worker start --session ... --acceptor-id ...`,
// becomes BUSY when the session runs, and its exit ends the request; SandRun
// releases the claim after the session terminated. The API token never
// reaches the guest.
func TestDevinSessionLifecycle(t *testing.T) {
	w := newDevinWorld(t, sandrunv1.GuestOs_GUEST_OS_MACOS)
	w.devin.AddSession("ses-1", true)
	req := w.requestBySource(t, "ses-1")
	phase, acceptor := w.devin.State("ses-1")
	if phase != "claimed" || acceptor != "sandrun-"+req.GetRequestId() {
		t.Fatalf("queue entry %s %s", phase, acceptor)
	}
	var runner *sandrunv1.Runner
	eventually(t, "runner waits for work", func() bool {
		r := w.request(t, req.GetRequestId())
		if r.GetRunnerId() == "" {
			return false
		}
		runner = w.runner(t, r.GetRunnerId())
		return runner.GetState() == sandrunv1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK
	})
	sandboxID := runner.GetOwnsandSandboxId()
	dir := agent.RunnerDir(sandrunv1.GuestOs_GUEST_OS_MACOS, runner.GetRunnerId())
	files := w.fake.Files(w.osOrg, sandboxID)
	if len(files) != 1 || string(files[dir+"/"+agent.CredentialFile]) != devinOutpostToken {
		t.Fatalf("guest files %q", files)
	}
	script := w.guest.startScript(sandboxID)
	want := "exec 'devin' 'worker' 'start' '--session=ses-1' '--outpost=outpost_env-0001' '--acceptor-id=" + acceptor + "' >>'" + dir + "/runner.log' 2>&1"
	if !strings.Contains(script, want) || !strings.Contains(script, "DEVIN_OUTPOSTS_TOKEN=$(cat '"+dir+"/credential')") {
		t.Fatalf("start script:\n%s", script)
	}
	if strings.Contains(script, "--once") {
		t.Fatal("the worker uses --once")
	}
	all := w.guest.all()
	if strings.Contains(all, devinAPIToken) || strings.Contains(all, devinOutpostToken) {
		t.Fatal("an exec command or environment carries a Devin credential")
	}
	if !strings.Contains(all, "https://cli.devin.ai/install.sh") {
		t.Fatal("no install script")
	}
	w.devin.SetSessionStatus("ses-1", "running")
	w.waitRunner(t, runner.GetRunnerId(), sandrunv1.RunnerState_RUNNER_STATE_BUSY)
	w.waitRequest(t, req.GetRequestId(), sandrunv1.RequestState_REQUEST_STATE_RUNNING)
	w.devin.SetEmptyLookup("ses-1", true)
	w.guest.exitProgram(sandboxID, 0)
	r := w.waitRequest(t, req.GetRequestId(), sandrunv1.RequestState_REQUEST_STATE_ENDED)
	if r.GetEndReason() != sandrunv1.RequestEndReason_REQUEST_END_REASON_COMPLETED {
		t.Fatalf("end reason %v", r.GetEndReason())
	}
	w.waitRunner(t, runner.GetRunnerId(), sandrunv1.RunnerState_RUNNER_STATE_DELETED)
	eventually(t, "the empty queue-entry lookup fails during release", func() bool {
		logs := w.env.Logs.String()
		return strings.Contains(logs, "agent_claim_release_failed") && strings.Contains(logs, "DEVIN get_queue_entry: decode response: empty body")
	})
	w.assertClaim(t, req.GetRequestId(), agent.ClaimHeld)
	w.assertNoRelease(t, "ses-1")
	w.devin.SetSessionStatusSilently("ses-1", "terminated")
	w.devin.SetEmptyLookup("ses-1", false)
	eventually(t, "claim released", func() bool { return w.devin.Releases("ses-1") == 1 })
	eventually(t, "the release is recorded", func() bool {
		cl, err := w.l.Agents.Claim(t.Context(), req.GetRequestId())
		return err == nil && cl.State == agent.ClaimReleased
	})
	if len(w.devin.Messages("ses-1")) != 0 {
		t.Fatal("operator message for a served session")
	}
	stays(t, "one request", func() bool { return len(w.requests(t)) == 1 })
	logs := w.env.Logs.String()
	if strings.Contains(logs, devinAPIToken) || strings.Contains(logs, devinOutpostToken) || strings.Contains(logs, "connect-token-secret") {
		t.Fatal("a credential reached the log")
	}
}

// TestDevinClaimRelease: at the maximum queue wait SandRun shows an operator
// message and releases the claim (the session returns to the queue and is
// new demand); after a user cancellation the same entry is not claimed
// again; an entry that another acceptor takes ends the queued request.
func TestDevinClaimRelease(t *testing.T) {
	w := newDevinWorld(t, sandrunv1.GuestOs_GUEST_OS_LINUX)
	w.full()
	w.devin.AddSession("ses-wait", true)
	waiting := w.requestBySource(t, "ses-wait")
	w.env.Clock.Advance(2 * time.Hour)
	w.waitRequest(t, waiting.GetRequestId(), sandrunv1.RequestState_REQUEST_STATE_EXPIRED)
	eventually(t, "release at max queue wait", func() bool { return w.devin.Releases("ses-wait") == 1 })
	if m := w.devin.Messages("ses-wait"); len(m) != 1 || !strings.Contains(m[0], "maximum queue wait") {
		t.Fatalf("operator messages %q", m)
	}
	// The released session is pending again: a new request takes it.
	again := w.requestBySource(t, "ses-wait#2")
	if again.GetState() != sandrunv1.RequestState_REQUEST_STATE_QUEUED {
		t.Fatalf("second request %v", again)
	}

	w.devin.AddSession("ses-cancel", true)
	canceled := w.requestBySource(t, "ses-cancel")
	w.cancel(t, canceled.GetRequestId())
	eventually(t, "release at cancel", func() bool { return w.devin.Releases("ses-cancel") == 1 })
	stays(t, "no request after the cancel", func() bool {
		for _, r := range w.requests(t) {
			if r.GetSourceKey() == "ses-cancel#2" {
				return false
			}
		}
		return true
	})

	w.devin.AddSession("ses-lost", true)
	lost := w.requestBySource(t, "ses-lost")
	w.devin.ExpireClaim("ses-lost")
	eventually(t, "claimed again", func() bool { return w.devin.Claims("ses-lost") == 2 })
	w.devin.ClaimElsewhere("ses-lost")
	r := w.waitRequest(t, lost.GetRequestId(), sandrunv1.RequestState_REQUEST_STATE_CANCELED)
	if r.GetEndReason() != sandrunv1.RequestEndReason_REQUEST_END_REASON_CANCELED_BY_SOURCE {
		t.Fatalf("end reason %v", r.GetEndReason())
	}
	if w.devin.Releases("ses-lost") != 0 {
		t.Fatal("SandRun released another acceptor's claim")
	}
}

func (w *devinWorld) warmRunner(t *testing.T) *sandrunv1.Runner {
	t.Helper()
	ctx := t.Context()
	_, err := w.env.User(w.org.Admin).Profile.UpdateRunnerProfile(ctx, connect.NewRequest(&sandrunv1.UpdateRunnerProfileRequest{
		OrganizationId: w.org.ID, RunnerProfileId: w.profileID,
		Profile: &sandrunv1.RunnerProfile{WarmCount: 1}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"warm_count"}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	var warm *sandrunv1.Runner
	eventually(t, "a warm Devin runner", func() bool {
		resp, err := w.env.User(w.org.Owner).Work.ListRunners(ctx, connect.NewRequest(&sandrunv1.ListRunnersRequest{
			OrganizationId: w.org.ID, States: []sandrunv1.RunnerState{sandrunv1.RunnerState_RUNNER_STATE_WARM},
		}))
		if err != nil {
			t.Fatal(err)
		}
		if len(resp.Msg.GetRunners()) == 0 {
			return false
		}
		warm = resp.Msg.GetRunners()[0]
		return true
	})
	return warm
}

func (w *devinWorld) assertClaim(t *testing.T, requestID string, want agent.ClaimState) {
	t.Helper()
	claim, err := w.l.Agents.Claim(t.Context(), requestID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.State != want {
		t.Fatalf("claim state = %v, want %v", claim.State, want)
	}
}

func (w *devinWorld) assertNoRelease(t *testing.T, sessionID string) {
	t.Helper()
	if calls := w.devin.ReleaseAttempts(sessionID); calls != 0 {
		t.Fatalf("vendor release calls = %d, want 0", calls)
	}
	if len(w.devin.Messages(sessionID)) != 0 {
		t.Fatal("SandRun sent an operator message for a lost claim")
	}
}

func TestDevinClaimLossAfterDispatchWins(t *testing.T) {
	w := newDevinWorld(t, sandrunv1.GuestOs_GUEST_OS_LINUX)
	warm := w.warmRunner(t)
	ctx := t.Context()
	// Pause dispatch after it locks the request but before it commits STARTING.
	_, err := w.env.DB.Pool.Exec(ctx, fmt.Sprintf(`
		CREATE FUNCTION pause_devin_binding() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			PERFORM pg_advisory_xact_lock(42);
			RETURN NEW;
		END $$;
		CREATE TRIGGER pause_devin_binding BEFORE UPDATE OF state ON requests
		FOR EACH ROW WHEN (OLD.state IS DISTINCT FROM NEW.state AND NEW.state = %d)
		EXECUTE FUNCTION pause_devin_binding();`, sandrunv1.RequestState_REQUEST_STATE_STARTING))
	if err != nil {
		t.Fatal(err)
	}
	binding := holdTx(t, w.env, "SELECT pg_advisory_xact_lock(42)")
	const sid = "ses-dispatch-wins"
	w.devin.AddSession(sid, true)
	req := w.requestBySource(t, sid)
	id := req.GetRequestId()
	waitForLockWaiter(t, w.env, lockWait{blockerPID: binding.pid,
		query: "UPDATE requests SET state = $2, runner_id = $3, update_time = $4"})
	// Keep the claim change uncommitted until the fake guest starts.
	claim := holdTx(t, w.env, "SELECT 1 FROM agent_claims WHERE request_id = $1 FOR UPDATE", id)
	// Use a separate connection to observe the locks. The two fixture
	// transactions, dispatch and claim loss can fill the default pool.
	observer, err := pgx.ConnectConfig(ctx, w.env.DB.Pool.Config().ConnConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close(ctx)
	w.devin.ClaimElsewhere(sid)
	eventually(t, "the loss event waits after looking up the claim", func() bool {
		var waiting bool
		err := observer.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'
			AND (query LIKE '%FROM requests WHERE request_id = $1 FOR UPDATE%' OR query LIKE 'UPDATE agent_claims SET state%'))`).Scan(&waiting)
		if err != nil {
			t.Fatal(err)
		}
		return waiting
	})
	binding.commit()
	w.waitRunner(t, warm.GetRunnerId(), sandrunv1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK)
	claim.commit()
	eventually(t, "the lost claim is released locally", func() bool {
		claim, err := w.l.Agents.Claim(ctx, id)
		return err == nil && claim.State == agent.ClaimReleased
	})
	r := w.request(t, id)
	if r.GetState() != sandrunv1.RequestState_REQUEST_STATE_STARTING || r.GetRunnerId() != warm.GetRunnerId() {
		t.Fatalf("claim loss changed the assigned request: %v", r)
	}
	w.finishDevinRequestAfterLoss(t, id, sid, warm)
}

func TestDevinClaimLossWhileRunning(t *testing.T) {
	w := newDevinWorld(t, sandrunv1.GuestOs_GUEST_OS_LINUX)
	const sid = "ses-running"
	w.devin.AddSession(sid, true)
	req := w.requestBySource(t, sid)
	id := req.GetRequestId()
	started := w.waitRequest(t, id, sandrunv1.RequestState_REQUEST_STATE_STARTING)
	runner := w.waitRunner(t, started.GetRunnerId(), sandrunv1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK)
	w.devin.SetSessionStatus(sid, "running")
	w.waitRequest(t, id, sandrunv1.RequestState_REQUEST_STATE_RUNNING)
	w.devin.DeleteSession(sid)
	eventually(t, "the lost claim is released locally", func() bool {
		claim, err := w.l.Agents.Claim(t.Context(), id)
		return err == nil && claim.State == agent.ClaimReleased
	})
	r := w.request(t, id)
	if r.GetState() != sandrunv1.RequestState_REQUEST_STATE_RUNNING || r.GetRunnerId() != runner.GetRunnerId() {
		t.Fatalf("claim loss changed the running request: %v", r)
	}
	w.finishDevinRequestAfterLoss(t, id, sid, runner)
}

func (w *devinWorld) finishDevinRequestAfterLoss(t *testing.T, id, sid string, runner *sandrunv1.Runner) {
	t.Helper()
	w.guest.exitProgram(runner.GetOwnsandSandboxId(), 0)
	ended := w.waitRequest(t, id, sandrunv1.RequestState_REQUEST_STATE_ENDED)
	if ended.GetEndReason() != sandrunv1.RequestEndReason_REQUEST_END_REASON_COMPLETED || ended.GetRunnerId() != runner.GetRunnerId() {
		t.Fatalf("program exit did not complete the assigned request: %v", ended)
	}
	w.waitRunner(t, runner.GetRunnerId(), sandrunv1.RunnerState_RUNNER_STATE_DELETED)
	w.assertNoRelease(t, sid)
}

func TestDevinClaimLossBeforeDispatchWins(t *testing.T) {
	for _, tc := range []struct {
		name string
		lost func(*devinfake.Fake, string)
	}{
		{"deleted", (*devinfake.Fake).DeleteSession},
		{"other_acceptor", (*devinfake.Fake).ClaimElsewhere},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newDevinWorld(t, sandrunv1.GuestOs_GUEST_OS_LINUX)
			warm := w.warmRunner(t)
			// Dispatch selects the warm runner and waits for its lock.
			dispatch := holdTx(t, w.env, "SELECT 1 FROM runners WHERE runner_id = $1 FOR UPDATE", warm.GetRunnerId())
			const sid = "ses-loss-wins"
			w.devin.AddSession(sid, true)
			req := w.requestBySource(t, sid)
			id := req.GetRequestId()
			waitForLockWaiter(t, w.env, lockWait{blockerPID: dispatch.pid, query: "FROM runners WHERE runner_id = $1 FOR UPDATE"})
			w.assertClaim(t, id, agent.ClaimHeld)

			tc.lost(w.devin, sid)
			canceled := w.waitRequest(t, id, sandrunv1.RequestState_REQUEST_STATE_CANCELED)
			if canceled.GetEndReason() != sandrunv1.RequestEndReason_REQUEST_END_REASON_CANCELED_BY_SOURCE || canceled.GetRunnerId() != "" {
				t.Fatalf("unexpected cancellation: %v", canceled)
			}
			w.assertClaim(t, id, agent.ClaimReleased)
			dispatch.commit()

			tc.lost(w.devin, sid)
			stays(t, "the canceled request and the unassigned warm runner", func() bool {
				return proto.Equal(w.request(t, id), canceled) &&
					w.runner(t, warm.GetRunnerId()).GetState() == sandrunv1.RunnerState_RUNNER_STATE_WARM
			})
			w.assertNoRelease(t, sid)
		})
	}
}

func TestDevinClaimLossRollsBack(t *testing.T) {
	for _, tc := range []struct {
		table string
		state int16
	}{
		{"agent_claims", int16(agent.ClaimReleased)},
		{"requests", int16(sandrunv1.RequestState_REQUEST_STATE_CANCELED)},
	} {
		t.Run(tc.table, func(t *testing.T) {
			w := newDevinWorld(t, sandrunv1.GuestOs_GUEST_OS_LINUX)
			w.full()
			ctx := t.Context()
			const sid = "ses-rollback"
			w.devin.AddSession(sid, true)
			req := w.requestBySource(t, sid)
			id := req.GetRequestId()
			_, err := w.env.DB.Pool.Exec(ctx, fmt.Sprintf(
				"ALTER TABLE %s ADD CONSTRAINT reject_devin_loss CHECK (state <> %d) NOT VALID", tc.table, tc.state))
			if err != nil {
				t.Fatal(err)
			}

			w.devin.DeleteSession(sid)
			eventually(t, "the loss write fails", func() bool {
				return strings.Contains(w.env.Logs.String(), "reject_devin_loss")
			})
			r := w.request(t, id)
			if r.GetState() != sandrunv1.RequestState_REQUEST_STATE_QUEUED || r.GetRunnerId() != "" {
				t.Fatalf("failed loss changed the request: %v", r)
			}
			w.assertClaim(t, id, agent.ClaimHeld)

			if _, err := w.env.DB.Pool.Exec(ctx, "ALTER TABLE "+tc.table+" DROP CONSTRAINT reject_devin_loss"); err != nil {
				t.Fatal(err)
			}
			w.devin.DeleteSession(sid)
			canceled := w.waitRequest(t, id, sandrunv1.RequestState_REQUEST_STATE_CANCELED)
			if canceled.GetEndReason() != sandrunv1.RequestEndReason_REQUEST_END_REASON_CANCELED_BY_SOURCE {
				t.Fatalf("retry did not cancel the request: %v", canceled)
			}
			w.assertClaim(t, id, agent.ClaimReleased)
			w.assertNoRelease(t, sid)
		})
	}
}

// TestDevinSourceKeyAfterRetention: when the retention job deleted the
// first request of a session, the next request of the session still gets
// an unused source key.
func TestDevinSourceKeyAfterRetention(t *testing.T) {
	w := newDevinWorld(t, sandrunv1.GuestOs_GUEST_OS_LINUX)
	w.full()
	w.devin.AddSession("ses-keep", true)
	first := w.requestBySource(t, "ses-keep")
	w.env.Clock.Advance(2 * time.Hour)
	w.waitRequest(t, first.GetRequestId(), sandrunv1.RequestState_REQUEST_STATE_EXPIRED)
	second := w.requestBySource(t, "ses-keep#2")
	// The retention job deletes the first request (and its claim).
	if _, err := w.env.DB.Pool.Exec(context.Background(), "DELETE FROM requests WHERE request_id = $1", first.GetRequestId()); err != nil {
		t.Fatal(err)
	}
	w.env.Clock.Advance(2 * time.Hour)
	w.waitRequest(t, second.GetRequestId(), sandrunv1.RequestState_REQUEST_STATE_EXPIRED)
	third := w.requestBySource(t, "ses-keep#3")
	if third.GetState() != sandrunv1.RequestState_REQUEST_STATE_QUEUED {
		t.Fatalf("third request %v", third)
	}
}

// TestDevinCancelReleasedByPoller: when the claim poller releases the claim
// of a canceled request (as after a restart between the cancellation commit
// and its release step), the session that returns to the queue is still
// declined.
func TestDevinCancelReleasedByPoller(t *testing.T) {
	w := newDevinWorld(t, sandrunv1.GuestOs_GUEST_OS_LINUX)
	w.full()
	w.devin.AddSession("ses-recover", true)
	req := w.requestBySource(t, "ses-recover")
	ctx := context.Background()
	// The cancellation commits without its release step.
	err := w.env.DB.InTx(ctx, func(tx pgx.Tx) error {
		_, err := w.l.Engine.EndRequestTx(ctx, tx, req.GetRequestId(), work.RequestEnd{State: sandrunv1.RequestState_REQUEST_STATE_CANCELED,
			Reason: sandrunv1.RequestEndReason_REQUEST_END_REASON_CANCELED_BY_USER})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the poller releases the claim", func() bool { return w.devin.Releases("ses-recover") == 1 })
	stays(t, "no request after the cancel", func() bool {
		for _, r := range w.requests(t) {
			if r.GetSourceKey() == "ses-recover#2" {
				return false
			}
		}
		return true
	})
}

func TestDevinLookupFailureReleaseGrace(t *testing.T) {
	const sid = "ses-release-grace"
	w := newDevinWorld(t, sandrunv1.GuestOs_GUEST_OS_LINUX)
	w.full()
	w.devin.AddSession(sid, true)
	requestID := w.requestBySource(t, sid).GetRequestId()
	// Stop background polling so each test step owns one round of release.
	w.env.Stop()
	ctx := t.Context()
	endedAt := w.env.Clock.Now()
	if _, err := w.env.DB.Pool.Exec(ctx, `
		UPDATE requests SET state = $2, end_reason = $3, start_time = $4
		WHERE request_id = $1`, requestID, sandrunv1.RequestState_REQUEST_STATE_ENDED,
		sandrunv1.RequestEndReason_REQUEST_END_REASON_COMPLETED, endedAt.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	w.devin.FailLookups(true)

	w.l.Agents.Poll(ctx)
	w.assertClaim(t, requestID, agent.ClaimHeld)
	w.assertNoRelease(t, sid)
	if !strings.Contains(w.env.Logs.String(), "agent_claim_release_failed") {
		t.Fatal("the poller did not report the lookup error")
	}

	if _, err := w.env.DB.Pool.Exec(ctx, "UPDATE requests SET end_time = $2 WHERE request_id = $1", requestID, endedAt); err != nil {
		t.Fatal(err)
	}
	// The test world configures one minute of release grace.
	w.env.Clock.Advance(time.Minute - time.Microsecond)
	w.l.Agents.Poll(ctx)
	w.assertClaim(t, requestID, agent.ClaimHeld)
	w.assertNoRelease(t, sid)

	w.env.Clock.Advance(time.Microsecond)
	w.devin.FailReleases(true)
	w.l.Agents.Poll(ctx)
	w.assertClaim(t, requestID, agent.ClaimHeld)
	if attempts := w.devin.ReleaseAttempts(sid); attempts != 1 {
		t.Fatalf("release attempts at grace = %d, want 1", attempts)
	}

	w.devin.FailReleases(false)
	w.l.Agents.Poll(ctx)
	w.assertClaim(t, requestID, agent.ClaimReleased)
	// The fake only accepts release with the original acceptor ID.
	if attempts, releases := w.devin.ReleaseAttempts(sid), w.devin.Releases(sid); attempts != 2 || releases != 1 {
		t.Fatalf("release attempts = %d and successes = %d, want 2 and 1", attempts, releases)
	}
	if len(w.devin.Messages(sid)) != 0 {
		t.Fatal("operator message for a served session")
	}
}

// TestDevinStartCheckPartialFailure: when the poller cannot read the queue
// entry of one STARTING session, it still marks the runner of another
// session BUSY when that session runs (a missed watch event).
func TestDevinStartCheckPartialFailure(t *testing.T) {
	t.Run("missing entry", func(t *testing.T) {
		testDevinStartCheckPartialFailure(t, (*devinfake.Fake).Forget, "DEVIN get_queue_entry: HTTP 404")
	})
	t.Run("empty response", func(t *testing.T) {
		testDevinStartCheckPartialFailure(t, func(fake *devinfake.Fake, sid string) {
			fake.SetEmptyLookup(sid, true)
		}, "DEVIN get_queue_entry: decode response: empty body")
	})
}

func testDevinStartCheckPartialFailure(t *testing.T, failLookup func(*devinfake.Fake, string), wantError string) {
	t.Helper()
	w := newDevinWorld(t, sandrunv1.GuestOs_GUEST_OS_LINUX)
	waiting := func(sid string) *sandrunv1.Runner {
		req := w.requestBySource(t, sid)
		var runner *sandrunv1.Runner
		eventually(t, sid+" runner waits for work", func() bool {
			r := w.request(t, req.GetRequestId())
			if r.GetRunnerId() == "" {
				return false
			}
			runner = w.runner(t, r.GetRunnerId())
			return runner.GetState() == sandrunv1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK
		})
		return runner
	}
	w.devin.AddSession("ses-failed", true)
	failed := waiting("ses-failed")
	w.devin.AddSession("ses-ok", true)
	ok := waiting("ses-ok")
	failLookup(w.devin, "ses-failed")
	eventually(t, "the failed session check to be logged", func() bool {
		logs := w.env.Logs.String()
		return strings.Contains(logs, "agent_session_check_failed") && strings.Contains(logs, wantError)
	})
	w.devin.SetSessionStatusSilently("ses-ok", "running")
	w.waitRunner(t, ok.GetRunnerId(), sandrunv1.RunnerState_RUNNER_STATE_BUSY)
	if state := w.runner(t, failed.GetRunnerId()).GetState(); state != sandrunv1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK {
		t.Fatalf("runner with a failed queue-entry lookup is %v, want WAITING_FOR_WORK", state)
	}
}

func TestDevinWatchReconnect(t *testing.T) {
	for _, protoMajor := range []int{1, 2} {
		t.Run(fmt.Sprintf("HTTP%d", protoMajor), func(t *testing.T) {
			const (
				headerBudget = 300 * time.Millisecond
				bodyBudget   = 200 * time.Millisecond
				refresh      = 1500 * time.Millisecond
				firstBackoff = 50 * time.Millisecond
			)
			fake := devinfake.New(t, devinAPIToken, "sandrun-outpost")
			lists := make(chan time.Time, 32)
			watches := make(chan time.Time, 32)
			canceled := make(chan time.Time, 32)
			var attempts atomic.Int32
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				query := r.URL.Query()
				if r.Method == http.MethodGet && r.URL.Path == "/opbeta/outposts/devins" {
					if query.Get("phase") == "pending" {
						lists <- time.Now()
					}
					if query.Get("watch") == "true" {
						if r.ProtoMajor != protoMajor {
							t.Errorf("watch used %s, want HTTP/%d", r.Proto, protoMajor)
						}
						attempt := attempts.Add(1)
						watches <- time.Now()
						switch attempt {
						case 1:
							<-r.Context().Done()
							canceled <- time.Now()
							return
						case 2:
							w.Header().Set("Retry-After", "7")
							w.WriteHeader(http.StatusServiceUnavailable)
							fmt.Fprint(w, "incomplete diagnostic")
							w.(http.Flusher).Flush()
							<-r.Context().Done()
							canceled <- time.Now()
							return
						case 3:
							w.WriteHeader(http.StatusServiceUnavailable)
							return
						case 4, 5:
							w.Header().Set("Content-Type", "text/event-stream")
							w.WriteHeader(http.StatusOK)
							return
						case 7:
							if protoMajor == 1 {
								<-r.Context().Done()
								canceled <- time.Now()
								return
							}
						}
					}
				}
				fake.Server.Config.Handler.ServeHTTP(w, r)
			}))
			if protoMajor == 2 {
				server.EnableHTTP2 = true
				server.StartTLS()
			} else {
				server.Start()
			}
			t.Cleanup(server.Close)
			transport := server.Client().Transport.(*http.Transport).Clone()
			transport.ResponseHeaderTimeout = headerBudget
			t.Cleanup(transport.CloseIdleConnections)

			reader := sdkmetric.NewManualReader()
			provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
			t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
			timing := fastAgentTiming()
			timing.BackoffMin, timing.BackoffMax = firstBackoff, 2*time.Second
			opts := layers.Options{AgentTiming: timing, Devin: devin.Options{
				BaseURL: server.URL, HTTP: &http.Client{Transport: transport}, WatchRefreshInterval: refresh, WatchErrorBodyTimeout: bodyBudget,
			}}
			w := createDevinWorldWithOptions(t, sandrunv1.GuestOs_GUEST_OS_LINUX, fake, opts,
				func(o *app.Options) { o.Metrics = provider })
			w.full()
			receiveDevinWatchTime(t, lists, "initial list")
			for i, failure := range []struct {
				budget time.Duration
				missed string
			}{{headerBudget, "ses-headers"}, {bodyBudget, "ses-body"}, {}, {}, {}} {
				start := receiveDevinWatchTime(t, watches, "failing watch")
				if failure.missed != "" {
					fake.AddSession(failure.missed, false)
					receiveDevinWatchTime(t, canceled, "stalled request cancellation")
				}
				relist := receiveDevinWatchTime(t, lists, "relist after failure")
				backoff := firstBackoff << i
				if elapsed := relist.Sub(start); elapsed < failure.budget+backoff {
					t.Fatalf("attempt %d relisted after %s, want at least %s", i+1, elapsed, failure.budget+backoff)
				}
				if failure.missed != "" {
					w.requestBySource(t, failure.missed)
				}
				t.Logf("attempt %d relisted after %s (timeout budget %s, backoff %s)", i+1, relist.Sub(start), failure.budget, backoff)
			}

			opened := receiveDevinWatchTime(t, watches, "healthy watch")
			eventually(t, "healthy stream", func() bool { return fake.Streams() == 1 })
			time.Sleep(time.Until(opened.Add(headerBudget + bodyBudget)))
			fake.AddSession("ses-event", true)
			w.requestBySource(t, "ses-event")
			fake.Announce("ses-event")
			fake.AddSession("ses-rotation", false)
			rotated := receiveDevinWatchTime(t, lists, "scheduled relist")
			receiveDevinWatchTime(t, watches, "watch after rotation")
			if protoMajor == 2 {
				eventually(t, "stream after rotation", func() bool { return fake.Streams() == 1 })
			}
			w.env.Stop()
			if protoMajor == 1 {
				receiveDevinWatchTime(t, canceled, "parent canceled while awaiting headers")
			}
			if len(lists) != 0 || attempts.Load() != 7 {
				t.Fatalf("extra requests after cancellation: %d lists, %d total watch attempts", len(lists), attempts.Load())
			}
			want := []string{"ses-body", "ses-event", "ses-headers", "ses-rotation"}
			for _, sid := range want {
				if claims := fake.Claims(sid); claims != 1 {
					t.Errorf("%s has %d claims, want one", sid, claims)
				}
			}
			rows, err := w.env.DB.Pool.Query(t.Context(), "SELECT source_key FROM requests WHERE organization_id = $1 ORDER BY source_key", w.org.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got, err := pgx.CollectRows(rows, pgx.RowTo[string]); err != nil || !slices.Equal(got, want) {
				t.Fatalf("persisted request keys = %q, want %q, error %v", got, want, err)
			}
			logs := w.env.Logs.String()
			if failed, scheduled := strings.Count(logs, `"cause":"error"`), strings.Count(logs, `"cause":"scheduled"`); failed != 5 || scheduled != 1 {
				t.Fatalf("reconnect causes: %d errors and %d scheduled, want 5 and 1", failed, scheduled)
			}
			if !strings.Contains(logs, "timeout awaiting response headers") ||
				strings.Count(logs, `"error":"the Devin queue stream ended before scheduled rotation"`) != 2 ||
				strings.Count(logs, `"op":"watch_queue","status":503`) != 2 {
				t.Fatal("missing reconnect or vendor status diagnostics")
			}
			var metrics metricdata.ResourceMetrics
			if err := reader.Collect(t.Context(), &metrics); err != nil {
				t.Fatal(err)
			}
			var reconnects int64
			for _, scope := range metrics.ScopeMetrics {
				for _, m := range scope.Metrics {
					if m.Name == "sandrun_vendor_watch_reconnects" {
						for _, point := range m.Data.(metricdata.Sum[int64]).DataPoints {
							if vendor, _ := point.Attributes.Value("vendor"); vendor.AsString() == "DEVIN" {
								reconnects += point.Value
							}
						}
					}
				}
			}
			if reconnects != 5 {
				t.Fatalf("DEVIN reconnect count = %d, want 5", reconnects)
			}
			t.Logf("scheduled relist after %s, no retry on shutdown, %d failure reconnects, one claim and one stored request per session", rotated.Sub(opened), reconnects)
		})
	}
}

func receiveDevinWatchTime(t *testing.T, observations <-chan time.Time, what string) time.Time {
	t.Helper()
	select {
	case at := <-observations:
		return at
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		return time.Time{}
	}
}

func TestDevinWatchDefaultPagination(t *testing.T) {
	fake := devinfake.New(t, devinAPIToken, "sandrun-outpost")
	sessionIDs := []string{"ses-1", "ses-2"}
	for _, sid := range sessionIDs {
		fake.AddSession(sid, false)
	}
	w := createDevinWorld(t, sandrunv1.GuestOs_GUEST_OS_LINUX, fake)
	eventually(t, "the Devin watch after the default pages", func() bool {
		return fake.Streams() == 1 || fake.Lists() > len(sessionIDs)
	})
	if got := fake.ClaimAttempts(); !slices.Equal(got, sessionIDs) {
		t.Fatalf("claim attempts = %q, want %q", got, sessionIDs)
	}
	if got := fake.Lists(); got != 2 {
		t.Fatalf("list requests = %d, want 2", got)
	}
	if got := fake.WatchCursors(); !slices.Equal(got, []string{"page-2"}) {
		t.Fatalf("watch cursors = %q, want [page-2]", got)
	}
	var sources []string
	for _, req := range w.requests(t) {
		sources = append(sources, req.GetSourceKey())
	}
	slices.Sort(sources)
	if !slices.Equal(sources, sessionIDs) {
		t.Fatalf("persisted request sources at watch startup = %q, want %q", sources, sessionIDs)
	}
	t.Logf("SandRun listed %d pages, claimed %q, persisted requests for %q and opened a watch at %q",
		fake.Lists(), fake.ClaimAttempts(), sources, fake.WatchCursors())
}

func TestDevinWatchPagination(t *testing.T) {
	tests := []struct {
		name        string
		pages       map[string]devinfake.QueuePage
		sessions    []string
		watchCursor string
		stalled     bool
	}{
		{
			name: "empty cursor",
			pages: map[string]devinfake.QueuePage{
				"": {SessionIDs: []string{"ses-1", "ses-1", "ses-2"}, HasNextPage: true},
			},
			sessions: []string{"ses-1", "ses-2"},
			stalled:  true,
		},
		{
			name: "empty cursor after an advancing page",
			pages: map[string]devinfake.QueuePage{
				"":       {SessionIDs: []string{"ses-1"}, Cursor: "page-1", HasNextPage: true},
				"page-1": {SessionIDs: []string{"ses-1", "ses-2"}, HasNextPage: true},
			},
			sessions:    []string{"ses-1", "ses-2"},
			watchCursor: "page-1",
			stalled:     true,
		},
		{
			name: "unchanged cursor",
			pages: map[string]devinfake.QueuePage{
				"":       {SessionIDs: []string{"ses-1"}, Cursor: "page-1", HasNextPage: true},
				"page-1": {SessionIDs: []string{"ses-1", "ses-2"}, Cursor: "page-1", HasNextPage: true},
			},
			sessions:    []string{"ses-1", "ses-2"},
			watchCursor: "page-1",
			stalled:     true,
		},
		{
			name: "earlier cursor repeats",
			pages: map[string]devinfake.QueuePage{
				"":       {SessionIDs: []string{"ses-1"}, Cursor: "page-1", HasNextPage: true},
				"page-1": {SessionIDs: []string{"ses-2"}, Cursor: "page-2", HasNextPage: true},
				"page-2": {SessionIDs: []string{"ses-1", "ses-3"}, Cursor: "page-1", HasNextPage: true},
			},
			sessions:    []string{"ses-1", "ses-2", "ses-3"},
			watchCursor: "page-2",
			stalled:     true,
		},
		{
			name: "terminal page returns an earlier cursor",
			pages: map[string]devinfake.QueuePage{
				"":       {SessionIDs: []string{"ses-1"}, Cursor: "page-1", HasNextPage: true},
				"page-1": {SessionIDs: []string{"ses-2"}, Cursor: "page-2", HasNextPage: true},
				"page-2": {SessionIDs: []string{"ses-3"}, Cursor: "page-1"},
			},
			sessions:    []string{"ses-1", "ses-2", "ses-3"},
			watchCursor: "page-1",
		},
		{
			name: "advancing cursors",
			pages: map[string]devinfake.QueuePage{
				"":       {SessionIDs: []string{"ses-1"}, Cursor: "page-1", HasNextPage: true},
				"page-1": {SessionIDs: []string{"ses-2"}, Cursor: "page-2", HasNextPage: true},
				"page-2": {SessionIDs: []string{"ses-3"}, Cursor: "page-3", HasNextPage: true},
				"page-3": {SessionIDs: []string{"ses-4"}, Cursor: "page-4", HasNextPage: true},
				"page-4": {SessionIDs: []string{"ses-5"}, Cursor: "page-5"},
			},
			sessions:    []string{"ses-1", "ses-2", "ses-3", "ses-4", "ses-5"},
			watchCursor: "page-5",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := devinfake.New(t, devinAPIToken, "sandrun-outpost")
			for _, sid := range tc.sessions {
				fake.AddSession(sid, false)
			}
			fake.SetQueuePages(t, tc.pages)
			w := createDevinWorld(t, sandrunv1.GuestOs_GUEST_OS_LINUX, fake)
			eventually(t, "the Devin watch after the listed pages", func() bool {
				return fake.Streams() == 1 || fake.Lists() > len(tc.pages)
			})
			if got := fake.Lists(); got != len(tc.pages) {
				t.Fatalf("list requests = %d, want %d", got, len(tc.pages))
			}
			if got := fake.WatchCursors(); !slices.Equal(got, []string{tc.watchCursor}) {
				t.Fatalf("watch cursors = %q, want [%q]", got, tc.watchCursor)
			}
			if got := fake.ClaimAttempts(); !slices.Equal(got, tc.sessions) {
				t.Fatalf("claim attempts = %q, want %q", got, tc.sessions)
			}
			for _, sid := range tc.sessions {
				w.requestBySource(t, sid)
			}
			var warnings int
			for line := range strings.Lines(w.env.Logs.String()) {
				if !strings.Contains(line, `"msg":"devin_queue_pagination_stalled"`) {
					continue
				}
				warnings++
				if !strings.Contains(line, `"level":"WARN"`) ||
					!strings.Contains(line, fmt.Sprintf(`"request_cursor_present":%t`, tc.watchCursor != "")) ||
					!strings.Contains(line, fmt.Sprintf(`"response_cursor_present":%t`, tc.pages[tc.watchCursor].Cursor != "")) {
					t.Fatalf("unexpected pagination warning: %s", line)
				}
				for _, page := range tc.pages {
					if page.Cursor != "" && strings.Contains(line, page.Cursor) {
						t.Fatalf("pagination warning contains a cursor: %s", line)
					}
				}
			}
			wantWarnings := 0
			if tc.stalled {
				wantWarnings = 1
			}
			if warnings != wantWarnings {
				t.Fatalf("pagination warnings = %d, want %d", warnings, wantWarnings)
			}
			t.Logf("Queue list calls %d, claims %q, watch cursors %q, stall warnings %d",
				fake.Lists(), fake.ClaimAttempts(), fake.WatchCursors(), warnings)
		})
	}
}

func TestDevinOutpostPagination(t *testing.T) {
	for _, tc := range []struct {
		name       string
		lastCursor string
		hasNext    bool
		match      bool
	}{
		{name: "unmatched cycle retries", lastCursor: "page-1", hasNext: true},
		{name: "match on the page that repeats a cursor", lastCursor: "page-1", hasNext: true, match: true},
		{name: "match on the final page", lastCursor: "page-3", match: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := devinfake.New(t, devinAPIToken, "sandrun-outpost")
			cursors := map[string]string{"": "page-1", "page-1": "page-2", "page-2": tc.lastCursor}
			type lookup struct {
				cursor string
				at     time.Time
			}
			lookups := make(chan lookup, 4)
			var count atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/opbeta/outposts" {
					fake.Server.Config.Handler.ServeHTTP(w, r)
					return
				}
				cursor := r.URL.Query().Get("cursor")
				n := count.Add(1)
				if n <= 4 {
					lookups <- lookup{cursor: cursor, at: time.Now()}
				}
				if n == 4 && cursor == "" && !tc.match {
					// Hold the retry before it can list the pending queue.
					<-r.Context().Done()
					return
				}
				items := "[]"
				if cursor == "page-2" && tc.match {
					items = fmt.Sprintf(`[{"metadata":{"outpost_id":%q},"spec":{"name":%q}}]`, fake.OutpostID, fake.OutpostName)
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"items":%s,"cursor":%q,"has_next_page":%t}`, items, cursors[cursor], cursor != "page-2" || tc.hasNext)
			}))
			t.Cleanup(server.Close)

			timing := fastAgentTiming()
			timing.BackoffMin = 100 * time.Millisecond
			w := createDevinWorldWithOptions(t, sandrunv1.GuestOs_GUEST_OS_LINUX, fake,
				layers.Options{AgentTiming: timing, Devin: devin.Options{BaseURL: server.URL}})
			want := []string{"", "page-1", "page-2"}
			if !tc.match {
				want = append(want, "")
			}
			var closingPage time.Time
			for i, cursor := range want {
				select {
				case got := <-lookups:
					if got.cursor != cursor {
						t.Fatalf("outpost request %d used cursor %q, want %q", i+1, got.cursor, cursor)
					}
					if i == 2 {
						closingPage = got.at
					}
					if i == 3 {
						delay := got.at.Sub(closingPage)
						if delay < timing.BackoffMin {
							t.Fatalf("outpost retry after %s, want at least %s", delay, timing.BackoffMin)
						}
						t.Logf("Outpost retry restarted after %s", delay)
					}
				case <-time.After(15 * time.Second):
					t.Fatalf("timed out waiting for outpost request %d", i+1)
				}
			}
			if tc.match {
				eventually(t, "the Devin watch after outpost lookup", func() bool {
					return fake.Streams() == 1 || count.Load() > 3
				})
				if fake.Lists() != 1 || len(fake.WatchCursors()) != 1 {
					t.Fatalf("queue list calls = %d, watch cursors = %q, want one list and one watch",
						fake.Lists(), fake.WatchCursors())
				}
			} else {
				if fake.Lists() != 0 || len(fake.WatchCursors()) != 0 {
					t.Fatalf("queue list calls = %d, watch cursors = %q before outpost lookup succeeded",
						fake.Lists(), fake.WatchCursors())
				}
				if !slices.ContainsFunc(strings.Split(w.env.Logs.String(), "\n"), func(line string) bool {
					return strings.Contains(line, `"msg":"vendor_watch_reconnect"`) &&
						strings.Contains(line, `"cause":"error"`) &&
						strings.Contains(line, `"error":"no Devin outpost is named \"sandrun-outpost\""`)
				}) {
					t.Fatal("missing the outpost lookup error on reconnect")
				}
			}
			if got := count.Load(); int(got) != len(want) {
				t.Fatalf("outpost requests = %d, want %d", got, len(want))
			}
			t.Logf("Outpost request cursors %q, queue lists %d, watch cursors %q",
				want, fake.Lists(), fake.WatchCursors())
		})
	}
}

func TestDevinInstallScripts(t *testing.T) {
	win := devin.InstallScript(sandrunv1.GuestOs_GUEST_OS_WINDOWS)
	for _, want := range []string{"irm https://static.devin.ai/cli/setup.ps1 | iex", "$env:PROCESSOR_ARCHITECTURE -ne 'AMD64'", "git.exe"} {
		if !strings.Contains(win, want) {
			t.Fatalf("windows script lacks %q", want)
		}
	}
	mac := devin.InstallScript(sandrunv1.GuestOs_GUEST_OS_MACOS)
	for _, want := range []string{`"$(uname -m)" != arm64`, "command -v git"} {
		if !strings.Contains(mac, want) {
			t.Fatalf("posix script lacks %q", want)
		}
	}
}
