// SPDX-License-Identifier: FSL-1.1-ALv2

package github_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/servers/sandrun-api/internal/profile"
)

func TestSponsoredNoDeviceDuringProfileDeletion(t *testing.T) {
	for _, tc := range []struct {
		name         string
		cleanupFirst bool
		expired      bool
		reconcile    bool
	}{
		{name: "deletion_first"},
		{name: "cleanup_first", cleanupFirst: true},
		{name: "cleanup_first_expired", cleanupFirst: true, expired: true},
		{name: "deletion_first_reconcile", reconcile: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newGHWorld(t, worldOpts{manualDispatch: true})
			const installation, sponsor, other = 501, 9001, 9002
			w.link(t, installation, "acme")
			profileID := w.createProfile(t, ghProfile("linux", 0))
			w.createProfile(t, ghProfile("other", 0))
			w.send(t, "workflow_job", job("queued", installation, sponsor, sandRunLinux, ""))
			sponsorID := w.requestOfJob(t, sponsor).GetRequestId()
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			var workers sync.WaitGroup
			defer func() {
				cancel()
				workers.Wait()
			}()

			prepared, deleteProfile := make(chan struct{}), make(chan struct{})
			w.l.Profiles.OnDeletion(profile.DeletionHook{
				Name: "gate after work cleanup",
				Prepare: func(ctx context.Context, _, _ string) error {
					close(prepared)
					select {
					case <-deleteProfile:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				},
			})
			deletion := make(chan error, 1)
			workers.Go(func() {
				_, err := w.env.User(w.org.Admin).Profile.DeleteRunnerProfile(ctx, connect.NewRequest(&sandrunv1.DeleteRunnerProfileRequest{
					OrganizationId: w.org.ID, RunnerProfileId: profileID,
				}))
				deletion <- err
			})
			select {
			case <-prepared:
			case <-ctx.Done():
				t.Fatal("deletion did not reach the prepare gate")
			}

			var calls atomic.Int32
			creates := make(chan *ownsandv1.CreateSandboxRequest, 8)
			rejectCreate := make(chan struct{})
			factory := newCreateSandboxPeer(t, func(ctx context.Context, req *ownsandv1.CreateSandboxRequest) error {
				call := calls.Add(1)
				select {
				case creates <- req:
				case <-ctx.Done():
					return ctx.Err()
				}
				if tc.reconcile && call == 1 {
					return context.DeadlineExceeded
				}
				select {
				case <-rejectCreate:
					return connect.NewError(connect.CodeFailedPrecondition, errors.New("no eligible device"))
				case <-ctx.Done():
					return ctx.Err()
				}
			})
			client, err := factory.New(w.osOrg, "osk_"+strings.Repeat("a", 43))
			if err != nil {
				t.Fatal(err)
			}
			w.os.SetCreateRequestHook(func(ctx context.Context, req *ownsandv1.CreateSandboxRequest) error {
				_, err := client.CreateSandbox(ctx, req)
				return err
			})
			awaitCreate := func() *ownsandv1.CreateSandboxRequest {
				t.Helper()
				select {
				case req := <-creates:
					return req
				case <-ctx.Done():
					t.Fatal("OwnSand never received CreateSandbox")
					return nil
				}
			}
			dispatchDone := make(chan struct{})
			workers.Go(func() {
				defer close(dispatchDone)
				w.l.Engine.Pass(ctx, w.org.ID)
			})
			first := awaitCreate()
			if tc.reconcile {
				select {
				case <-dispatchDone:
				case <-ctx.Done():
					t.Fatal("dispatcher did not return after the deadline error")
				}
				workers.Go(func() { _ = w.l.Engine.RunReconciler(ctx) })
				if retry := awaitCreate(); first.GetIdempotencyKey() == "" || !proto.Equal(first, retry) {
					t.Fatal("reconciliation changed the stored create or its idempotency key")
				}
				t.Log("The reconciler reused the original OwnSand create parameters and idempotency key.")
			}
			var runnerID, storedSponsor string
			if err := w.env.DB.Pool.QueryRow(ctx, `SELECT runner_id::text, sponsor_request_id::text FROM runners
				WHERE runner_profile_id = $1 AND request_id IS NULL AND ownsand_sandbox_id IS NULL`, profileID).Scan(&runnerID, &storedSponsor); err != nil || storedSponsor != sponsorID {
				t.Fatalf("late unbound reservation: runner %q, sponsor %q, error %v", runnerID, storedSponsor, err)
			}
			if tc.expired {
				w.env.Clock.Advance(2 * time.Hour)
			}
			w.send(t, "workflow_job", job("queued", installation, other, []string{"self-hosted", "sandrun-other"}, ""))
			otherBefore := w.requestOfJob(t, other)
			if otherBefore.GetState() != sandrunv1.RequestState_REQUEST_STATE_QUEUED {
				t.Fatalf("unrelated job before cleanup: %v", otherBefore)
			}

			// Hold the sponsor so whichever operation starts first keeps its
			// other locks until the second operation reaches its lock wait.
			barrier, err := w.env.DB.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = barrier.Rollback(context.Background()) }()
			if _, err := barrier.Exec(ctx, "UPDATE requests SET start_attempts = 1 WHERE request_id = $1", sponsorID); err != nil {
				t.Fatal(err)
			}
			barrierPID := int32(barrier.Conn().PgConn().PID())
			if tc.cleanupFirst {
				close(rejectCreate)
				cleanupPID, _ := waitForSponsoredCleanupLock(t, ctx, w, []int32{barrierPID}, "%FROM requests WHERE request_id =% FOR UPDATE")
				close(deleteProfile)
				waitForSponsoredCleanupLock(t, ctx, w, []int32{cleanupPID}, "SELECT 1 FROM runner_profiles WHERE % FOR UPDATE")
			} else {
				close(deleteProfile)
				deletePID, _ := waitForSponsoredCleanupLock(t, ctx, w, []int32{barrierPID}, "SELECT request_id::text FROM requests WHERE % FOR UPDATE")
				close(rejectCreate)
				// Without the profile lock, cleanup waits for the sponsor while
				// holding the runner. Release the barrier in either case so
				// PostgreSQL can surface the resulting deadlock.
				_, query := waitForSponsoredCleanupLock(t, ctx, w, []int32{deletePID, barrierPID},
					"SELECT 1 FROM runner_profiles WHERE % FOR SHARE", "%FROM requests WHERE request_id =% FOR UPDATE")
				if !strings.HasSuffix(query, "FOR SHARE") {
					t.Error("cleanup waited for the sponsor before taking the profile lock")
				}
			}
			if err := barrier.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-deletion:
				if err != nil {
					t.Errorf("delete profile: %v", err)
				}
			case <-ctx.Done():
				t.Fatal("profile deletion did not finish")
			}
			select {
			case <-dispatchDone:
			case <-ctx.Done():
				t.Fatal("dispatch did not finish")
			}
			if tc.reconcile {
				cancel()
				workers.Wait()
			}

			if logs := w.env.Logs.String(); strings.Contains(logs, "40P01") || strings.Contains(logs, "dispatch_drop_runner_failed") {
				t.Error("SandRun logged a failed cleanup or deadlock")
			}
			_, err = w.env.User(w.org.Admin).Profile.GetRunnerProfile(t.Context(), connect.NewRequest(&sandrunv1.GetRunnerProfileRequest{
				OrganizationId: w.org.ID, RunnerProfileId: profileID,
			}))
			wantCode(t, "get deleted profile", err, connect.CodeNotFound)
			wantState, wantReason := sandrunv1.RequestState_REQUEST_STATE_CANCELED, sandrunv1.RequestEndReason_REQUEST_END_REASON_PROFILE_DELETED
			wantRunner, wantRunnerReason := sandrunv1.RunnerState_RUNNER_STATE_DELETED, sandrunv1.RunnerEndReason_RUNNER_END_REASON_CLEAN_UP
			if tc.cleanupFirst {
				wantState, wantReason = sandrunv1.RequestState_REQUEST_STATE_FAILED, sandrunv1.RequestEndReason_REQUEST_END_REASON_NO_ELIGIBLE_DEVICE
				wantRunner, wantRunnerReason = sandrunv1.RunnerState_RUNNER_STATE_FAILED, sandrunv1.RunnerEndReason_RUNNER_END_REASON_START_FAILED
				if tc.expired {
					wantState, wantReason = sandrunv1.RequestState_REQUEST_STATE_EXPIRED, sandrunv1.RequestEndReason_REQUEST_END_REASON_MAX_QUEUE_WAIT
				}
			}
			if got := w.requestOfJob(t, sponsor); got.GetState() != wantState || got.GetEndReason() != wantReason ||
				got.GetStartAttempts() != 1 || got.GetRunnerId() != "" {
				t.Fatalf("sponsor after deletion: %v", got)
			}
			if got := w.runner(t, runnerID); got.GetState() != wantRunner || got.GetEndReason() != wantRunnerReason ||
				got.GetRequestId() != "" || got.GetOwnsandSandboxId() != "" {
				t.Fatalf("runner after deletion: %v", got)
			}
			if got := w.requestOfJob(t, other); !proto.Equal(got, otherBefore) {
				t.Fatalf("cleanup changed the unrelated queued job: %v", got)
			}
			t.Logf("Deletion committed. Runner end reason: %s. Sponsor end reason: %s. The start-attempt count remained 1.", wantRunnerReason, wantReason)
		})
	}
}

func waitForSponsoredCleanupLock(t *testing.T, ctx context.Context, w *ghWorld, blockers []int32, patterns ...string) (int32, string) {
	t.Helper()
	var pid int32
	var query string
	eventually(t, "PostgreSQL lock wait for "+strings.Join(patterns, " or "), func() bool {
		err := w.env.DB.Pool.QueryRow(ctx, `SELECT pid, query FROM pg_stat_activity
			WHERE datname = current_database() AND pid <> pg_backend_pid()
				AND pg_blocking_pids(pid) && $1::int[] AND query LIKE ANY($2) LIMIT 1`, blockers, patterns).Scan(&pid, &query)
		if errors.Is(err, pgx.ErrNoRows) {
			return false
		}
		if err != nil {
			t.Fatal(err)
		}
		return true
	})
	t.Logf("PostgreSQL backend %d waited on %s", pid, query)
	return pid, query
}
