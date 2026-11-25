// SPDX-License-Identifier: FSL-1.1-ALv2

package layers_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/servers/tide-api/internal/api"
	"github.com/delinoio/mono/servers/tide-api/internal/layers"
	"github.com/delinoio/mono/servers/tide-api/internal/link"
	"github.com/delinoio/mono/servers/tide-api/internal/ownsandclient/ownsandfake"
	"github.com/delinoio/mono/servers/tide-api/internal/testenv"
)

func TestOwnSandRemovalLeaseRecovery(t *testing.T) {
	for _, tc := range []struct {
		name string
		call teardownRPC
	}{
		{"unlink", unlinkOwnSand},
		{"delete_organization", deleteTideOrg},
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
	if err := deleteTideOrg(ctx, replica, w.org); err != nil {
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
		{"delete_organization", deleteTideOrg},
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
	wantCode(t, "competing RPC after the initial deadline", deleteTideOrg(ctx, replica, w.org), connect.CodeFailedPrecondition)
	w.checkFence(t, &replicaLayers)

	// Simulate loss of a lease while the provider call is still in flight.
	if _, err := w.env.DB.Pool.Exec(ctx, "UPDATE ownsand_links SET removal_expires_at = clock_timestamp() WHERE organization_id = $1", w.org.ID); err != nil {
		t.Fatal(err)
	}
	retry := make(chan error, 1)
	go func() { retry <- deleteTideOrg(ctx, replica, w.org) }()
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

type removalLeaseWorld struct {
	*workWorld
	duration time.Duration
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
	w.createSession(t, w.env.User(w.org.Member), profile)
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
		runnerID, w.org.ID, profile, int16(tidev1.WorkloadKind_WORKLOAD_KIND_DEV_SESSION), int16(tidev1.GuestOs_GUEST_OS_LINUX),
		int16(tidev1.RunnerState_RUNNER_STATE_BUSY), sb.GetSandboxId()); err != nil {
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
