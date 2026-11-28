// SPDX-License-Identifier: FSL-1.1-ALv2

package layers_test

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
	"github.com/jackc/pgx/v5/pgxpool"

	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/servers/sandrun-api/internal/agent"
	"github.com/delinoio/mono/servers/sandrun-api/internal/agent/cursor/cursorfake"
	"github.com/delinoio/mono/servers/sandrun-api/internal/app"
	"github.com/delinoio/mono/servers/sandrun-api/internal/db"
	"github.com/delinoio/mono/servers/sandrun-api/internal/ownsandclient/ownsandfake"
	"github.com/delinoio/mono/servers/sandrun-api/internal/testenv"
)

const (
	queuedRequest   = sandrunv1.RequestState_REQUEST_STATE_QUEUED
	canceledRequest = sandrunv1.RequestState_REQUEST_STATE_CANCELED
	linkRemoved     = sandrunv1.RequestEndReason_REQUEST_END_REASON_INTEGRATION_REMOVED
)

func TestOwnSandUnlinkReleasesItsOwnClaimsAfterCommit(t *testing.T) {
	t.Parallel()
	for _, outcome := range []string{"commit", "rollback"} {
		t.Run(outcome, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			tracer := &unlinkCommitTracer{}
			w := newUnlinkClaimWorld(t, func(o *app.Options) {
				cfg := o.DB.Pool.Config()
				cfg.ConnConfig.Tracer = tracer
				pool, err := pgxpool.NewWithConfig(ctx, cfg)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(pool.Close)
				o.DB = &db.DB{Pool: pool}
			})
			pool := w.env.DB.Pool
			removeAsync := func() <-chan error {
				result := make(chan error, 1)
				go func() { result <- unlinkOwnSand(ctx, w.env, w.org) }()
				return result
			}
			receive := func(result <-chan error) error {
				t.Helper()
				select {
				case err := <-result:
					return err
				case <-ctx.Done():
					t.Fatal("unlink did not finish:", ctx.Err())
					return nil
				}
			}

			firstID := w.queueUnlinkClaim(t, "first")
			firstCommit := newUnlinkGate()
			tracer.next.Store(&unlinkCommitGates{after: firstCommit})
			first := removeAsync()
			firstCommit.wait(t, ctx)
			w.assertUnlinkClaim(t, firstID, "first", canceledRequest, agent.ClaimHeld, 0)

			if _, err := w.setLink(w.org.Admin, w.osOrg, ownsandKey('a')); err != nil {
				t.Fatal(err)
			}
			secondID := w.queueUnlinkClaim(t, "second")
			if outcome == "rollback" {
				if _, err := pool.Exec(ctx, `
					CREATE FUNCTION reject_unlink_commit() RETURNS trigger LANGUAGE plpgsql AS $$
					BEGIN
						RAISE EXCEPTION 'injected unlink commit failure';
					END;
					$$;
					CREATE CONSTRAINT TRIGGER reject_unlink_commit
						AFTER DELETE ON ownsand_links DEFERRABLE INITIALLY DEFERRED
						FOR EACH ROW EXECUTE FUNCTION reject_unlink_commit();`); err != nil {
					t.Fatal(err)
				}
			}

			beforeSecondCommit, afterSecondCommit := newUnlinkGate(), newUnlinkGate()
			tracer.next.Store(&unlinkCommitGates{before: beforeSecondCommit, after: afterSecondCommit})
			second := removeAsync()
			beforeSecondCommit.wait(t, ctx)
			w.assertUnlinkClaim(t, secondID, "second", queuedRequest, agent.ClaimHeld, 0)

			close(firstCommit.resume)
			if err := receive(first); err != nil {
				t.Fatal(err)
			}
			if attempts := w.cursor.FailAttempts("second"); attempts != 0 {
				t.Errorf("second unlink produced %d vendor calls before commit, want 0", attempts)
			}
			w.assertUnlinkClaim(t, firstID, "first", canceledRequest, agent.ClaimReleased, 1)
			w.assertUnlinkClaim(t, secondID, "second", queuedRequest, agent.ClaimHeld, 0)
			t.Log("First unlink released its own claim while the second cancellation was still uncommitted.")

			close(beforeSecondCommit.resume)
			if outcome == "rollback" {
				wantCode(t, "second unlink with a failed commit", receive(second), connect.CodeInternal)
				if !strings.Contains(w.env.Logs.String(), "injected unlink commit failure") {
					t.Fatal("PostgreSQL did not report the injected commit failure")
				}
				w.assertUnlinkClaim(t, secondID, "second", queuedRequest, agent.ClaimHeld, 0)
				link, ok, err := w.l.Links.Get(ctx, pool, w.org.ID)
				if err != nil || !ok || !link.Dispatchable() {
					t.Fatalf("link after failed commit = %+v, %v, %v", link, ok, err)
				}
				t.Log("Second unlink rolled back, cleared the removal mark and made no vendor call.")
			} else {
				afterSecondCommit.wait(t, ctx)
				w.assertUnlinkClaim(t, secondID, "second", canceledRequest, agent.ClaimHeld, 0)
				close(afterSecondCommit.resume)
				if err := receive(second); err != nil {
					t.Fatal(err)
				}
				w.assertUnlinkClaim(t, secondID, "second", canceledRequest, agent.ClaimReleased, 1)
				t.Log("Second unlink released its claim after its cancellation became visible.")
			}
		})
	}
}

func TestOwnSandUnlinkClaimReleaseRetries(t *testing.T) {
	t.Parallel()
	w := newUnlinkClaimWorld(t)
	const vendorID = "retry"
	id := w.queueUnlinkClaim(t, vendorID)
	w.cursor.FailClaimCalls(true)
	if err := unlinkOwnSand(t.Context(), w.env, w.org); err != nil {
		t.Fatal(err)
	}
	w.assertUnlinkClaim(t, id, vendorID, canceledRequest, agent.ClaimHeld, 1)
	if _, ok, err := w.l.Links.Get(t.Context(), w.env.DB.Pool, w.org.ID); err != nil || ok {
		t.Fatalf("link still present after failed vendor release: %v, %v", ok, err)
	}

	w.cursor.FailClaimCalls(false)
	w.l.Agents.Poll(t.Context())
	w.assertUnlinkClaim(t, id, vendorID, canceledRequest, agent.ClaimReleased, 2)
	t.Log("Unlink committed while Cursor returned HTTP 503. The next claim poll released the held claim.")
}

func newUnlinkClaimWorld(t *testing.T, opts ...testenv.Option) *cursorWorld {
	t.Helper()
	fake := cursorfake.New(t, cursorKey, "sandrun-pool")
	timing := fastAgentTiming()
	timing.PollInterval = time.Hour // Keep automatic retries out of the ordering checks.
	w := startCursorWorld(t, sandrunv1.GuestOs_GUEST_OS_LINUX, timing, fake, opts...)
	eventually(t, "the Cursor stream", func() bool { return fake.Streams() == 1 })
	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) {
		o.CreateError = connect.NewWireError(connect.CodeUnavailable, errors.New("full"))
	})
	return w
}

func (w *cursorWorld) queueUnlinkClaim(t *testing.T, vendorID string) string {
	t.Helper()
	w.cursor.AddRequest(vendorID, true)
	id := w.requestBySource(t, vendorID).GetRequestId()
	w.assertUnlinkClaim(t, id, vendorID, queuedRequest, agent.ClaimHeld, 0)
	return id
}

func (w *cursorWorld) assertUnlinkClaim(t *testing.T, id, vendorID string, state sandrunv1.RequestState, claimState agent.ClaimState, attempts int) {
	t.Helper()
	r := w.request(t, id)
	if r.GetState() != state || (state == canceledRequest && r.GetEndReason() != linkRemoved) {
		t.Fatalf("%s: request state/reason = %v/%v, want state %v", vendorID, r.GetState(), r.GetEndReason(), state)
	}
	claim, err := w.l.Agents.Claim(t.Context(), id)
	if err != nil || claim.State != claimState {
		t.Fatalf("%s: claim = %+v, %v, want %v", vendorID, claim, err, claimState)
	}
	if got := w.cursor.FailAttempts(vendorID); got != attempts || w.cursor.Released(vendorID) != 0 {
		t.Fatalf("%s: Cursor fail attempts = %d, releases = %d, want %d and 0", vendorID, got, w.cursor.Released(vendorID), attempts)
	}
	if _, held := w.cursor.Claim(vendorID); held != (claimState == agent.ClaimHeld) {
		t.Fatalf("%s: Cursor reports held = %v, claim state = %v", vendorID, held, claimState)
	}
}

type unlinkGate struct {
	reached chan struct{}
	resume  chan struct{}
}

func newUnlinkGate() *unlinkGate {
	return &unlinkGate{reached: make(chan struct{}), resume: make(chan struct{})}
}

func (g *unlinkGate) pause(ctx context.Context) {
	if g == nil {
		return
	}
	close(g.reached)
	select {
	case <-g.resume:
	case <-ctx.Done():
	}
}

func (g *unlinkGate) wait(t *testing.T, ctx context.Context) {
	t.Helper()
	select {
	case <-g.reached:
	case <-ctx.Done():
		t.Fatal("unlink did not reach the commit gate:", ctx.Err())
	}
}

type unlinkCommitGates struct{ before, after *unlinkGate }
type unlinkTraceKey struct{}

// The tracer pauses only the transaction that deletes the link. At QueryEnd
// on a successful COMMIT, a separate connection can see its changes.
type unlinkCommitTracer struct {
	next atomic.Pointer[unlinkCommitGates]
	txs  sync.Map
}

func (tr *unlinkCommitTracer) TraceQueryStart(ctx context.Context, conn *pgx.Conn, q pgx.TraceQueryStartData) context.Context {
	switch q.SQL {
	case "DELETE FROM ownsand_links WHERE ownsand_link_id = $1":
		if gates := tr.next.Swap(nil); gates != nil {
			tr.txs.Store(conn, gates)
		}
	case "rollback":
		tr.txs.Delete(conn)
	case "commit":
		if value, ok := tr.txs.LoadAndDelete(conn); ok {
			gates := value.(*unlinkCommitGates)
			gates.before.pause(ctx)
			return context.WithValue(ctx, unlinkTraceKey{}, gates.after)
		}
	}
	return ctx
}

func (*unlinkCommitTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, q pgx.TraceQueryEndData) {
	if gate, ok := ctx.Value(unlinkTraceKey{}).(*unlinkGate); ok && q.Err == nil {
		gate.pause(ctx)
	}
}
