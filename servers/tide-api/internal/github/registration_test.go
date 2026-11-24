// SPDX-License-Identifier: FSL-1.1-ALv2

package github_test

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/servers/tide-api/internal/githubapi/githubfake"
	"github.com/delinoio/mono/servers/tide-api/internal/ownsandclient/ownsandfake"
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
			w.send(t, "workflow_job", job("queued", 501, 1, tideLinux, ""))
			select {
			case <-jitEntered:
			case <-time.After(10 * time.Second):
				t.Fatal("timed out waiting for the JIT request")
			}
			starting := w.runnersIn(t, tidev1.RunnerState_RUNNER_STATE_PREPARING)
			if len(starting) != 1 {
				t.Fatalf("got %d preparing runners, want 1", len(starting))
			}
			runnerID := starting[0].GetRunnerId()
			runnerName := "tide-" + runnerID
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

			if r := w.runner(t, runnerID); r.GetState() != tidev1.RunnerState_RUNNER_STATE_FAILED ||
				r.GetEndReason() != tidev1.RunnerEndReason_RUNNER_END_REASON_START_FAILED {
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
