// SPDX-License-Identifier: FSL-1.1-ALv2

package github_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/servers/sandrun-api/internal/api"
	"github.com/delinoio/mono/servers/sandrun-api/internal/app"
	"github.com/delinoio/mono/servers/sandrun-api/internal/githubapi/githubfake"
	"github.com/delinoio/mono/servers/sandrun-api/internal/jobs"
	"github.com/delinoio/mono/servers/sandrun-api/internal/ownsandclient/ownsandfake"
)

func TestJITRegistrationRollback(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	for _, status := range []int{http.StatusNoContent, http.StatusNotFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			nextRetention := make(chan struct{})
			w := newGHWorld(t, worldOpts{}, func(o *app.Options) {
				o.DailyJobInterval = 50 * time.Millisecond
				o.Extensions = append(o.Extensions, func(r *app.Registry) error {
					// Pause after the registered jobs so cleanup can finish
					// before the test allows the next retention pass.
					r.AddDailyJob(jobs.Job{Name: "test_retention_barrier", Run: func(ctx context.Context, _ time.Time) (int64, error) {
						select {
						case <-nextRetention:
							return 0, nil
						case <-ctx.Done():
							return 0, ctx.Err()
						}
					}})
					return nil
				})
			})
			runNextRetention := func() {
				t.Helper()
				select {
				case nextRetention <- struct{}{}:
				case <-time.After(10 * time.Second):
					t.Fatal("timed out waiting for the daily jobs")
				}
			}
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
			deleteError := connect.NewWireError(connect.CodeUnavailable, errors.New("injected sandbox deletion failure"))
			w.os.Update(w.osOrg, func(o *ownsandfake.Org) {
				o.CreateError = connect.NewWireError(connect.CodeUnavailable, errors.New("fixture has no spare capacity"))
				o.FileError = errors.New("injected JIT file write failure")
				o.DeleteError = deleteError
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
					t.Fatalf("read runner and its GitHub registration: %v", err)
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

			endedAt := w.runner(t, runnerID).GetEndedAt()
			if endedAt == nil {
				t.Fatal("failed runner has no end time")
			}
			jobTime := endedAt.AsTime().Add(31 * 24 * time.Hour)
			w.env.Clock.Advance(jobTime.Sub(w.env.Clock.Now()))
			db := w.env.DB.Pool
			ctx := t.Context()
			older, boundary, younger, active, expiredRequest := api.NewID(), api.NewID(), api.NewID(), api.NewID(), api.NewID()
			// The active control uses the existing fake sandbox so the reconciler
			// does not end it before the first retention pass.
			if tag, err := db.Exec(ctx, `
				INSERT INTO runners (runner_id, organization_id, runner_profile_id, kind, guest_os, state,
					idempotency_key, create_request, ownsand_sandbox_id, create_time, end_time, update_time)
				SELECT control.id, r.organization_id, r.runner_profile_id, r.kind, r.guest_os,
					CASE WHEN control.end_time IS NULL THEN $7::smallint ELSE r.state END,
					control.id, r.create_request, CASE WHEN control.end_time IS NULL THEN r.ownsand_sandbox_id END,
					r.create_time, control.end_time, r.update_time
				FROM runners r CROSS JOIN (VALUES
					($2::uuid, $6::timestamptz - interval '31 days'),
					($3::uuid, $6::timestamptz - interval '30 days'),
					($4::uuid, $6::timestamptz - interval '29 days'),
					($5::uuid, NULL)
				) control(id, end_time)
				WHERE r.runner_id = $1`, runnerID, older, boundary, younger, active, jobTime,
				int16(sandrunv1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK)); err != nil || tag.RowsAffected() != 4 {
				t.Fatalf("insert retention control runners: %v, %v", tag, err)
			}
			if tag, err := db.Exec(ctx, `
				INSERT INTO requests (request_id, organization_id, runner_profile_id, kind, source, source_key,
					state, priority, queue_time, queue_deadline, end_time, create_time, update_time)
				SELECT $2::uuid, organization_id, runner_profile_id, kind, source, $2::uuid::text,
					$3, priority, queue_time, queue_deadline, $4, create_time, update_time
				FROM requests WHERE organization_id = $1 LIMIT 1`, w.org.ID, expiredRequest,
				int16(sandrunv1.RequestState_REQUEST_STATE_FAILED), endedAt.AsTime()); err != nil || tag.RowsAffected() != 1 {
				t.Fatalf("insert expired request: %v, %v", tag, err)
			}
			rowsExist := func(query, id string) bool {
				t.Helper()
				var exists bool
				if err := db.QueryRow(ctx, query, id).Scan(&exists); err != nil {
					t.Fatal(err)
				}
				return exists
			}
			const runnerExists = "SELECT EXISTS (SELECT 1 FROM runners WHERE runner_id = $1)"
			runNextRetention()
			eventually(t, "unmarked 31-day-old runner deleted by retention", func() bool {
				return !rowsExist(runnerExists, older)
			})
			if id, pending, removed := readCleanup(); id != storedID || !pending || removed {
				t.Fatalf("after retention: GitHub ID = %d, cleanup pending = %v, registration removed = %v", id, pending, removed)
			}
			if !rowsExist(runnerExists, boundary) || !rowsExist(runnerExists, younger) ||
				!rowsExist("SELECT EXISTS (SELECT 1 FROM runners WHERE runner_id = $1 AND end_time IS NULL)", active) {
				t.Fatal("retention removed a runner at or below the cutoff or with no end time")
			}
			if rowsExist("SELECT EXISTS (SELECT 1 FROM requests WHERE request_id = $1)", expiredRequest) {
				t.Fatal("request retention kept an expired request")
			}
			t.Log("At 31 days, daily retention deleted the unmarked expired runner and request and kept the marked runner, registration, and all three runner controls.")

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
			// The hook runs before the fake reads DeleteError. Wait for the
			// driver to log the result before allowing the next delete to succeed.
			eventually(t, "OwnSand sandbox deletion failure for runner "+runnerID, func() bool {
				for line := range strings.SplitSeq(w.env.Logs.String(), "\n") {
					var entry struct {
						Message  string `json:"msg"`
						RunnerID string `json:"runner_id"`
						Error    string `json:"error"`
					}
					if json.Unmarshal([]byte(line), &entry) == nil &&
						entry.Message == "runner_sandbox_cleanup_failed" && entry.RunnerID == runnerID &&
						strings.Contains(entry.Error, deleteError.Error()) {
						return true
					}
				}
				return false
			})
			_, registered = w.gh.RunnerByName(runnerName)
			if _, pending, removed = readCleanup(); !pending || !removed || registered || w.os.Active(w.osOrg) != 1 {
				t.Fatal("failed OwnSand deletion did not retain the sandbox and completed registration removal")
			}

			githubDeletes := w.gh.Calls("delete_runner")
			failedDeleteCalls := w.os.Calls("DeleteSandbox")
			w.os.Update(w.osOrg, func(o *ownsandfake.Org) { o.DeleteError = nil })
			eventually(t, "cleanup mark cleared and sandbox deleted", func() bool {
				_, pending, removed := readCleanup()
				return !pending && removed && w.os.Active(w.osOrg) == 0
			})
			if got := w.os.Calls("DeleteSandbox"); got < 2 || got <= failedDeleteCalls {
				t.Fatalf("OwnSand deletions = %d, want a retry after %d calls with the error active", got, failedDeleteCalls)
			}
			if got := w.gh.Calls("delete_runner"); got != githubDeletes {
				t.Fatalf("GitHub deletions = %d, want %d after the registration was removed", got, githubDeletes)
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
				r.GetEndReason() != sandrunv1.RunnerEndReason_RUNNER_END_REASON_START_FAILED ||
				!r.GetEndedAt().AsTime().Equal(endedAt.AsTime()) {
				t.Fatalf("runner after cleanup = %v, want FAILED with START_FAILED and original end time %v", r, endedAt)
			}
			t.Logf("GitHub HTTP %d allowed OwnSand deletion. An OwnSand retry sent no additional GitHub deletion. GetRunner returned FAILED with START_FAILED after the mark cleared.", status)
			runNextRetention()
			eventually(t, "expired runner deleted after cleanup", func() bool {
				return !rowsExist(runnerExists, runnerID)
			})
			if rowsExist("SELECT EXISTS (SELECT 1 FROM github_runners WHERE runner_id = $1)", runnerID) {
				t.Fatal("runner deletion did not cascade to its registration")
			}
			t.Log("At the same job time, the next retention pass deleted the cleaned-up runner and cascaded to its registration.")
		})
	}
}
