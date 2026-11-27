// SPDX-License-Identifier: FSL-1.1-ALv2

package layers_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/delinoio/mono/servers/sandrun-api/internal/api"
	"github.com/delinoio/mono/servers/sandrun-api/internal/db"
	"github.com/delinoio/mono/servers/sandrun-api/internal/layers"
)

func TestOwnSandRemovalRollbackFence(t *testing.T) {
	w := newRemovalLeaseWorld(t, time.Minute, false)
	var replicaLayers layers.Layers
	replica := w.replica(t, &replicaLayers, false)
	d := w.env.DB
	ctx := t.Context()
	deployment := api.NewID()

	config := d.Pool.Config()
	config.ConnConfig.RuntimeParams["default_transaction_isolation"] = "repeatable read"
	guardPool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer guardPool.Close()
	guard := &db.DB{Pool: guardPool}

	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Bypass the API admission check as a lease-aware binary from before
	// the guard would. The claim transaction holds the shared lock.
	if _, err := tx.Exec(ctx, `UPDATE ownsand_links SET removing = true, removal_token = $2,
		removal_expires_at = now() + interval '1 minute' WHERE organization_id = $1`, w.org.ID, api.NewID()); err != nil {
		t.Fatal(err)
	}
	drained := make(chan error, 1)
	go func() { drained <- guard.FenceOwnSandTeardown(ctx, deployment) }()
	w.waitForBlockedQuery(t, "SELECT deployment_id IS NOT NULL FROM ownsand_removal_rollback WHERE singleton FOR UPDATE")
	select {
	case err := <-drained:
		t.Fatalf("fence finished before the claim committed: %v", err)
	default:
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-drained; !errors.Is(err, db.ErrOwnSandRemovalTokens) {
		t.Fatalf("fence after the claim committed: %v", err)
	}

	// Expiry permits takeover, but must still block rollback.
	if _, err := d.Pool.Exec(ctx, "UPDATE ownsand_links SET removal_expires_at = now() - interval '1 second' WHERE organization_id = $1", w.org.ID); err != nil {
		t.Fatal(err)
	}
	if err := guard.FenceOwnSandTeardown(ctx, deployment); !errors.Is(err, db.ErrOwnSandRemovalTokens) {
		t.Fatalf("fence with an expired claim: %v", err)
	}
	token, _ := w.lease(t)
	w.fake.SetDeleteHook(func(ctx context.Context) error { return w.checkCleanupCredential(ctx, token) })
	if err := unlinkOwnSand(ctx, replica, w.org); err != nil {
		t.Fatalf("retry after the failed fence: %v", err)
	}
	w.checkRemoved(t)
	if _, err := w.setLink(w.org.Admin, w.osOrg, ownsandKey('a')); err != nil {
		t.Fatal(err)
	}
	// A tokenless legacy mark is compatible with the pre-lease claim format.
	if _, err := d.Pool.Exec(ctx, "UPDATE ownsand_links SET removing = true WHERE organization_id = $1", w.org.ID); err != nil {
		t.Fatal(err)
	}
	if err := guard.FenceOwnSandTeardown(ctx, deployment); err != nil {
		t.Fatal(err)
	}
	wantCode(t, "unlink on the first replica while fenced", unlinkOwnSand(ctx, w.env, w.org), connect.CodeUnavailable)
	wantCode(t, "delete on a second replica while fenced", deleteSandRunOrg(ctx, replica, w.org), connect.CodeUnavailable)
	_, err = d.Pool.Exec(ctx, "UPDATE ownsand_links SET removal_token = $2, removal_expires_at = now() WHERE organization_id = $1", w.org.ID, api.NewID())
	if !db.IsOwnSandTeardownPaused(err) {
		t.Fatalf("unguarded claim write while fenced: %v", err)
	}
	if err := guard.CancelOwnSandRollback(ctx, api.NewID()); err != nil {
		t.Fatal(err)
	}
	wantCode(t, "unlink after canceling an unrelated deployment", unlinkOwnSand(ctx, w.env, w.org), connect.CodeUnavailable)
	if err := guard.CancelOwnSandRollback(ctx, deployment); err != nil {
		t.Fatal(err)
	}
	wantCode(t, "unlink after admission resumed with the legacy mark intact", unlinkOwnSand(ctx, w.env, w.org), connect.CodeFailedPrecondition)
}
