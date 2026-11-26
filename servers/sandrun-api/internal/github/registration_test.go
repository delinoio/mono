// SPDX-License-Identifier: FSL-1.1-ALv2

package github_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/servers/sandrun-api/internal/githubapi/githubfake"
	"github.com/delinoio/mono/servers/sandrun-api/internal/ownsandclient/ownsandfake"
)

func TestJITRegistrationRollback(t *testing.T) {
	const writeError = "injected runner registration write failure"
	for _, tc := range []struct {
		name            string
		deleteStatus    int
		onCommit        bool
		commitUncertain bool
	}{
		{name: "deleted", deleteStatus: http.StatusNoContent},
		{name: "already absent", deleteStatus: http.StatusNotFound},
		{name: "delete failed", deleteStatus: http.StatusInternalServerError},
		{name: "commit rejected", deleteStatus: http.StatusNoContent, onCommit: true},
		{name: "commit uncertain", onCommit: true, commitUncertain: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newGHWorld(t, worldOpts{})
			w.link(t, 501, "acme")
			w.createProfile(t, ghProfile("linux", 0))

			jitEntered := make(chan struct{})
			release := make(chan struct{})
			releaseJIT := sync.OnceFunc(func() { close(release) })
			defer releaseJIT()
			w.gh.Update(func() {
				w.gh.JITGate = func() {
					close(jitEntered)
					<-release
				}
			})
			w.send(t, "workflow_job", job("queued", 501, 1, sandRunLinux, ""))
			select {
			case <-jitEntered:
			case <-time.After(10 * time.Second):
				t.Fatal("timed out waiting for the JIT request")
			}
			starting := w.runnersIn(t, sandrunv1.RunnerState_RUNNER_STATE_PREPARING)
			if len(starting) != 1 {
				t.Fatalf("got %d preparing runners, want 1", len(starting))
			}
			runnerID := starting[0].GetRunnerId()
			runnerName := "sandrun-" + runnerID
			// The queued job can trigger another start after the failure.
			// Hold it in the queue so observations cover only this sandbox.
			w.os.Update(w.osOrg, func(o *ownsandfake.Org) {
				o.CreateError = connect.NewWireError(connect.CodeUnavailable, errors.New("fixture has no spare capacity"))
			})

			trigger := "CREATE TRIGGER fail_registration BEFORE UPDATE OF github_runner_id ON github_runners"
			sqlState := "P0001"
			if tc.onCommit {
				trigger = "CREATE CONSTRAINT TRIGGER fail_registration AFTER UPDATE ON github_runners DEFERRABLE INITIALLY DEFERRED"
			}
			if tc.commitUncertain {
				sqlState = "40003" // statement_completion_unknown
			}
			if _, err := w.env.DB.Pool.Exec(t.Context(), fmt.Sprintf(`
				CREATE FUNCTION fail_registration() RETURNS trigger LANGUAGE plpgsql AS $$
				BEGIN
					RAISE EXCEPTION '%s' USING ERRCODE = '%s';
				END;
				$$;
				%s FOR EACH ROW WHEN (NEW.runner_id = '%s' AND NEW.github_runner_id IS NOT NULL)
				EXECUTE FUNCTION fail_registration();`, writeError, sqlState, trigger, runnerID)); err != nil {
				t.Fatal(err)
			}

			type deletion struct {
				id           int64
				registration githubfake.Runner
				sandboxCalls int
			}
			deletions := make(chan deletion, 1)
			w.gh.Update(func() {
				if tc.deleteStatus == http.StatusInternalServerError {
					w.gh.FailDelete = tc.deleteStatus
				}
				w.gh.DeleteGate = func(id int64) {
					reg, _ := w.gh.RunnerByName(runnerName)
					select {
					case deletions <- deletion{id, reg, w.os.Calls("DeleteSandbox")}:
					default:
					}
					if tc.deleteStatus == http.StatusNotFound {
						w.gh.ForgetRunner(reg.ID)
					}
				}
				w.gh.JITGate = nil
			})
			releaseJIT()
			eventually(t, "the sandbox deleted", func() bool { return w.os.Active(w.osOrg) == 0 })

			if r := w.runner(t, runnerID); r.GetState() != sandrunv1.RunnerState_RUNNER_STATE_FAILED ||
				r.GetEndReason() != sandrunv1.RunnerEndReason_RUNNER_END_REASON_START_FAILED {
				t.Fatalf("runner = %v, want FAILED with START_FAILED", r)
			}
			var storedID *int64
			if err := w.env.DB.Pool.QueryRow(t.Context(), "SELECT github_runner_id FROM github_runners WHERE runner_id = $1", runnerID).Scan(&storedID); err != nil {
				t.Fatal(err)
			}
			if storedID != nil {
				t.Error("the failed write left a registration ID in the database")
			}
			if w.os.Calls("WriteFile") != 0 || len(w.progs.all()) != 1 {
				t.Error("the guest received a file write or an exec after the installation check")
			}
			logs := w.env.Logs.String()
			var failedStart, failedDelete bool
			for line := range strings.SplitSeq(logs, "\n") {
				failedStart = failedStart || strings.Contains(line, `"msg":"runner_start_failed"`) && strings.Contains(line, writeError)
				failedDelete = failedDelete || strings.Contains(line, `"msg":"github_runner_removal_failed"`) && strings.Contains(line, "github delete_runner: HTTP 500")
			}
			if !failedStart || failedDelete != (tc.deleteStatus == http.StatusInternalServerError) {
				t.Errorf("start failure logged with the write error = %v, deletion failure logged with HTTP 500 = %v", failedStart, failedDelete)
			}

			reg, registered := w.gh.RunnerByName(runnerName)
			if tc.commitUncertain {
				if !registered || w.gh.Calls("delete_runner") != 0 {
					t.Fatal("an uncertain commit attempted to delete the GitHub registration")
				}
			} else {
				select {
				case d := <-deletions:
					if d.registration.ID == 0 || d.id != d.registration.ID || d.sandboxCalls != 0 || w.gh.Calls("delete_runner") != 1 {
						t.Errorf("GitHub DELETE targeted ID %d with registered ID %d after %d sandbox delete calls", d.id, d.registration.ID, d.sandboxCalls)
					}
					reg = d.registration
				default:
					t.Fatal("no GitHub DELETE for the rolled-back registration")
				}
				if registered != (tc.deleteStatus == http.StatusInternalServerError) {
					t.Errorf("registration still present = %v after HTTP %d", registered, tc.deleteStatus)
				}
				if tc.deleteStatus == http.StatusNotFound && !strings.Contains(w.ghLogs.String(), `"op":"delete_runner","status":404`) {
					t.Error("GitHub did not return 404 for the absent registration")
				}
			}
			if reg.JIT == "" || strings.Contains(logs, reg.JIT) || strings.Contains(w.ghLogs.String(), reg.JIT) {
				t.Error("the fixture did not generate a JIT configuration or it appeared in the logs")
			}
		})
	}
}

func TestJITRegistrationCleanupAfterStartFailure(t *testing.T) {
	for _, status := range []int{http.StatusNoContent, http.StatusNotFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			w := newGHWorld(t, worldOpts{})
			w.link(t, 501, "acme")
			w.createProfile(t, ghProfile("linux", 0))

			jitEntered, release := make(chan struct{}), make(chan struct{})
			releaseJIT := sync.OnceFunc(func() { close(release) })
			defer releaseJIT()
			w.gh.Update(func() {
				w.gh.JITGate = func() {
					close(jitEntered)
					<-release
				}
			})
			w.send(t, "workflow_job", job("queued", 501, 1, sandRunLinux, ""))
			select {
			case <-jitEntered:
			case <-time.After(10 * time.Second):
				t.Fatal("timed out waiting for JIT generation")
			}
			starting := w.runnersIn(t, sandrunv1.RunnerState_RUNNER_STATE_PREPARING)
			if len(starting) != 1 {
				t.Fatalf("preparing runners = %d, want 1", len(starting))
			}
			runnerID := starting[0].GetRunnerId()
			runnerName := "sandrun-" + runnerID
			w.os.Update(w.osOrg, func(o *ownsandfake.Org) {
				o.CreateError = connect.NewWireError(connect.CodeUnavailable, errors.New("fixture has no spare capacity"))
				o.FileError = errors.New("injected JIT file write failure")
				o.DeleteError = connect.NewWireError(connect.CodeUnavailable, errors.New("injected sandbox deletion failure"))
			})
			var mu sync.Mutex
			var deletedIDs []int64
			w.gh.Update(func() {
				w.gh.JITGate = nil
				w.gh.FailDelete = http.StatusServiceUnavailable
				w.gh.DeleteGate = func(id int64) {
					mu.Lock()
					defer mu.Unlock()
					deletedIDs = append(deletedIDs, id)
				}
			})
			firstSandboxDelete := make(chan bool, 1)
			w.os.SetDeleteHook(func(context.Context) error {
				_, registered := w.gh.RunnerByName(runnerName)
				select {
				case firstSandboxDelete <- registered:
				default:
				}
				return nil
			})
			releaseJIT()

			eventually(t, "two GitHub deletion attempts with HTTP 503", func() bool {
				return strings.Count(w.ghLogs.String(), `"op":"delete_runner","status":503`) >= 2
			})
			readCleanup := func() (id int64, pending, removed bool) {
				t.Helper()
				if err := w.env.DB.Pool.QueryRow(t.Context(), `SELECT g.github_runner_id, r.sandbox_cleanup, g.registration_removed
					FROM runners r JOIN github_runners g USING (runner_id) WHERE r.runner_id = $1`, runnerID).Scan(&id, &pending, &removed); err != nil {
					t.Fatal(err)
				}
				return id, pending, removed
			}
			storedID, pending, removed := readCleanup()
			reg, registered := w.gh.RunnerByName(runnerName)
			if !registered || storedID != reg.ID || !pending || removed || w.os.Active(w.osOrg) != 1 || w.os.Calls("DeleteSandbox") != 0 {
				t.Fatal("retryable GitHub error failed to retain the registration, cleanup mark or sandbox")
			}

			// The give-up interval is for DELETING runners. Teardown of this
			// FAILED runner must retain the OwnSand link when GitHub still fails.
			w.env.Clock.Advance(2 * time.Minute)
			_, err := w.env.User(w.org.Admin).OwnSandLink.RemoveOwnSandLink(t.Context(), connect.NewRequest(&sandrunv1.RemoveOwnSandLinkRequest{
				OrganizationId: w.org.ID,
			}))
			if err == nil {
				t.Fatal("OwnSand link removal succeeded while GitHub was unavailable")
			}
			if _, pending, removed = readCleanup(); !pending || removed || w.os.Calls("DeleteSandbox") != 0 {
				t.Fatal("teardown lost the cleanup mark or began sandbox deletion")
			}
			t.Logf("GitHub returned HTTP 503 on repeated deletion of the stored registration. SandRun kept the sandbox and cleanup mark. RemoveOwnSandLink returned %s.", connect.CodeOf(err))

			if status == http.StatusNotFound {
				w.gh.ForgetRunner(storedID)
			}
			w.gh.Update(func() { w.gh.FailDelete = 0 })
			select {
			case stillRegistered := <-firstSandboxDelete:
				if stillRegistered {
					t.Fatal("OwnSand received a delete while the runner was registered")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("OwnSand did not receive a deletion after GitHub recovery")
			}
			if _, pending, removed = readCleanup(); !pending || !removed || w.os.Active(w.osOrg) != 1 {
				t.Fatal("failed OwnSand deletion did not retain the sandbox and completed registration removal")
			}

			calls := w.gh.Calls("delete_runner")
			w.os.Update(w.osOrg, func(o *ownsandfake.Org) { o.DeleteError = nil })
			eventually(t, "cleanup mark cleared and sandbox deleted", func() bool {
				_, pending, removed := readCleanup()
				return !pending && removed && w.os.Active(w.osOrg) == 0
			})
			if got := w.gh.Calls("delete_runner"); got != calls {
				t.Fatalf("GitHub deletions = %d, want %d after the registration was removed", got, calls)
			}
			mu.Lock()
			for _, id := range deletedIDs {
				if id != storedID {
					mu.Unlock()
					t.Fatalf("GitHub deletion targeted runner ID %d, want %d", id, storedID)
				}
			}
			mu.Unlock()
			if r := w.runner(t, runnerID); r.GetState() != sandrunv1.RunnerState_RUNNER_STATE_FAILED ||
				r.GetEndReason() != sandrunv1.RunnerEndReason_RUNNER_END_REASON_START_FAILED {
				t.Fatalf("runner after cleanup = %v, want FAILED with START_FAILED", r)
			}
			t.Logf("GitHub HTTP %d allowed OwnSand deletion. An OwnSand retry sent no additional GitHub deletion. GetRunner returned FAILED with START_FAILED after the mark cleared.", status)
		})
	}
}
