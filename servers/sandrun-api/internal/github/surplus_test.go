// SPDX-License-Identifier: FSL-1.1-ALv2

package github_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/servers/sandrun-api/internal/api"
	"github.com/delinoio/mono/servers/sandrun-api/internal/layers"
	"github.com/delinoio/mono/servers/sandrun-api/internal/lease"
	"github.com/delinoio/mono/servers/sandrun-api/internal/testenv"
)

func TestSurplusLeaseRecovery(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		warm  int32
		retry bool
	}{
		{name: "last queued job"},
		{name: "queued job and warm reserve", warm: 1, retry: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newGHWorld(t, worldOpts{manualDispatch: true})
			w.link(t, 501, "acme")
			w.createProfile(t, ghProfile("linux", tc.warm))
			stopDrivers := runSurplusTestDrivers(t, w)
			waiting := sandrunv1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK
			for i := 0; i < int(tc.warm)+1; i++ {
				w.send(t, "workflow_job", job("queued", 501, int64(i+1), sandRunLinux, ""))
			}
			w.l.Engine.Pass(t.Context(), w.org.ID)
			// The dispatcher fills the reserve after queued demand is covered.
			w.l.Engine.Pass(t.Context(), w.org.ID)
			eventually(t, "idle pool runners", func() bool {
				return len(w.runnersIn(t, waiting)) == 2*int(tc.warm)+1
			})
			idle := w.runnersIn(t, waiting)
			newest := idle[0]
			for _, r := range idle[1:] {
				if r.GetCreatedAt().AsTime().After(newest.GetCreatedAt().AsTime()) ||
					(r.GetCreatedAt().AsTime().Equal(newest.GetCreatedAt().AsTime()) && r.GetRunnerId() > newest.GetRunnerId()) {
					newest = r
				}
			}
			runnerName := "sandrun-" + newest.GetRunnerId()
			if _, ok := w.gh.RunnerByName(runnerName); !ok {
				t.Fatal("newest runner has no GitHub registration")
			}

			var sandboxDeleted, orderViolation atomic.Bool
			w.os.SetDeleteHook(func(context.Context) error {
				if _, ok := w.gh.RunnerByName(runnerName); ok {
					orderViolation.Store(true)
				}
				sandboxDeleted.Store(true)
				return nil
			})
			if tc.retry {
				w.gh.Update(func() { w.gh.FailDelete = http.StatusServiceUnavailable })
			}
			completion := job("completed", 501, 1, sandRunLinux, "")
			guid := w.send(t, "workflow_job", completion)
			w.l.GitHub.Wait()
			ended := w.requestOfJob(t, 1)
			if ended.GetState() != sandrunv1.RequestState_REQUEST_STATE_CANCELED ||
				ended.GetEndReason() != sandrunv1.RequestEndReason_REQUEST_END_REASON_CANCELED_BY_SOURCE ||
				pendingSurplusGeneration(t, w) == "" || sandboxDeleted.Load() {
				t.Fatalf("completion did not leave durable cleanup work: %v", ended)
			}

			stopDrivers()
			w.env.Stop()
			if _, err := w.env.DB.Pool.Exec(t.Context(), "DELETE FROM instance_leases WHERE lease_name = $1 AND holder = 'manual-dispatch'", lease.Name); err != nil {
				t.Fatal(err)
			}
			replica := w.env.NewReplica(t, testenv.WithLayerOptions(layers.Options{OwnSand: w.os, GitHub: w.githubOptions(w.env.Clock.Now)}, &layers.Layers{}))
			w.env = replica
			w.gh.Target = replica.Server.URL + "/webhooks/github"
			w.gh.TargetClient = replica.Server.Client()

			if tc.retry {
				eventually(t, "transient GitHub removal failure", func() bool {
					return strings.Contains(replica.Logs.String(), "github_surplus_delete_failed")
				})
				if w.runner(t, newest.GetRunnerId()).GetState() != waiting || sandboxDeleted.Load() || pendingSurplusGeneration(t, w) == "" {
					t.Fatal("failed registration removal lost the runner or pending check")
				}
				w.gh.Update(func() { w.gh.FailDelete = 0 })
			}
			eventually(t, "newest runner deleted after lease acquisition", func() bool {
				return w.runner(t, newest.GetRunnerId()).GetState() == sandrunv1.RunnerState_RUNNER_STATE_DELETED
			})
			if r := w.runner(t, newest.GetRunnerId()); r.GetEndReason() != sandrunv1.RunnerEndReason_RUNNER_END_REASON_SURPLUS ||
				!sandboxDeleted.Load() || orderViolation.Load() {
				t.Fatalf("runner %v, sandbox deleted %v, deletion before registration removal %v", r, sandboxDeleted.Load(), orderViolation.Load())
			}
			for _, r := range idle {
				if r != newest && w.runner(t, r.GetRunnerId()).GetState() != waiting {
					t.Fatalf("runner %s did not stay for queued demand or the warm reserve", r.GetRunnerId())
				}
			}
			if tc.warm != 0 && w.requestOfJob(t, 2).GetState() != sandrunv1.RequestState_REQUEST_STATE_QUEUED {
				t.Fatal("recovery changed the remaining queued request")
			}

			eventually(t, "surplus generation acknowledged", func() bool { return pendingSurplusGeneration(t, w) == "" })
			body, err := json.Marshal(completion)
			if err != nil {
				t.Fatal(err)
			}
			if status := w.gh.SendRaw(w.gh.Headers("workflow_job", guid, body), body); status != http.StatusNoContent {
				t.Fatalf("duplicate status = %d", status)
			}
			got := w.requestOfJob(t, 1)
			if got.GetRequestId() != ended.GetRequestId() || !got.GetEndedAt().AsTime().Equal(ended.GetEndedAt().AsTime()) ||
				got.GetEndReason() != ended.GetEndReason() || len(w.requests(t)) != int(tc.warm)+1 || pendingSurplusGeneration(t, w) != "" {
				t.Fatalf("redelivery changed the terminal request or queued another check: %v", got)
			}
		})
	}
}

func TestSurplusRetriesRenamedRunnerRemoval(t *testing.T) {
	t.Parallel()
	w := newGHWorld(t, worldOpts{manualDispatch: true})
	w.link(t, 501, "acme")
	profileID := w.createProfile(t, ghProfile("linux", 1))
	runSurplusTestDrivers(t, w)
	waiting := sandrunv1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK
	w.l.Engine.Pass(t.Context(), w.org.ID)
	eventually(t, "old-label warm runner", func() bool { return len(w.runnersIn(t, waiting)) == 1 })
	old := w.runnersIn(t, waiting)[0]
	oldName := "sandrun-" + old.GetRunnerId()
	oldReg, ok := w.gh.RunnerByName(oldName)
	if !ok {
		t.Fatal("old-label runner has no GitHub registration")
	}

	renameGitHubProfile(t, w, profileID)
	// Keep the old runner through queued-event cleanup while the new runner starts.
	w.gh.SetBusy(oldName, true)
	w.send(t, "workflow_job", job("queued", 501, 1, []string{renamedLabel}, ""))
	w.l.GitHub.Wait()
	w.gh.SetBusy(oldName, false)
	w.env.Clock.Advance(time.Second)
	w.l.Engine.Pass(t.Context(), w.org.ID)
	assertRenamedCapacity(t, w, old.GetRunnerId())
	var current *sandrunv1.Runner
	for _, r := range w.runnersIn(t, waiting) {
		if r.GetRunnerId() != old.GetRunnerId() {
			current = r
		}
	}
	if current == nil || !current.GetCreatedAt().AsTime().After(old.GetCreatedAt().AsTime()) {
		t.Fatalf("expected a newer current-label runner, got %v", current)
	}

	var oldAttempts, otherAttempts atomic.Int32
	var orderViolation atomic.Bool
	w.os.SetDeleteHook(func(context.Context) error {
		if _, ok := w.gh.RunnerByName(oldName); ok {
			orderViolation.Store(true)
		}
		return nil
	})
	var failOld atomic.Bool
	failOld.Store(true)
	w.gh.Update(func() {
		w.gh.DeleteGate = func(id int64) {
			status := 0
			if id == oldReg.ID {
				oldAttempts.Add(1)
				if failOld.Load() {
					status = http.StatusServiceUnavailable
				}
			} else {
				otherAttempts.Add(1)
			}
			w.gh.Update(func() { w.gh.FailDelete = status })
		}
	})
	w.send(t, "workflow_job", job("completed", 501, 1, []string{renamedLabel}, ""))
	var generation string
	var due time.Time
	const checkQuery = "SELECT generation::text, check_after FROM github_surplus_checks WHERE organization_id = $1"
	if err := w.env.DB.Pool.QueryRow(t.Context(), checkQuery, w.org.ID).Scan(&generation, &due); err != nil {
		t.Fatal(err)
	}

	w.l.GitHub.CheckSurplusOnce(t.Context())
	if oldAttempts.Load() != 1 || otherAttempts.Load() != 0 {
		t.Fatalf("GitHub delete attempts for old runner = %d, for others = %d, want 1 and 0", oldAttempts.Load(), otherAttempts.Load())
	}
	assertRegistrationLabel(t, w, old.GetRunnerId(), "sandrun-linux", "acme")
	assertRegistrationLabel(t, w, current.GetRunnerId(), renamedLabel, "acme")
	if w.runner(t, old.GetRunnerId()).GetState() != waiting || w.runner(t, current.GetRunnerId()).GetState() != waiting ||
		w.os.Active(w.osOrg) != 2 || w.os.Calls("DeleteSandbox") != 0 {
		t.Fatal("failed rename cleanup changed a waiting runner or its sandbox")
	}
	var retryGeneration string
	var retryDue time.Time
	err := w.env.DB.Pool.QueryRow(t.Context(), checkQuery, w.org.ID).Scan(&retryGeneration, &retryDue)
	if err != nil || retryGeneration != generation || !retryDue.After(due) {
		t.Fatalf("retry generation = %q, due = %v, want %q after %v, error = %v", retryGeneration, retryDue, generation, due, err)
	}

	failOld.Store(false)
	eventually(t, "same surplus check due for retry", func() bool {
		var due bool
		if err := w.env.DB.Pool.QueryRow(t.Context(), "SELECT check_after <= now() FROM github_surplus_checks WHERE generation = $1", generation).Scan(&due); err != nil {
			t.Fatal(err)
		}
		return due
	})
	w.l.GitHub.CheckSurplusOnce(t.Context())
	eventually(t, "old runner deleted", func() bool {
		return w.runner(t, old.GetRunnerId()).GetState() == sandrunv1.RunnerState_RUNNER_STATE_DELETED
	})
	if oldAttempts.Load() != 2 || otherAttempts.Load() != 0 {
		t.Fatalf("GitHub delete attempts after retry: old = %d, others = %d, want 2 and 0", oldAttempts.Load(), otherAttempts.Load())
	}
	if r := w.runner(t, old.GetRunnerId()); r.GetEndReason() != sandrunv1.RunnerEndReason_RUNNER_END_REASON_CLEAN_UP {
		t.Fatalf("old runner end reason = %v, want CLEAN_UP", r.GetEndReason())
	}
	if _, ok := w.gh.RunnerByName(oldName); ok || orderViolation.Load() || w.os.Active(w.osOrg) != 1 {
		t.Fatal("retry did not remove the old registration before deleting its sandbox")
	}
	for _, sandbox := range w.os.Sandboxes(w.osOrg) {
		if sandbox.GetSandboxId() == old.GetOwnsandSandboxId() && sandbox.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_DELETED {
			t.Fatal("retry did not delete the old runner's sandbox")
		}
	}
	assertRegistrationLabel(t, w, current.GetRunnerId(), renamedLabel, "acme")
	if w.runner(t, current.GetRunnerId()).GetState() != waiting || pendingSurplusGeneration(t, w) != "" {
		t.Fatal("retry did not preserve the warm reserve and acknowledge the generation")
	}
}

func TestSurplusAcknowledgesBusyRenamedRunner(t *testing.T) {
	t.Parallel()
	w := newGHWorld(t, worldOpts{manualDispatch: true})
	w.link(t, 501, "acme")
	profileID := w.createProfile(t, ghProfile("linux", 1))
	runSurplusTestDrivers(t, w)
	waiting := sandrunv1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK
	w.l.Engine.Pass(t.Context(), w.org.ID)
	eventually(t, "old-label warm runner", func() bool { return len(w.runnersIn(t, waiting)) == 1 })
	oldID := w.runnersIn(t, waiting)[0].GetRunnerId()
	renameGitHubProfile(t, w, profileID)
	w.gh.SetBusy("sandrun-"+oldID, true)

	w.send(t, "workflow_job", job("completed", 501, 1, []string{renamedLabel}, ""))
	if pendingSurplusGeneration(t, w) == "" {
		t.Fatal("completion did not schedule a surplus check")
	}
	w.l.GitHub.CheckSurplusOnce(t.Context())
	if w.gh.Calls("delete_runner") != 1 || !strings.Contains(w.env.Logs.String(), "github_runner_removal_refused") {
		t.Fatal("GitHub refusal did not take the busy path")
	}
	assertRegistrationLabel(t, w, oldID, "sandrun-linux", "acme")
	if w.runner(t, oldID).GetState() != waiting || w.os.Active(w.osOrg) != 1 || w.os.Calls("DeleteSandbox") != 0 {
		t.Fatal("GitHub refusal changed the old runner or its sandbox")
	}
	if pendingSurplusGeneration(t, w) != "" {
		t.Fatal("GitHub refusal left a pending retry")
	}
}

func TestSurplusAcknowledgmentPreservesNewerGeneration(t *testing.T) {
	t.Parallel()
	w := newGHWorld(t, worldOpts{manualDispatch: true})
	w.link(t, 501, "acme")
	w.createProfile(t, ghProfile("linux", 0))
	runSurplusTestDrivers(t, w)
	for _, id := range []int64{1, 2} {
		w.send(t, "workflow_job", job("queued", 501, id, sandRunLinux, ""))
	}
	w.l.Engine.Pass(t.Context(), w.org.ID)
	eventually(t, "two idle runners", func() bool {
		return len(w.runnersIn(t, sandrunv1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK)) == 2
	})
	w.send(t, "workflow_job", job("completed", 501, 1, sandRunLinux, ""))
	older := pendingSurplusGeneration(t, w)
	entered, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	w.gh.Update(func() {
		w.gh.DeleteGate = func(int64) {
			close(entered)
			<-release
		}
	})
	t.Cleanup(unblock)
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.l.GitHub.CheckSurplusOnce(t.Context())
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("first pass did not attempt registration removal")
	}
	w.send(t, "workflow_job", job("completed", 501, 2, sandRunLinux, ""))
	newer := pendingSurplusGeneration(t, w)
	if older == "" || newer == "" || newer == older {
		t.Fatalf("generations before and after second completion: %q, %q", older, newer)
	}
	w.gh.Update(func() { w.gh.DeleteGate = nil })
	unblock()
	<-done
	if got := pendingSurplusGeneration(t, w); got != newer {
		t.Fatalf("old acknowledgment left generation %q, want %q", got, newer)
	}
	w.l.GitHub.CheckSurplusOnce(t.Context())
	eventually(t, "both surplus runners deleted", func() bool {
		return len(w.runnersIn(t, sandrunv1.RunnerState_RUNNER_STATE_DELETED)) == 2
	})
	if got := pendingSurplusGeneration(t, w); got != "" {
		t.Fatalf("new generation was not acknowledged: %q", got)
	}
}

func TestSurplusLocksOrganizationBeforeInstallation(t *testing.T) {
	t.Parallel()
	w := newGHWorld(t, worldOpts{manualDispatch: true})
	w.link(t, 501, "acme")
	w.createProfile(t, ghProfile("linux", 0))
	runSurplusTestDrivers(t, w)
	w.send(t, "workflow_job", job("queued", 501, 1, sandRunLinux, ""))
	w.l.GitHub.Wait()
	w.l.Engine.Pass(t.Context(), w.org.ID)
	waiting := sandrunv1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK
	eventually(t, "idle runner", func() bool { return len(w.runnersIn(t, waiting)) == 1 })
	runnerID := w.runnersIn(t, waiting)[0].GetRunnerId()
	w.send(t, "workflow_job", job("completed", 501, 1, sandRunLinux, ""))

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	pool := w.env.DB.Pool
	barrier, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = barrier.Rollback(context.Background()) }()
	if _, err := barrier.Exec(ctx, "SELECT 1 FROM github_installations WHERE github_installation_id = 501 FOR UPDATE"); err != nil {
		t.Fatal(err)
	}
	checked := make(chan struct{})
	go func() {
		defer close(checked)
		w.l.GitHub.CheckSurplusOnce(ctx)
	}()
	workerPID, query := waitForInstallationTestBlock(t, w, ctx, barrier.Conn().PgConn().PID(), "surplus check at the installation lock")
	if !strings.Contains(query, "FROM github_installations") || !strings.Contains(query, "FOR SHARE") {
		t.Errorf("surplus check waited on %q, want the installation share lock", query)
	}

	// Organization deletion takes this lock first. It must wait for the
	// surplus check even while the check waits for the installation.
	locked := make(chan error, 1)
	go func() {
		locked <- pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			return api.LockOrganization(ctx, tx, w.org.ID)
		})
	}()
	_, query = waitForInstallationTestBlock(t, w, ctx, workerPID, "organization update behind the surplus check")
	if !strings.Contains(query, "FROM organizations") || !strings.Contains(query, "FOR UPDATE") {
		t.Errorf("organization update waited on %q, want the organization update lock", query)
	}

	// Change only the link state so the test can observe any runner cleanup
	// initiated by the surplus check.
	suspended := sandrunv1.GitHubInstallationState_GIT_HUB_INSTALLATION_STATE_SUSPENDED
	if _, err := barrier.Exec(ctx, "UPDATE github_installations SET state = $1 WHERE github_installation_id = 501", int16(suspended)); err != nil {
		t.Fatal(err)
	}
	if err := barrier.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-checked:
	case <-ctx.Done():
		t.Fatal("surplus check did not complete after release")
	}
	if err := <-locked; err != nil {
		t.Fatalf("lock organization: %v", err)
	}
	if got := pendingSurplusGeneration(t, w); got != "" {
		t.Fatalf("pending check for a suspended installation: %q", got)
	}
	if runner := w.runner(t, runnerID); runner.GetState() != waiting {
		t.Fatalf("surplus check changed the runner of a suspended installation: %v", runner)
	}
	if _, ok := w.gh.RunnerByName("sandrun-" + runnerID); !ok {
		t.Fatal("surplus check removed the registration of a suspended installation")
	}
}

func runSurplusTestDrivers(t *testing.T, w *ghWorld) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = w.l.Engine.RunReconciler(ctx)
	}()
	stop := sync.OnceFunc(func() {
		cancel()
		<-done
	})
	t.Cleanup(stop)
	return stop
}

func pendingSurplusGeneration(t *testing.T, w *ghWorld) string {
	t.Helper()
	var id string
	err := w.env.DB.Pool.QueryRow(t.Context(), "SELECT generation::text FROM github_surplus_checks WHERE organization_id = $1", w.org.ID).Scan(&id)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal(err)
	}
	return id
}
