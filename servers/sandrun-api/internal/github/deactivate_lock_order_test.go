// SPDX-License-Identifier: FSL-1.1-ALv2

package github_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/servers/sandrun-api/internal/api"
	"github.com/delinoio/mono/servers/sandrun-api/internal/profile"
	"github.com/delinoio/mono/servers/sandrun-api/internal/work"
)

func TestInstallationDeactivationAndProfileDeletionLockOrder(t *testing.T) {
	t.Parallel()
	for _, action := range []string{"unlink", "suspend", "deleted"} {
		for _, contention := range []string{"requests", "runners"} {
			t.Run(action+"/"+contention, func(t *testing.T) {
				testDeactivationLockOrder(t, action, contention)
			})
		}
	}
}

func testDeactivationLockOrder(t *testing.T, action, contention string) {
	t.Helper()
	w := newGHWorld(t, worldOpts{manualDispatch: true})
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	const githubID = 501
	const pool = "501"
	inst := w.link(t, githubID, "acme")
	profileID := w.createProfile(t, ghProfile("linux", 0))
	var refusedProfile string
	if action == "unlink" {
		refusedProfile = w.createProfile(t, ghProfile("refused", 0))
		w.send(t, "workflow_job", job("queued", githubID, 2, []string{"sandrun-refused"}, ""))
	}

	reconcileCtx, stopReconciler := context.WithCancel(ctx)
	reconcilerDone := make(chan struct{})
	go func() {
		defer close(reconcilerDone)
		_ = w.l.Engine.RunReconciler(reconcileCtx)
	}()
	defer func() {
		stopReconciler()
		<-reconcilerDone
	}()
	w.send(t, "workflow_job", job("queued", githubID, 1, sandRunLinux, ""))
	w.l.Engine.Pass(ctx, w.org.ID)
	waiting := sandrunv1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK
	wantWaiting := 1
	if refusedProfile != "" {
		wantWaiting++
	}
	eventually(t, "waiting GitHub registrations", func() bool {
		return len(w.runnersIn(t, waiting)) == wantWaiting
	})
	var busy, refused *sandrunv1.Runner
	for _, r := range w.runnersIn(t, waiting) {
		switch r.GetRunnerProfileId() {
		case profileID:
			busy = r
		case refusedProfile:
			refused = r
		}
	}
	// Freeze the runner drivers before adding the two post-Prepare rows.
	stopReconciler()
	<-reconcilerDone
	if busy == nil || (refusedProfile != "" && refused == nil) {
		t.Fatal("missing busy or refused runner control")
	}
	busyName := "sandrun-" + busy.GetRunnerId()
	w.gh.SetBusy(busyName, true)
	w.send(t, "workflow_job", job("in_progress", githubID, 1, sandRunLinux, busyName))
	if r := w.runner(t, busy.GetRunnerId()); r.GetState() != sandrunv1.RunnerState_RUNNER_STATE_BUSY {
		t.Fatalf("busy control did not take the job: %v", r)
	}
	if refused != nil {
		w.gh.SetBusy("sandrun-"+refused.GetRunnerId(), true)
	}
	if action == "suspend" {
		w.gh.SetSuspended(githubID, true)
	}

	prepared, continueDelete := make(chan struct{}), make(chan struct{})
	w.l.Profiles.OnDeletion(profile.DeletionHook{
		Name: "test gate after work cleanup",
		Prepare: func(ctx context.Context, _, _ string) error {
			close(prepared)
			select {
			case <-continueDelete:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	})
	admin := w.env.User(w.org.Admin)
	profileDone := make(chan error, 1)
	go func() {
		_, err := admin.Profile.DeleteRunnerProfile(ctx, connect.NewRequest(&sandrunv1.DeleteRunnerProfileRequest{
			OrganizationId: w.org.ID, RunnerProfileId: profileID,
		}))
		profileDone <- err
	}()
	awaitUnlinkStep(t, "profile deletion preparation", prepared)

	// Add two idle runners after Prepare so both final transactions can
	// lock them. Leave them unregistered so unlink reaches its final transaction
	// before trying either runner lock.
	lowRunner := addLateUnregisteredRunner(t, ctx, w, profileID, pool)
	highRunner := addLateUnregisteredRunner(t, ctx, w, profileID, pool)
	if lowRunner >= highRunner {
		t.Fatalf("expected ascending fixture runner IDs, got %s and %s", lowRunner, highRunner)
	}

	var queuedIDs []string
	lockSQL := "SELECT 1 FROM runners WHERE runner_id = $1 FOR UPDATE"
	lockID := lowRunner
	profileQuery := "SELECT runner_id::text FROM runners WHERE organization_id =%"
	deactivationQuery := "SELECT % FROM runners WHERE runner_id = $1 FOR UPDATE"
	if contention == "requests" {
		low, high := api.NewID(), api.NewID()
		queuedIDs = []string{low, high}
		p, err := w.l.Profiles.Get(ctx, w.env.DB.Pool, w.org.ID, profileID)
		if err != nil {
			t.Fatal(err)
		}
		// Insert in descending ID order to distinguish an unordered heap
		// scan from an ascending lock scan.
		if err := w.env.DB.InTx(ctx, func(tx pgx.Tx) error {
			for _, id := range []string{high, low} {
				if _, _, err := w.l.Engine.CreateRequest(ctx, tx, work.NewRequest{
					Profile: p, PoolKey: pool, Source: work.SourceGitHub, RequestID: id,
				}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		lockSQL = "SELECT 1 FROM requests WHERE request_id = $1 FOR UPDATE"
		lockID = low
		profileQuery = "SELECT request_id::text FROM requests WHERE organization_id =%runner_profile_id%"
		deactivationQuery = "SELECT request_id::text, state FROM requests%WHERE organization_id =%FOR UPDATE"
	}

	holder, err := w.env.DB.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(context.Background()) }()
	if _, err := holder.Exec(ctx, lockSQL, lockID); err != nil {
		t.Fatal(err)
	}
	startDeactivation := func() <-chan error {
		done := make(chan error, 1)
		go func() {
			if action == "unlink" {
				_, err := admin.GitHub.UnlinkGitHubInstallation(ctx, connect.NewRequest(&sandrunv1.UnlinkGitHubInstallationRequest{
					OrganizationId: w.org.ID, InstallationId: inst.GetInstallationId(),
				}))
				done <- err
				return
			}
			if _, status := w.gh.Send("installation", installationEvent(action, githubID, "acme")); status != http.StatusNoContent {
				done <- fmt.Errorf("%s webhook returned HTTP %d", action, status)
				return
			}
			done <- nil
		}()
		return done
	}

	blocker := int32(holder.Conn().PgConn().PID())
	var deactivationDone <-chan error
	wantReason := sandrunv1.RequestEndReason_REQUEST_END_REASON_PROFILE_DELETED
	if action == "unlink" && contention == "requests" {
		deactivationDone = startDeactivation()
		pid, _ := waitForSponsoredCleanupLock(t, ctx, w, []int32{blocker}, deactivationQuery)
		close(continueDelete)
		waitForSponsoredCleanupLock(t, ctx, w, []int32{blocker, pid}, profileQuery)
		wantReason = sandrunv1.RequestEndReason_REQUEST_END_REASON_INTEGRATION_REMOVED
	} else {
		close(continueDelete)
		pid, _ := waitForSponsoredCleanupLock(t, ctx, w, []int32{blocker}, profileQuery)
		deactivationDone = startDeactivation()
		waitForSponsoredCleanupLock(t, ctx, w, []int32{blocker, pid}, deactivationQuery)
	}

	if refused != nil {
		// The 422 retirement transaction has finished. Keep this runner locked
		// while final unlink commits to prove the final scan and cleanup skip it.
		refusedLock, err := w.env.DB.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = refusedLock.Rollback(context.Background()) }()
		if _, err := refusedLock.Exec(ctx, "SELECT 1 FROM runners WHERE runner_id = $1 FOR UPDATE NOWAIT", refused.GetRunnerId()); err != nil {
			t.Fatal(err)
		}
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	profileErr := awaitUnlinkStep(t, "profile deletion result", profileDone)
	deactivationErr := awaitUnlinkStep(t, "installation deactivation result", deactivationDone)
	if profileErr != nil || deactivationErr != nil {
		t.Fatalf("profile deletion: %v, installation deactivation: %v", profileErr, deactivationErr)
	}
	w.l.GitHub.Wait()
	_, err = admin.Profile.GetRunnerProfile(ctx, connect.NewRequest(&sandrunv1.GetRunnerProfileRequest{OrganizationId: w.org.ID, RunnerProfileId: profileID}))
	wantCode(t, "deleted runner profile", err, connect.CodeNotFound)
	installations, err := admin.GitHub.ListGitHubInstallations(ctx, connect.NewRequest(&sandrunv1.ListGitHubInstallationsRequest{OrganizationId: w.org.ID}))
	if err != nil {
		t.Fatal(err)
	}
	links := installations.Msg.GetInstallations()
	switch action {
	case "unlink":
		if len(links) != 0 {
			t.Fatalf("installation survived unlink: %v", links)
		}
	case "suspend", "deleted":
		state := sandrunv1.GitHubInstallationState_GIT_HUB_INSTALLATION_STATE_DELETED
		if action == "suspend" {
			state = sandrunv1.GitHubInstallationState_GIT_HUB_INSTALLATION_STATE_SUSPENDED
		}
		if len(links) != 1 || links[0].GetState() != state {
			t.Fatalf("installations %v, want one %s link", links, state)
		}
	}
	for _, id := range queuedIDs {
		r, err := w.l.Engine.GetRequest(ctx, w.env.DB.Pool, w.org.ID, id)
		if err != nil {
			t.Fatal(err)
		}
		if r.State != sandrunv1.RequestState_REQUEST_STATE_CANCELED || r.EndReason != wantReason {
			t.Errorf("request %s ended with %s and %s, want CANCELED and %s", id, r.State, r.EndReason, wantReason)
		}
	}
	for _, id := range []string{lowRunner, highRunner} {
		if r := w.runner(t, id); r.GetState() != sandrunv1.RunnerState_RUNNER_STATE_DELETED ||
			r.GetEndReason() != sandrunv1.RunnerEndReason_RUNNER_END_REASON_CLEAN_UP {
			t.Errorf("idle runner not deleted with CLEAN_UP: %v", r)
		}
	}
	if r := w.requestOfJob(t, 1); r.GetState() != sandrunv1.RequestState_REQUEST_STATE_ENDED ||
		r.GetEndReason() != sandrunv1.RequestEndReason_REQUEST_END_REASON_INTEGRATION_REMOVED {
		t.Errorf("busy runner's request did not end with INTEGRATION_REMOVED: %v", r)
	}
	if r := w.runner(t, busy.GetRunnerId()); r.GetState() != sandrunv1.RunnerState_RUNNER_STATE_BUSY {
		t.Errorf("busy runner did not survive: %v", r)
	}
	if _, ok := w.gh.RunnerByName(busyName); !ok {
		t.Error("busy registration did not survive")
	}
	wantActive := 1
	if refused != nil {
		wantActive++
		if r := w.runner(t, refused.GetRunnerId()); r.GetState() != waiting {
			t.Errorf("GitHub 422 control did not survive: %v", r)
		}
		if _, ok := w.gh.RunnerByName("sandrun-" + refused.GetRunnerId()); !ok ||
			!strings.Contains(w.ghLogs.String(), `"op":"delete_runner","status":422`) {
			t.Error("expected GitHub 422 to preserve the refused registration")
		}
	}
	if active := w.os.Active(w.osOrg); active != wantActive {
		t.Errorf("active sandboxes = %d, want %d", active, wantActive)
	}
	t.Logf("Both operations committed. Two idle sandboxes were released. The busy runner survived. Queued requests: %d, expected end reason: %s.", len(queuedIDs), wantReason)
}

func addLateUnregisteredRunner(t *testing.T, ctx context.Context, w *ghWorld, profileID, pool string) string {
	t.Helper()
	id, key := api.NewID(), api.NewID()
	client, err := w.os.New(w.osOrg, "osk_"+strings.Repeat("a", 43))
	if err != nil {
		t.Fatal(err)
	}
	sandbox, err := client.CreateSandbox(ctx, &ownsandv1.CreateSandboxRequest{IdempotencyKey: key})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.env.DB.Pool.Exec(ctx, `
		INSERT INTO runners (runner_id, organization_id, runner_profile_id, kind, guest_os, state, pool_key,
			idempotency_key, create_request, ownsand_sandbox_id, create_time, update_time)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, ''::bytea, $9, clock_timestamp(), clock_timestamp())`,
		id, w.org.ID, profileID, int16(sandrunv1.WorkloadKind_WORKLOAD_KIND_GITHUB_JOB), int16(sandrunv1.GuestOs_GUEST_OS_LINUX),
		int16(sandrunv1.RunnerState_RUNNER_STATE_PREPARING), pool, key, sandbox.GetSandboxId()); err != nil {
		t.Fatal(err)
	}
	return id
}
