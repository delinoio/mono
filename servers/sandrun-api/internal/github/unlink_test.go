// SPDX-License-Identifier: FSL-1.1-ALv2

package github_test

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/go/ownsand"
	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
)

func TestUnlinkWaitsForJITRegistration(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusNoContent, http.StatusUnprocessableEntity, http.StatusServiceUnavailable, http.StatusNotFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			w := newGHWorld(t, worldOpts{})
			inst := w.link(t, 501, "acme")
			w.createProfile(t, ghProfile("linux", 0))

			jitEntered, jitRelease := make(chan struct{}), make(chan struct{})
			releaseJIT := sync.OnceFunc(func() { close(jitRelease) })
			defer releaseJIT()
			w.gh.Update(func() {
				w.gh.JITGate = func() {
					close(jitEntered)
					<-jitRelease
				}
			})
			w.send(t, "workflow_job", job("queued", 501, 1, sandRunLinux, ""))
			awaitUnlinkStep(t, "JIT request", jitEntered)
			starting := w.runnersIn(t, sandrunv1.RunnerState_RUNNER_STATE_PREPARING)
			if len(starting) != 1 {
				t.Fatalf("preparing runners = %d, want 1", len(starting))
			}
			runnerID := starting[0].GetRunnerId()
			runnerName := "sandrun-" + runnerID
			var storedID *int64
			if err := w.env.DB.Pool.QueryRow(t.Context(), "SELECT github_runner_id FROM github_runners WHERE runner_id = $1", runnerID).
				Scan(&storedID); err != nil || storedID != nil {
				t.Fatalf("pending JIT placeholder ID = %v, error = %v", storedID, err)
			}

			deleteEntered, deleteRelease := make(chan int64, 1), make(chan struct{})
			releaseDelete := sync.OnceFunc(func() { close(deleteRelease) })
			defer releaseDelete()
			w.gh.Update(func() {
				w.gh.JITGate = nil
				if status == http.StatusServiceUnavailable {
					w.gh.FailDelete = status
				}
				w.gh.DeleteGate = func(id int64) {
					switch status {
					case http.StatusUnprocessableEntity:
						w.gh.SetBusy(runnerName, true)
					case http.StatusNotFound:
						w.gh.ForgetRunner(id)
					}
					select {
					case deleteEntered <- id:
					case <-deleteRelease:
						return
					}
					<-deleteRelease
				}
			})

			unlink := func() error {
				_, err := w.env.User(w.org.Admin).GitHub.UnlinkGitHubInstallation(t.Context(), connect.NewRequest(&sandrunv1.UnlinkGitHubInstallationRequest{
					OrganizationId: w.org.ID, InstallationId: inst.GetInstallationId(),
				}))
				return err
			}
			assertRetained := func() {
				t.Helper()
				if !installationPresent(t, w, inst.GetInstallationId()) || w.os.Active(w.osOrg) != 1 || w.os.Calls("DeleteSandbox") != 0 {
					t.Fatal("the link or sandbox was removed before GitHub accepted the deletion")
				}
			}
			done := make(chan error, 1)
			go func() { done <- unlink() }()
			w.waitInstallationLock(t, "UPDATE")
			assertRetained()
			if calls := w.gh.Calls("delete_runner"); calls != 0 {
				t.Fatalf("GitHub deletions while JIT was paused = %d, want 0", calls)
			}
			t.Log("PostgreSQL observed unlink waiting for JIT. The link and sandbox were still present.")

			releaseJIT()
			id := awaitUnlinkStep(t, "GitHub deletion", deleteEntered)
			if err := w.env.DB.Pool.QueryRow(t.Context(), "SELECT github_runner_id FROM github_runners WHERE runner_id = $1", runnerID).
				Scan(&storedID); err != nil || storedID == nil || *storedID != id {
				t.Fatalf("GitHub DELETE ID = %d, stored registration = %v, error = %v", id, storedID, err)
			}
			assertRetained()
			t.Log("GitHub DELETE targeted the newly published ID while the link and sandbox were present.")
			w.gh.Update(func() { w.gh.DeleteGate = nil })
			releaseDelete()

			err := awaitUnlinkStep(t, "unlink result", done)
			if status == http.StatusServiceUnavailable {
				wantCode(t, "unlink with GitHub unavailable", err, connect.CodeUnavailable)
				assertRetained()
				if _, ok := w.gh.RunnerByName(runnerName); !ok {
					t.Fatal("GitHub 503 did not preserve the registration")
				}
				w.gh.Update(func() { w.gh.FailDelete = 0 })
				err = unlink()
				t.Log("GitHub 503 preserved the registration, link and sandbox. Unlink retried after recovery.")
			}
			if err != nil {
				t.Fatal(err)
			}
			if installationPresent(t, w, inst.GetInstallationId()) {
				t.Fatal("installation link remained after unlink")
			}
			if status == http.StatusUnprocessableEntity {
				if _, ok := w.gh.RunnerByName(runnerName); !ok || w.os.Active(w.osOrg) != 1 || w.os.Calls("DeleteSandbox") != 0 {
					t.Fatal("GitHub's busy refusal did not preserve the registration and sandbox")
				}
				eventually(t, "the retained runner program to start", func() bool {
					return w.runner(t, runnerID).GetState() == sandrunv1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK
				})
				w.progs.exit(starting[0].GetOwnsandSandboxId())
				t.Log("GitHub 422 kept the runner program and sandbox until the job could finish.")
			} else if _, ok := w.gh.RunnerByName(runnerName); ok {
				t.Fatal("registration remained after unlink")
			}
			eventually(t, "sandbox deleted", func() bool { return w.os.Active(w.osOrg) == 0 })
			if status == http.StatusNotFound && !strings.Contains(w.ghLogs.String(), `"op":"delete_runner","status":404`) {
				t.Fatal("GitHub did not return 404 for the absent registration")
			}
		})
	}
}

func TestJITRegistrationWaitsForUnlink(t *testing.T) {
	t.Parallel()
	w := newGHWorld(t, worldOpts{})
	inst := w.link(t, 501, "acme")
	w.createProfile(t, ghProfile("linux", 0))
	w.send(t, "workflow_job", job("queued", 501, 1, sandRunLinux, ""))
	eventually(t, "one registered waiting runner", func() bool {
		return len(w.runnersIn(t, sandrunv1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK)) == 1
	})

	prepareEntered, prepareRelease := make(chan struct{}, 1), make(chan struct{})
	releasePrepare := sync.OnceFunc(func() { close(prepareRelease) })
	defer releasePrepare()
	w.os.SetExec(func(ctx context.Context, sandboxID string, params ownsand.ExecParams) (*ownsand.ExecResult, error) {
		if !strings.Contains(strings.Join(params.Command, " "), "--jitconfig") {
			select {
			case prepareEntered <- struct{}{}:
			default:
			}
			select {
			case <-prepareRelease:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return w.progs.exec(ctx, sandboxID, params)
	}, w.progs.attach)
	w.send(t, "workflow_job", job("queued", 501, 2, sandRunLinux, ""))
	awaitUnlinkStep(t, "second runner preparation", prepareEntered)

	deleteEntered, deleteRelease := make(chan struct{}), make(chan struct{})
	signalDelete := sync.OnceFunc(func() { close(deleteEntered) })
	releaseDelete := sync.OnceFunc(func() { close(deleteRelease) })
	defer releaseDelete()
	w.gh.Update(func() {
		w.gh.DeleteGate = func(int64) {
			signalDelete()
			<-deleteRelease
		}
	})
	done := make(chan error, 1)
	go func() {
		_, err := w.env.User(w.org.Admin).GitHub.UnlinkGitHubInstallation(t.Context(), connect.NewRequest(&sandrunv1.UnlinkGitHubInstallationRequest{
			OrganizationId: w.org.ID, InstallationId: inst.GetInstallationId(),
		}))
		done <- err
	}()
	awaitUnlinkStep(t, "unlink's GitHub deletion", deleteEntered)
	releasePrepare()
	w.waitInstallationLock(t, "SHARE")
	w.gh.Update(func() { w.gh.DeleteGate = nil })
	releaseDelete()
	if err := awaitUnlinkStep(t, "unlink result", done); err != nil {
		t.Fatal(err)
	}
	eventually(t, "both sandboxes deleted", func() bool {
		return len(w.runnersIn(t, sandrunv1.RunnerState_RUNNER_STATE_DELETED)) == 2 && w.os.Active(w.osOrg) == 0
	})
	if calls := w.gh.Calls("generate_jitconfig"); calls != 1 || len(w.gh.Runners()) != 0 {
		t.Fatalf("JIT generation calls = %d, remaining registrations = %d", calls, len(w.gh.Runners()))
	}
	t.Log("PostgreSQL observed the second JIT start waiting on unlink. One JIT call registered the first runner. The second start made no GitHub JIT call.")
}

func TestUnlinkRetiresRegisteredRunnersOnFailure(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		jobs bool
	}{
		{name: "queued jobs", jobs: true},
		{name: "warm reserve"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, installationID, first, second := unlinkWorld(t, tc.jobs)
			ctx := t.Context()
			failed, ok := w.gh.RunnerByName("sandrun-" + second.GetRunnerId())
			if !ok {
				t.Fatal("second runner has no registration")
			}
			w.gh.Update(func() {
				w.gh.DeleteGate = func(id int64) {
					if id == failed.ID {
						w.gh.Update(func() { w.gh.FailDelete = http.StatusServiceUnavailable })
					}
				}
			})

			wantCode(t, "partial unlink", w.unlink(t, installationID), connect.CodeUnavailable)
			w.assertUnlinkRetired(t, first.GetRunnerId())
			if !installationPresent(t, w, installationID) {
				t.Fatal("partial unlink removed the installation")
			}
			if state := w.runner(t, second.GetRunnerId()).GetState(); state != sandrunv1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK {
				t.Fatalf("runner that GitHub failed to remove changed state: %v", state)
			}
			if _, ok := w.gh.RunnerByName(failed.Name); !ok || w.os.Active(w.osOrg) != 1 {
				t.Fatal("partial unlink did not preserve the second registration and sandbox")
			}
			if tc.jobs {
				for _, id := range []int64{1, 2} {
					if r := w.requestOfJob(t, id); r.GetState() != sandrunv1.RequestState_REQUEST_STATE_QUEUED {
						t.Fatalf("job %d changed state: %v", id, r)
					}
				}
			}
			if calls := w.gh.Calls("delete_runner"); calls != 2 {
				t.Fatalf("GitHub DELETE calls = %d, want 2", calls)
			}
			w.l.Engine.Pass(ctx, w.org.ID)
			if got := len(w.runners(t)); got != 3 || w.os.Active(w.osOrg) != 2 {
				t.Fatalf("replacement pass left %d stored runners and %d active sandboxes, want 3 and 2", got, w.os.Active(w.osOrg))
			}
			t.Log("GitHub 503 kept the installation active. One replacement sandbox started on the next dispatch pass.")

			w.gh.Update(func() { w.gh.FailDelete, w.gh.DeleteGate = 0, nil })
			if err := w.unlink(t, installationID); err != nil {
				t.Fatal(err)
			}
			if installationPresent(t, w, installationID) || len(w.gh.Runners()) != 0 || w.os.Active(w.osOrg) != 0 {
				t.Fatal("retry left the installation, a registration or a sandbox")
			}
		})
	}
}

func TestUnlinkRetirementSurvivesFinalCommitFailure(t *testing.T) {
	t.Parallel()
	w, installationID, first, second := unlinkWorld(t, true)
	ctx := t.Context()
	if _, err := w.env.DB.Pool.Exec(ctx, `
		CREATE FUNCTION fail_runner_retirement() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			RAISE EXCEPTION 'injected runner retirement commit failure' USING ERRCODE = '40001';
		END;
		$$;
		CREATE CONSTRAINT TRIGGER fail_runner_retirement
			AFTER UPDATE ON github_runners DEFERRABLE INITIALLY DEFERRED
			FOR EACH ROW WHEN (NEW.registration_removed) EXECUTE FUNCTION fail_runner_retirement();`); err != nil {
		t.Fatal(err)
	}
	wantCode(t, "failed runner retirement commit", w.unlink(t, installationID), connect.CodeInternal)
	if _, ok := w.gh.RunnerByName("sandrun-" + first.GetRunnerId()); ok || w.gh.Calls("delete_runner") != 1 {
		t.Fatal("the failed commit must stop unlink after the first GitHub DELETE")
	}
	if state := w.runner(t, first.GetRunnerId()).GetState(); state != sandrunv1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK ||
		!installationPresent(t, w, installationID) || w.os.Active(w.osOrg) != 2 {
		t.Fatalf("the failed retirement commit left runner state %v or changed the installation or sandboxes", state)
	}
	var removed bool
	if err := w.env.DB.Pool.QueryRow(ctx, "SELECT registration_removed FROM github_runners WHERE runner_id = $1", first.GetRunnerId()).
		Scan(&removed); err != nil || removed {
		t.Fatalf("removal marker after failed retirement commit = %v, error = %v", removed, err)
	}
	t.Log("GitHub accepted the first removal. The failed runner commit rolled back the marker and state and stopped unlink.")

	if _, err := w.env.DB.Pool.Exec(ctx, "DROP TRIGGER fail_runner_retirement ON github_runners"); err != nil {
		t.Fatal(err)
	}
	// Simulate a marker left by an older unlink implementation.
	if _, err := w.env.DB.Pool.Exec(ctx, "UPDATE github_runners SET registration_removed = true WHERE runner_id = $1", second.GetRunnerId()); err != nil {
		t.Fatal(err)
	}
	reg, ok := w.gh.RunnerByName("sandrun-" + second.GetRunnerId())
	if !ok {
		t.Fatal("second runner has no registration")
	}
	w.gh.ForgetRunner(reg.ID)
	if _, err := w.env.DB.Pool.Exec(ctx, `
		CREATE FUNCTION fail_final_unlink() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			RAISE EXCEPTION 'injected unlink commit failure' USING ERRCODE = '40001';
		END;
		$$;
		CREATE CONSTRAINT TRIGGER fail_final_unlink
			AFTER DELETE ON github_installations DEFERRABLE INITIALLY DEFERRED
			FOR EACH ROW EXECUTE FUNCTION fail_final_unlink();`); err != nil {
		t.Fatal(err)
	}
	wantCode(t, "failed final unlink commit", w.unlink(t, installationID), connect.CodeInternal)
	w.assertUnlinkRetired(t, first.GetRunnerId())
	w.assertUnlinkRetired(t, second.GetRunnerId())
	if !installationPresent(t, w, installationID) || w.os.Active(w.osOrg) != 0 {
		t.Fatal("final rollback failed to preserve the link or kept retired sandboxes")
	}
	for _, id := range []int64{1, 2} {
		if r := w.requestOfJob(t, id); r.GetState() != sandrunv1.RequestState_REQUEST_STATE_QUEUED {
			t.Fatalf("final rollback did not preserve job %d: %v", id, r)
		}
	}
	if w.gh.Calls("delete_runner") != 2 || !strings.Contains(w.ghLogs.String(), `"op":"delete_runner","status":404`) {
		t.Fatal("expected only one additional GitHub DELETE, returning 404 for the first registration")
	}
	t.Log("Existing marker and GitHub 404 retired both runners. PostgreSQL rejected the final commit and kept both jobs queued.")

	if _, err := w.env.DB.Pool.Exec(ctx, "DROP TRIGGER fail_final_unlink ON github_installations"); err != nil {
		t.Fatal(err)
	}
	if err := w.unlink(t, installationID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{1, 2} {
		r := w.requestOfJob(t, id)
		if r.GetState() != sandrunv1.RequestState_REQUEST_STATE_CANCELED ||
			r.GetEndReason() != sandrunv1.RequestEndReason_REQUEST_END_REASON_INTEGRATION_REMOVED {
			t.Fatalf("unlink retry did not cancel job %d: %v", id, r)
		}
	}
	var audits int
	if err := w.env.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM audit_events WHERE organization_id = $1 AND action = $2",
		w.org.ID, int16(sandrunv1.AuditAction_AUDIT_ACTION_GITHUB_INSTALLATION_UNLINKED)).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("unlink audit count = %d, error = %v, want 1", audits, err)
	}
}

func unlinkWorld(t *testing.T, queuedJobs bool) (*ghWorld, string, *sandrunv1.Runner, *sandrunv1.Runner) {
	t.Helper()
	w := newGHWorld(t, worldOpts{manualDispatch: true})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = w.l.Engine.RunReconciler(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	inst := w.link(t, 501, "acme")
	if queuedJobs {
		w.createProfile(t, ghProfile("linux", 0))
		for _, id := range []int64{1, 2} {
			w.send(t, "workflow_job", job("queued", 501, id, sandRunLinux, ""))
		}
	} else {
		w.createProfile(t, ghProfile("linux", 2))
	}
	w.l.Engine.Pass(ctx, w.org.ID)
	waiting := sandrunv1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK
	eventually(t, "two registered idle runners", func() bool { return len(w.runnersIn(t, waiting)) == 2 })
	runners := w.runnersIn(t, waiting)
	var firstID string
	if err := w.env.DB.Pool.QueryRow(ctx, "SELECT runner_id FROM runners ORDER BY runner_id LIMIT 1").Scan(&firstID); err != nil {
		t.Fatal(err)
	}
	if runners[0].GetRunnerId() != firstID {
		runners[0], runners[1] = runners[1], runners[0]
	}
	return w, inst.GetInstallationId(), runners[0], runners[1]
}

func (w *ghWorld) unlink(t *testing.T, installationID string) error {
	t.Helper()
	_, err := w.env.User(w.org.Admin).GitHub.UnlinkGitHubInstallation(t.Context(), connect.NewRequest(&sandrunv1.UnlinkGitHubInstallationRequest{
		OrganizationId: w.org.ID, InstallationId: installationID,
	}))
	return err
}

func (w *ghWorld) assertUnlinkRetired(t *testing.T, runnerID string) {
	t.Helper()
	r := w.runner(t, runnerID)
	if state := r.GetState(); (state != sandrunv1.RunnerState_RUNNER_STATE_DELETING && state != sandrunv1.RunnerState_RUNNER_STATE_DELETED) ||
		r.GetEndReason() != sandrunv1.RunnerEndReason_RUNNER_END_REASON_CLEAN_UP {
		t.Fatalf("runner %s did not retire with CLEAN_UP: %v", runnerID, r)
	}
	var removed bool
	if err := w.env.DB.Pool.QueryRow(t.Context(), "SELECT registration_removed FROM github_runners WHERE runner_id = $1", runnerID).
		Scan(&removed); err != nil || !removed {
		t.Fatalf("runner %s registration removed = %v, error = %v", runnerID, removed, err)
	}
	if _, ok := w.gh.RunnerByName("sandrun-" + runnerID); ok {
		t.Fatalf("runner %s still registered at GitHub", runnerID)
	}
}

func awaitUnlinkStep[T any](t *testing.T, step string, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", step)
		var zero T
		return zero
	}
}

func (w *ghWorld) waitInstallationLock(t *testing.T, mode string) {
	t.Helper()
	eventually(t, "an installation row lock wait for "+mode, func() bool {
		var waiting bool
		if err := w.env.DB.Pool.QueryRow(t.Context(), `SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity WHERE datname = current_database()
			AND wait_event_type = 'Lock' AND query LIKE $1
		)`, "SELECT % FROM github_installations WHERE github_installation_id = $1 FOR "+mode).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		return waiting
	})
}

func installationPresent(t *testing.T, w *ghWorld, installationID string) bool {
	t.Helper()
	var present bool
	if err := w.env.DB.Pool.QueryRow(t.Context(), "SELECT EXISTS (SELECT 1 FROM github_installations WHERE installation_id = $1)", installationID).
		Scan(&present); err != nil {
		t.Fatal(err)
	}
	return present
}
