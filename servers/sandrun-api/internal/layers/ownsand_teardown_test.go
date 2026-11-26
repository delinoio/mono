// SPDX-License-Identifier: FSL-1.1-ALv2

package layers_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/servers/sandrun-api/internal/api"
	"github.com/delinoio/mono/servers/sandrun-api/internal/app"
	"github.com/delinoio/mono/servers/sandrun-api/internal/auth"
	"github.com/delinoio/mono/servers/sandrun-api/internal/devsession"
	"github.com/delinoio/mono/servers/sandrun-api/internal/layers"
	"github.com/delinoio/mono/servers/sandrun-api/internal/ownsandclient/ownsandfake"
	"github.com/delinoio/mono/servers/sandrun-api/internal/testenv"
)

type teardownRPC func(context.Context, *testenv.Env, testenv.Org) error

func unlinkOwnSand(ctx context.Context, env *testenv.Env, org testenv.Org) error {
	_, err := env.User(org.Admin).OwnSandLink.RemoveOwnSandLink(ctx, connect.NewRequest(&sandrunv1.RemoveOwnSandLinkRequest{OrganizationId: org.ID}))
	return err
}

func deleteSandRunOrg(ctx context.Context, env *testenv.Env, org testenv.Org) error {
	_, err := env.User(org.Owner).Organization.DeleteOrganization(ctx, connect.NewRequest(&sandrunv1.DeleteOrganizationRequest{OrganizationId: org.ID}))
	return err
}

func TestOwnSandTeardownContention(t *testing.T) {
	for _, tc := range []struct {
		name             string
		first, competing teardownRPC
		unlinks          bool
	}{
		{"unlink_then_unlink", unlinkOwnSand, unlinkOwnSand, true},
		{"unlink_then_delete", unlinkOwnSand, deleteSandRunOrg, true},
		{"delete_then_unlink", deleteSandRunOrg, unlinkOwnSand, false},
		{"delete_then_delete", deleteSandRunOrg, deleteSandRunOrg, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorkWorld(t, devsession.Timing{})
			var replicaLayers layers.Layers
			replica := w.env.NewReplica(t, testenv.WithLayers(w.fake, &replicaLayers))
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()

			profileID := w.createProfile(t, devProfileProto("linux-dev", 0, 0))
			running := w.createSession(t, w.env.User(w.org.Member), profileID)
			w.waitState(t, running.GetDevSessionId(), sandrunv1.RequestState_REQUEST_STATE_RUNNING)
			originalID, removing := teardownLink(t, w)
			if removing {
				t.Fatal("link was marked before teardown")
			}
			waitCleanup, releaseCleanup, cleanupCalls := pauseTeardown(t, w)

			winner := make(chan error, 1)
			go func() { winner <- tc.first(ctx, w.env, w.org) }()
			waitCleanup(ctx)

			queued := w.createSession(t, w.env.User(w.org.Member), profileID)
			creates := w.fake.Calls("CreateSandbox")
			wantCode(t, "competing teardown", tc.competing(ctx, replica, w.org), connect.CodeFailedPrecondition)
			if id, removing := teardownLink(t, w); id != originalID || !removing {
				t.Fatalf("link during contention = %q, removing %v", id, removing)
			}
			if got := cleanupCalls.Load(); got != 1 {
				t.Fatalf("teardown cleanup calls = %d, want 1", got)
			}
			replicaLayers.Engine.Pass(ctx, w.org.ID)
			if got := w.fake.Calls("CreateSandbox"); got != creates {
				t.Fatalf("CreateSandbox calls during teardown = %d, want %d", got, creates)
			}
			if got := w.fake.Active(w.osOrg); got != 1 {
				t.Fatalf("active sandboxes during teardown = %d, want the original sandbox", got)
			}
			var runners int
			err := w.env.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM runners WHERE organization_id = $1", w.org.ID).Scan(&runners)
			if err != nil || runners != 1 {
				t.Fatalf("runner rows during teardown = %d, want 1 (%v)", runners, err)
			}

			releaseCleanup()
			if err := <-winner; err != nil {
				t.Fatalf("owning teardown: %v", err)
			}
			if got := w.fake.Active(w.osOrg); got != 0 {
				t.Fatalf("active sandboxes after teardown = %d", got)
			}
			if id, _ := teardownLink(t, w); id != "" {
				t.Fatalf("link %s survived teardown", id)
			}
			if !tc.unlinks {
				wantCode(t, "organization after teardown", deleteSandRunOrg(ctx, replica, w.org), connect.CodeNotFound)
				return
			}

			if got := w.session(t, queued.GetDevSessionId()); got.GetState() != sandrunv1.RequestState_REQUEST_STATE_CANCELED ||
				got.GetEndReason() != sandrunv1.RequestEndReason_REQUEST_END_REASON_INTEGRATION_REMOVED {
				t.Fatalf("queued session after unlink = %v", got)
			}
			w.relinkAfterTeardown(t, originalID, profileID)
		})
	}
	t.Run("cleanup_failure", func(t *testing.T) {
		w := newWorkWorld(t, devsession.Timing{})
		profileID := w.createProfile(t, devProfileProto("linux-dev", 0, 0))
		running := w.createSession(t, w.env.User(w.org.Member), profileID)
		w.waitState(t, running.GetDevSessionId(), sandrunv1.RequestState_REQUEST_STATE_RUNNING)
		originalID, _ := teardownLink(t, w)
		w.fake.Update(w.osOrg, func(o *ownsandfake.Org) {
			o.DeleteError = connect.NewWireError(connect.CodeUnavailable, errors.New("device busy"))
		})
		if err := unlinkOwnSand(context.Background(), w.env, w.org); err == nil {
			t.Fatal("unlink succeeded after OwnSand cleanup failed")
		}
		w.fake.Update(w.osOrg, func(o *ownsandfake.Org) { o.DeleteError = nil })
		w.checkTeardownRetry(t, unlinkOwnSand, originalID, profileID)
	})

	for _, tc := range []struct {
		name string
		call teardownRPC
	}{
		{"unlink_canceled_in_transaction", unlinkOwnSand},
		{"delete_canceled_in_transaction", deleteSandRunOrg},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorkWorld(t, devsession.Timing{})
			profileID := w.createProfile(t, devProfileProto("linux-dev", 0, 0))
			running := w.createSession(t, w.env.User(w.org.Member), profileID)
			w.waitState(t, running.GetDevSessionId(), sandrunv1.RequestState_REQUEST_STATE_RUNNING)
			originalID, _ := teardownLink(t, w)
			waitCleanup, releaseCleanup, _ := pauseTeardown(t, w)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- tc.call(ctx, w.env, w.org) }()
			waitCleanup(ctx)

			queued := w.createSession(t, w.env.User(w.org.Member), profileID)
			commit := holdTx(t, w.env, "SELECT 1 FROM organizations WHERE organization_id = $1 FOR UPDATE", w.org.ID)
			releaseCleanup()
			waitForLockWaiter(t, w.env)
			cancel()
			wantCode(t, "canceled teardown", <-result, connect.CodeCanceled)
			commit()
			eventually(t, "claim released by canceled request", func() bool {
				id, removing := teardownLink(t, w)
				return id == originalID && !removing
			})
			w.waitState(t, queued.GetDevSessionId(), sandrunv1.RequestState_REQUEST_STATE_RUNNING)
			w.checkTeardownRetry(t, tc.call, originalID, profileID)
		})
	}

	t.Run("no_link", func(t *testing.T) {
		w := newWorld(t)
		ctx := context.Background()
		wantCode(t, "unlink a missing link", unlinkOwnSand(ctx, w.env, w.org), connect.CodeNotFound)
		if err := deleteSandRunOrg(ctx, w.env, w.org); err != nil {
			t.Fatalf("delete an organization without a link: %v", err)
		}
	})
	t.Run("link_appears_after_prepare", func(t *testing.T) {
		prepared, finish := make(chan struct{}), make(chan struct{})
		w := newWorld(t, testenv.WithExtension(func(r *app.Registry) error {
			r.OnOrganizationDeletion(api.OrganizationDeletionHook{
				Name: "wait_for_link",
				Prepare: func(ctx context.Context, _ api.OrganizationDeletion) (api.OrganizationDeletionSteps, error) {
					close(prepared)
					select {
					case <-finish:
						return api.OrganizationDeletionSteps{}, nil
					case <-ctx.Done():
						return api.OrganizationDeletionSteps{}, ctx.Err()
					}
				},
			})
			return nil
		}))
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		result := make(chan error, 1)
		go func() { result <- deleteSandRunOrg(ctx, w.env, w.org) }()
		select {
		case <-prepared:
		case <-ctx.Done():
			t.Fatal("organization deletion did not finish its work preparation")
		}
		ownsandOrg := api.NewID()
		w.fake.AddOrg(ownsandOrg, ownsandKey('a'))
		if _, err := w.setLink(w.org.Admin, ownsandOrg, ownsandKey('a')); err != nil {
			t.Fatal(err)
		}
		claim, err := w.l.Links.ClaimRemoval(ctx, w.org.ID)
		if err != nil || !claim.HasLink() {
			t.Fatalf("claim the new link: %v", err)
		}
		defer w.l.Links.ReleaseRemoval(ctx, claim)
		close(finish)
		wantCode(t, "deletion with an unexpected link", <-result, connect.CodeFailedPrecondition)
		if id, removing := teardownLink(t, &workWorld{world: w}); id == "" || !removing {
			t.Fatalf("new link after deletion = %q, removing %v", id, removing)
		}
	})
}

func pauseTeardown(t *testing.T, w *workWorld) (wait func(context.Context), release func(), calls *atomic.Int32) {
	t.Helper()
	started, resume := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	release = func() { releaseOnce.Do(func() { close(resume) }) }
	t.Cleanup(release)
	calls = &atomic.Int32{}
	w.fake.SetDeleteHook(func(ctx context.Context) error {
		// Count calls from authenticated teardown RPCs. Reconciler calls
		// also wait, so they cannot delete the sandbox during the check.
		if _, rpc := auth.PrincipalFrom(ctx); rpc && calls.Add(1) == 1 {
			close(started)
		}
		select {
		case <-resume:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	wait = func(ctx context.Context) {
		t.Helper()
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("teardown did not reach OwnSand sandbox deletion")
		}
	}
	return wait, release, calls
}

func teardownLink(t *testing.T, w *workWorld) (id string, removing bool) {
	t.Helper()
	lk, ok, err := w.l.Links.Get(context.Background(), w.env.DB.Pool, w.org.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		return "", false
	}
	return lk.LinkID, lk.Removing
}

func (w *workWorld) relinkAfterTeardown(t *testing.T, originalID, profileID string) {
	t.Helper()
	if _, err := w.setLink(w.org.Admin, w.osOrg, ownsandKey('a')); err != nil {
		t.Fatal(err)
	}
	id, removing := teardownLink(t, w)
	if id == "" || id == originalID || removing {
		t.Fatalf("replacement link = %q, removing %v (original %q)", id, removing, originalID)
	}
	var target string
	if err := w.env.DB.Pool.QueryRow(context.Background(), "SELECT target_id FROM audit_events WHERE organization_id = $1 AND action = $2",
		w.org.ID, int16(sandrunv1.AuditAction_AUDIT_ACTION_OWNSAND_LINK_REMOVED)).Scan(&target); err != nil || target != originalID {
		t.Fatalf("removal audit target = %q, want %q (%v)", target, originalID, err)
	}
	if count := countAudit(t, w.env, w.org.ID, sandrunv1.AuditAction_AUDIT_ACTION_OWNSAND_LINK_REMOVED); count != 1 {
		t.Fatalf("removal audit events = %d, want 1", count)
	}
	s := w.createSession(t, w.env.User(w.org.Member), profileID)
	w.waitState(t, s.GetDevSessionId(), sandrunv1.RequestState_REQUEST_STATE_RUNNING)
}

func (w *workWorld) checkTeardownRetry(t *testing.T, call teardownRPC, originalID, profileID string) {
	t.Helper()
	if id, removing := teardownLink(t, w); id != originalID || removing {
		t.Fatalf("link after failed teardown = %q, removing %v", id, removing)
	}
	next := w.createSession(t, w.env.User(w.org.Member), profileID)
	w.waitState(t, next.GetDevSessionId(), sandrunv1.RequestState_REQUEST_STATE_RUNNING)
	if err := call(context.Background(), w.env, w.org); err != nil {
		t.Fatalf("retry teardown: %v", err)
	}
	if id, _ := teardownLink(t, w); id != "" {
		t.Fatalf("link %s survived teardown retry", id)
	}
	if got := w.fake.Active(w.osOrg); got != 0 {
		t.Fatalf("active sandboxes after teardown retry = %d", got)
	}
}
