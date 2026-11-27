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

	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/servers/sandrun-api/internal/api"
	"github.com/delinoio/mono/servers/sandrun-api/internal/layers"
	"github.com/delinoio/mono/servers/sandrun-api/internal/lease"
	"github.com/delinoio/mono/servers/sandrun-api/internal/testenv"
)

func TestSurplusLeaseRecovery(t *testing.T) {
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

func TestSurplusAcknowledgmentPreservesNewerGeneration(t *testing.T) {
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
