// SPDX-License-Identifier: FSL-1.1-ALv2

package layers_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/servers/sandrun-api/internal/api"
	"github.com/delinoio/mono/servers/sandrun-api/internal/app"
	"github.com/delinoio/mono/servers/sandrun-api/internal/layers"
	"github.com/delinoio/mono/servers/sandrun-api/internal/link"
	"github.com/delinoio/mono/servers/sandrun-api/internal/ownsandclient/ownsandfake"
	"github.com/delinoio/mono/servers/sandrun-api/internal/testenv"
)

func TestOwnSandRemovalLeaseRecovery(t *testing.T) {
	for _, tc := range []struct {
		name string
		call teardownRPC
	}{
		{"unlink", unlinkOwnSand},
		{"delete_organization", deleteSandRunOrg},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newRemovalLeaseWorld(t, 2*time.Second, false)
			var replicaLayers layers.Layers
			replica := w.replica(t, &replicaLayers, false)
			ctx := t.Context()
			claim, err := w.l.Links.ClaimRemoval(ctx, w.org.ID)
			if err != nil || !claim.HasLink() {
				t.Fatalf("claim the link without starting cleanup: %v", err)
			}
			token, firstExpiry := w.lease(t)
			wantCode(t, "competing teardown before expiry", tc.call(ctx, replica, w.org), connect.CodeFailedPrecondition)
			w.checkFence(t, &replicaLayers)
			if w.fake.Calls("DeleteSandbox") != 0 {
				t.Fatal("competing RPC started sandbox cleanup")
			}
			w.waitUntilExpired(t, firstExpiry)
			w.checkFence(t, &replicaLayers)
			w.fake.SetDeleteHook(func(ctx context.Context) error {
				if err := w.checkCleanupCredential(ctx, token); err != nil {
					return err
				}
				err := w.env.DB.InTx(ctx, func(tx pgx.Tx) error {
					if err := api.LockOrganization(ctx, tx, w.org.ID); err != nil {
						return err
					}
					return w.l.Links.CheckRemoval(ctx, tx, claim)
				})
				if connect.CodeOf(err) != connect.CodeFailedPrecondition {
					return errors.New("the abandoned claim could still finalize")
				}
				w.l.Links.ReleaseRemoval(ctx, claim)
				return w.checkCleanupCredential(ctx, token)
			})
			if err := tc.call(ctx, replica, w.org); err != nil {
				t.Fatalf("retry after expiry: %v", err)
			}
			w.checkRemoved(t)
			t.Log("The replica rejected a live claim, recovered it after database expiry, and deleted the sandbox while the key was present. The abandoned claim could neither finalize nor release the new mark.")
		})
	}
}

func TestOwnSandRemovalLeaseFailedRelease(t *testing.T) {
	w := newRemovalLeaseWorld(t, 2*time.Second, false)
	ctx := t.Context()
	pool := w.env.DB.Pool
	if _, err := pool.Exec(ctx, `
		CREATE FUNCTION fail_link_release() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF OLD.removing AND NOT NEW.removing THEN RAISE EXCEPTION 'release unavailable'; END IF;
			RETURN NEW;
		END $$;
		CREATE TRIGGER fail_link_release BEFORE UPDATE ON ownsand_links
			FOR EACH ROW EXECUTE FUNCTION fail_link_release();
	`); err != nil {
		t.Fatal(err)
	}
	w.fake.SetDeleteHook(func(context.Context) error {
		return connect.NewWireError(connect.CodeUnavailable, errors.New("OwnSand unavailable"))
	})
	if err := unlinkOwnSand(ctx, w.env, w.org); err == nil {
		t.Fatal("unlink succeeded with failed cleanup")
	}
	token, expiry := w.lease(t)
	if _, err := pool.Exec(ctx, "DROP TRIGGER fail_link_release ON ownsand_links"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(w.env.Logs.String(), "ownsand_link_removal_unmark_failed") {
		t.Fatal("the simulated database release failure was not logged")
	}
	var replicaLayers layers.Layers
	replica := w.replica(t, &replicaLayers, false)
	w.waitUntilExpired(t, expiry)
	w.fake.SetDeleteHook(func(ctx context.Context) error { return w.checkCleanupCredential(ctx, token) })
	if err := deleteSandRunOrg(ctx, replica, w.org); err != nil {
		t.Fatalf("retry after cleanup and database recovery: %v", err)
	}
	w.checkRemoved(t)
	t.Log("The failed database release left the original key and mark. After the database deadline, the other RPC retried cleanup and removed the sandbox and key.")
}

func TestOwnSandRemovalLeaseLegacyGate(t *testing.T) {
	for _, tc := range []struct {
		name string
		call teardownRPC
	}{
		{"unlink", unlinkOwnSand},
		{"delete_organization", deleteSandRunOrg},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newRemovalLeaseWorld(t, 2*time.Second, false)
			ctx := t.Context()
			if _, err := w.env.DB.Pool.Exec(ctx, `UPDATE ownsand_links SET removing = true,
				organization_deletion_attempt_id = $2 WHERE organization_id = $1`, w.org.ID, api.NewID()); err != nil {
				t.Fatal(err)
			}
			wantCode(t, "legacy mark during rollout", tc.call(ctx, w.env, w.org), connect.CodeFailedPrecondition)
			w.checkFence(t, w.l)
			if w.fake.Calls("DeleteSandbox") != 0 {
				t.Fatal("teardown started before legacy recovery was enabled")
			}
			replica := w.replica(t, &layers.Layers{}, true)
			w.fake.SetDeleteHook(func(ctx context.Context) error { return w.checkCleanupCredential(ctx, "") })
			if err := tc.call(ctx, replica, w.org); err != nil {
				t.Fatalf("legacy recovery after enabling the drain gate: %v", err)
			}
			w.checkRemoved(t)
			t.Log("A pre-lease removal mark stayed fenced until legacy recovery was enabled.")
		})
	}
}

func TestOwnSandRemovalLeaseRenewsAndCancelsAfterLoss(t *testing.T) {
	w := newRemovalLeaseWorld(t, 3*time.Second, false)
	var replicaLayers layers.Layers
	replica := w.replica(t, &replicaLayers, false)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	var calls atomic.Int32
	firstEntered, firstCanceled, retryEntered := make(chan struct{}), make(chan struct{}), make(chan struct{})
	allowRetry := make(chan struct{})
	w.fake.SetDeleteHook(func(ctx context.Context) error {
		switch calls.Add(1) {
		case 1:
			close(firstEntered)
			<-ctx.Done()
			close(firstCanceled)
			return ctx.Err()
		case 2:
			close(retryEntered)
			select {
			case <-allowRetry:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		default:
			return errors.New("unexpected cleanup call")
		}
	})
	wait := func(ch <-chan struct{}, what string) {
		t.Helper()
		select {
		case <-ch:
		case <-ctx.Done():
			t.Fatalf("waiting for %s: %v", what, ctx.Err())
		}
	}
	first := make(chan error, 1)
	go func() { first <- unlinkOwnSand(ctx, w.env, w.org) }()
	wait(firstEntered, "the first sandbox deletion")
	firstToken, firstExpiry := w.lease(t)
	w.waitUntilExpired(t, firstExpiry)
	if token, expiry := w.lease(t); token != firstToken || !expiry.After(firstExpiry) {
		t.Fatal("renewal did not extend the original claim")
	}
	wantCode(t, "competing RPC after the initial deadline", deleteSandRunOrg(ctx, replica, w.org), connect.CodeFailedPrecondition)
	w.checkFence(t, &replicaLayers)

	// Simulate loss of a lease while the provider call is still in flight.
	if _, err := w.env.DB.Pool.Exec(ctx, "UPDATE ownsand_links SET removal_expires_at = clock_timestamp() WHERE organization_id = $1", w.org.ID); err != nil {
		t.Fatal(err)
	}
	retry := make(chan error, 1)
	go func() { retry <- deleteSandRunOrg(ctx, replica, w.org) }()
	wait(retryEntered, "the new owner's sandbox deletion")
	wait(firstCanceled, "the old owner's preparation context cancellation")
	if err := <-first; err == nil {
		t.Fatal("the old RPC finalized after losing its claim")
	}
	if err := w.checkCleanupCredential(ctx, firstToken); err != nil {
		t.Fatal(err)
	}
	w.checkFence(t, &replicaLayers)
	close(allowRetry)
	if err := <-retry; err != nil {
		t.Fatalf("finalize with the new claim: %v", err)
	}
	w.checkRemoved(t)
	t.Log("Renewal blocked a second RPC beyond the initial expiry. Losing the lease canceled the first provider call. The new owner kept dispatch and key replacement blocked until it completed cleanup.")
}

func TestOwnSandRemovalLeaseFinalization(t *testing.T) {
	for _, rpc := range removalFinalizationRPCs {
		t.Run(rpc.name, func(t *testing.T) {
			for _, outcome := range []struct {
				name string
				code connect.Code
			}{
				{name: "commit"},
				{name: "cancel", code: connect.CodeCanceled},
				{name: "audit_error", code: connect.CodeInternal},
			} {
				t.Run(outcome.name, func(t *testing.T) {
					w := newRemovalLeaseWorld(t, 3*time.Second, false)
					ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
					defer cancel()
					pool := w.env.DB.Pool
					if outcome.code == connect.CodeInternal {
						if _, err := pool.Exec(ctx, fmt.Sprintf(`
							CREATE FUNCTION fail_removal_audit() RETURNS trigger LANGUAGE plpgsql AS $$
							BEGIN
								RAISE EXCEPTION 'removal audit unavailable';
							END $$;
							CREATE TRIGGER fail_removal_audit BEFORE INSERT ON audit_events
								FOR EACH ROW WHEN (NEW.action = %d) EXECUTE FUNCTION fail_removal_audit();
						`, rpc.audit)); err != nil {
							t.Fatal(err)
						}
					}

					afterCommit := make(chan error, 1)
					recordCommit := func(ctx context.Context) {
						ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
						defer cancel()
						var linkExists, auditExists bool
						err := pool.QueryRow(ctx, `SELECT
							EXISTS (SELECT 1 FROM ownsand_links WHERE organization_id = $1),
							EXISTS (SELECT 1 FROM audit_events WHERE organization_id = $1 AND action = $2)`,
							w.org.ID, int16(rpc.audit)).Scan(&linkExists, &auditExists)
						if err == nil && (linkExists || !auditExists) {
							err = errors.New("after-commit callback ran before the link removal and audit were visible")
						}
						afterCommit <- err
					}

					var replicaLayers layers.Layers
					replica := w.env.NewReplica(t, w.layerOptions(&replicaLayers, false), testenv.WithExtension(func(r *app.Registry) error {
						replicaLayers.Links.OnRemoval(link.RemovalHook{
							Name: "observe_commit",
							InTx: func(context.Context, pgx.Tx, string) (func(context.Context), error) {
								return recordCommit, nil
							},
						})
						r.OnOrganizationDeletion(api.OrganizationDeletionHook{
							Name: "observe_commit",
							Prepare: func(context.Context, api.OrganizationDeletion) (api.OrganizationDeletionSteps, error) {
								return api.OrganizationDeletionSteps{AfterCommit: recordCommit}, nil
							},
						})
						return nil
					}))

					result := make(chan error, 1)
					release := holdTx(t, w.env, "SELECT 1 FROM requests WHERE request_id = $1 FOR UPDATE", w.queuedID)
					if outcome.code == 0 {
						wait, resume, _ := pauseTeardown(t, w.workWorld)
						go func() { result <- rpc.call(ctx, replica, w.org) }()
						wait(ctx)
						releaseLink := holdTx(t, w.env, "SELECT 1 FROM ownsand_links WHERE organization_id = $1 FOR UPDATE", w.org.ID)
						resume()
						w.waitForBlockedQuery(t, "SELECT ownsand_link_id::text FROM ownsand_links")
						w.waitForBlockedQuery(t, "SELECT removal_expires_at FROM ownsand_links WHERE ownsand_link_id = $1 AND removal_token = $2 FOR UPDATE")
						releaseLink()
						t.Log("Finalization and renewal both waited on the link row. Finalization queued first.")
					} else {
						go func() { result <- rpc.call(ctx, replica, w.org) }()
					}
					w.waitForBlockedQuery(t, "FROM requests WHERE organization_id = $1 AND ($2")
					t.Log("Finalization is waiting on the queued request row after checking the link claim.")

					if outcome.code == 0 {
						// The previous watcher timed out within two renewal intervals.
						timer := time.NewTimer(2*w.duration/3 + 250*time.Millisecond)
						defer timer.Stop()
						select {
						case err := <-result:
							t.Fatalf("RPC completed while the request row was locked: %v", err)
						case <-timer.C:
						case <-ctx.Done():
							t.Fatal(ctx.Err())
						}
						select {
						case err := <-afterCommit:
							t.Fatalf("callback ran while finalization was blocked: %v", err)
						default:
						}
					}
					if outcome.code == connect.CodeCanceled {
						cancel()
						wantCode(t, "caller cancellation", <-result, connect.CodeCanceled)
						// The HTTP caller can return before the server rolls back.
						eventually(t, "canceled finalization to release its link claim", func() bool {
							id, removing := teardownLink(t, w.workWorld)
							return id != "" && !removing
						})
					}
					release()
					switch outcome.code {
					case 0:
						if err := <-result; err != nil {
							t.Fatalf("finalize after the former renewal timeout: %v", err)
						}
						if err := <-afterCommit; err != nil {
							t.Fatal(err)
						}
						w.checkRemoved(t)
						if rpc.audit == sandrunv1.AuditAction_AUDIT_ACTION_ORGANIZATION_DELETED {
							wantCode(t, "deleted organization", deleteSandRunOrg(ctx, replica, w.org), connect.CodeNotFound)
						} else if got := w.session(t, w.queuedID); got.GetState() != sandrunv1.RequestState_REQUEST_STATE_CANCELED ||
							got.GetEndReason() != sandrunv1.RequestEndReason_REQUEST_END_REASON_INTEGRATION_REMOVED {
							t.Fatalf("queued session after unlink = %v", got)
						}
						t.Log("The RPC committed after the former renewal timeout. The callback observed the committed audit and removal.")
					case connect.CodeInternal:
						wantCode(t, "forced audit error", <-result, connect.CodeInternal)
					}
					if outcome.code != 0 {
						w.checkFinalizationRollback(t, rpc.audit)
						if id, removing := teardownLink(t, w.workWorld); id == "" || removing {
							t.Fatalf("link after rollback = %q, removing = %t", id, removing)
						}
						select {
						case err := <-afterCommit:
							t.Fatalf("callback ran after rollback: %v", err)
						default:
						}
						t.Log("Finalization rolled back without canceling the queued request, writing an audit, or running the callback.")
					}
				})
			}
		})
	}
}

func TestOwnSandRemovalLeaseRejectsBeforeFinalization(t *testing.T) {
	for _, rpc := range removalFinalizationRPCs {
		t.Run(rpc.name, func(t *testing.T) {
			for _, tc := range []struct {
				name     string
				update   string
				duration time.Duration
				code     connect.Code
			}{
				{"changed_token", "UPDATE ownsand_links SET removal_token = $2 WHERE organization_id = $1", time.Minute, connect.CodeFailedPrecondition},
				{"expired_claim", "UPDATE ownsand_links SET removal_expires_at = clock_timestamp() WHERE organization_id = $1", time.Minute, connect.CodeFailedPrecondition},
				{"renewal_timeout", "SELECT 1 FROM ownsand_links WHERE organization_id = $1 FOR UPDATE", 3 * time.Second, connect.CodeCanceled},
			} {
				t.Run(tc.name, func(t *testing.T) {
					w := newRemovalLeaseWorld(t, tc.duration, false)
					ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
					defer cancel()
					wait, resume, _ := pauseTeardown(t, w.workWorld)
					result := make(chan error, 1)
					go func() { result <- rpc.call(ctx, w.env, w.org) }()
					wait(ctx)
					token, expiry := w.lease(t)
					args := []any{w.org.ID}
					if tc.name == "changed_token" {
						token = api.NewID()
						args = append(args, token)
					}

					// Preparation sees the original claim until the other
					// transaction commits. Verify which query waits for it.
					release := holdTx(t, w.env, tc.update, args...)
					resume()
					w.waitForBlockedQuery(t, "SELECT ownsand_link_id::text FROM ownsand_links")
					if tc.code == connect.CodeCanceled {
						eventually(t, "renewal failure while finalization waits for the link lock", func() bool {
							return strings.Contains(w.env.Logs.String(), "ownsand_link_removal_claim_lost")
						})
						// The original lease must still be live at this failure.
						var live bool
						if err := w.env.DB.Pool.QueryRow(ctx, "SELECT clock_timestamp() < $1", expiry).Scan(&live); err != nil || !live {
							t.Fatalf("original lease was not live after the renewal failure: %v", err)
						}
					}
					release()
					wantCode(t, "claim rejected before finalization", <-result, tc.code)
					w.checkFinalizationRollback(t, rpc.audit)
					if tc.code == connect.CodeFailedPrecondition {
						if got, _ := w.lease(t); got != token {
							t.Fatalf("claim after rejection = %q, want %q", got, token)
						}
					} else if id, removing := teardownLink(t, w.workWorld); id == "" || removing {
						t.Fatalf("link after renewal failure = %q, removing = %t", id, removing)
					}
					t.Log("The RPC rejected the attempt before finalization and preserved the organization, link, and queued request.")
				})
			}
		})
	}
}

var removalFinalizationRPCs = []struct {
	name  string
	call  teardownRPC
	audit sandrunv1.AuditAction
}{
	{"unlink", unlinkOwnSand, sandrunv1.AuditAction_AUDIT_ACTION_OWNSAND_LINK_REMOVED},
	{"delete_organization", deleteSandRunOrg, sandrunv1.AuditAction_AUDIT_ACTION_ORGANIZATION_DELETED},
}

func (w *removalLeaseWorld) waitForBlockedQuery(t *testing.T, query string) {
	t.Helper()
	eventually(t, "lock wait in "+query, func() bool {
		var waiting bool
		err := w.env.DB.Pool.QueryRow(t.Context(), `SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity WHERE datname = current_database()
				AND wait_event_type = 'Lock' AND strpos(query, $1) > 0
		)`, query).Scan(&waiting)
		if err != nil {
			t.Fatal(err)
		}
		return waiting
	})
}

func (w *removalLeaseWorld) checkFinalizationRollback(t *testing.T, action sandrunv1.AuditAction) {
	t.Helper()
	if got := w.session(t, w.queuedID); got.GetState() != sandrunv1.RequestState_REQUEST_STATE_QUEUED {
		t.Fatalf("queued session did not survive rollback: %v", got)
	}
	if count := countAudit(t, w.env, w.org.ID, action); count != 0 {
		t.Fatalf("removal audit events after rollback = %d", count)
	}
}

type removalLeaseWorld struct {
	*workWorld
	duration time.Duration
	queuedID string
}

func newRemovalLeaseWorld(t *testing.T, duration time.Duration, reclaimLegacy bool) *removalLeaseWorld {
	t.Helper()
	ctx := t.Context()
	// Keep background dispatch and reconciliation out of the fixture. The
	// test invokes a dispatcher pass while each fence is active.
	seed := testenv.New(t)
	eventually(t, "core instance holds the lease", seed.App.Lease.Held)
	w := &removalLeaseWorld{
		workWorld: &workWorld{world: &world{l: &layers.Layers{}, fake: ownsandfake.New(), org: seed.NewOrg("alice")}, osOrg: api.NewID()},
		duration:  duration,
	}
	w.env = seed.NewReplica(t, w.layerOptions(w.l, reclaimLegacy))
	w.fake.AddOrg(w.osOrg, ownsandKey('a'))
	if _, err := w.setLink(w.org.Admin, w.osOrg, ownsandKey('a')); err != nil {
		t.Fatal(err)
	}
	profile := w.createProfile(t, devProfileProto("linux-dev", 0, 0))
	w.queuedID = w.createSession(t, w.env.User(w.org.Member), profile).GetDevSessionId()
	client, _, err := w.l.Links.Client(ctx, w.org.ID)
	if err != nil {
		t.Fatal(err)
	}
	runnerID := api.NewID()
	sb, err := client.CreateSandbox(ctx, &ownsandv1.CreateSandboxRequest{IdempotencyKey: runnerID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.env.DB.Pool.Exec(ctx, `INSERT INTO runners (runner_id, organization_id, runner_profile_id, kind, guest_os, state,
		idempotency_key, create_request, ownsand_sandbox_id, create_time, update_time)
		VALUES ($1, $2, $3, $4, $5, $6, $1, '', $7, now(), now())`,
		runnerID, w.org.ID, profile, int16(sandrunv1.WorkloadKind_WORKLOAD_KIND_DEV_SESSION), int16(sandrunv1.GuestOs_GUEST_OS_LINUX),
		int16(sandrunv1.RunnerState_RUNNER_STATE_BUSY), sb.GetSandboxId()); err != nil {
		t.Fatal(err)
	}
	return w
}

func (w *removalLeaseWorld) layerOptions(out *layers.Layers, reclaimLegacy bool) testenv.Option {
	return testenv.WithLayerOptions(layers.Options{OwnSand: w.fake, OwnSandRemoval: link.RemovalOptions{
		LeaseDuration: w.duration, ReclaimLegacy: reclaimLegacy,
	}}, out)
}

func (w *removalLeaseWorld) replica(t *testing.T, out *layers.Layers, reclaimLegacy bool) *testenv.Env {
	t.Helper()
	return w.env.NewReplica(t, w.layerOptions(out, reclaimLegacy))
}

func (w *removalLeaseWorld) lease(t *testing.T) (string, time.Time) {
	t.Helper()
	var token string
	var expiry time.Time
	if err := w.env.DB.Pool.QueryRow(t.Context(), `SELECT removal_token::text, removal_expires_at
		FROM ownsand_links WHERE organization_id = $1 AND removing`, w.org.ID).Scan(&token, &expiry); err != nil {
		t.Fatal(err)
	}
	return token, expiry
}

func (w *removalLeaseWorld) waitUntilExpired(t *testing.T, expiry time.Time) {
	t.Helper()
	eventually(t, "database time to pass the original expiry", func() bool {
		var elapsed bool
		if err := w.env.DB.Pool.QueryRow(t.Context(), "SELECT clock_timestamp() >= $1", expiry).Scan(&elapsed); err != nil {
			t.Fatal(err)
		}
		return elapsed
	})
}

func (w *removalLeaseWorld) checkFence(t *testing.T, replica *layers.Layers) {
	t.Helper()
	if id, removing := teardownLink(t, w.workWorld); id == "" || !removing {
		t.Fatalf("link during teardown = %q, removing %v", id, removing)
	}
	_, err := w.setLink(w.org.Admin, w.osOrg, ownsandKey('a'))
	wantCode(t, "key replacement while fenced", err, connect.CodeFailedPrecondition)
	creates := w.fake.Calls("CreateSandbox")
	replica.Engine.Pass(t.Context(), w.org.ID)
	if w.fake.Calls("CreateSandbox") != creates {
		t.Fatal("dispatcher created a sandbox during teardown")
	}
}

func (w *removalLeaseWorld) checkCleanupCredential(ctx context.Context, oldToken string) error {
	var claimed bool
	err := w.env.DB.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM ownsand_links
		WHERE organization_id = $1 AND removing AND removal_token::text <> $2 AND length(api_key_ciphertext) > 0)`,
		w.org.ID, oldToken).Scan(&claimed)
	if err != nil {
		return err
	}
	if !claimed || w.fake.Active(w.osOrg) != 1 {
		return errors.New("cleanup did not have the new claim, key and original sandbox")
	}
	return nil
}

func (w *removalLeaseWorld) checkRemoved(t *testing.T) {
	t.Helper()
	if id, _ := teardownLink(t, w.workWorld); id != "" || w.fake.Active(w.osOrg) != 0 {
		t.Fatalf("teardown left a link or sandbox: link %q, sandboxes %d", id, w.fake.Active(w.osOrg))
	}
}
