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
