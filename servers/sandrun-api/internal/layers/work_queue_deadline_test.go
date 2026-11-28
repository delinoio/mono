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
	"google.golang.org/protobuf/types/known/durationpb"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/servers/sandrun-api/internal/app"
	"github.com/delinoio/mono/servers/sandrun-api/internal/db"
	"github.com/delinoio/mono/servers/sandrun-api/internal/devsession"
	"github.com/delinoio/mono/servers/sandrun-api/internal/layers"
	"github.com/delinoio/mono/servers/sandrun-api/internal/lease"
	"github.com/delinoio/mono/servers/sandrun-api/internal/ownsandclient/ownsandfake"
	"github.com/delinoio/mono/servers/sandrun-api/internal/testenv"
)

func TestWarmBindingQueueDeadline(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		advance time.Duration
	}{
		{"at_deadline", time.Hour},
		{"before_deadline", time.Hour - time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorkWorld(t, devsession.Timing{})
			profileID := w.createProfile(t, devProfileProto("warm", 1, 0))
			var warm *sandrunv1.Runner
			eventually(t, "an idle warm runner", func() bool {
				resp, err := w.env.User(w.org.Owner).Work.ListRunners(context.Background(), connect.NewRequest(&sandrunv1.ListRunnersRequest{
					OrganizationId: w.org.ID,
					States:         []sandrunv1.RunnerState{sandrunv1.RunnerState_RUNNER_STATE_WARM},
				}))
				if err != nil {
					t.Fatal(err)
				}
				if len(resp.Msg.GetRunners()) != 1 {
					return false
				}
				warm = resp.Msg.GetRunners()[0]
				return true
			})

			holder := holdTx(t, w.env, "SELECT 1 FROM runners WHERE runner_id = $1 FOR UPDATE", warm.GetRunnerId())
			member := w.env.User(w.org.Member)
			id := w.createSession(t, member, profileID).GetDevSessionId()
			waitForLockWaiter(t, w.env, lockWait{blockerPID: holder.pid, query: "FROM runners WHERE runner_id = $1 FOR UPDATE"})
			w.env.Clock.Advance(tc.advance)
			holder.commit()

			if tc.advance == time.Hour {
				w.waitState(t, id, sandrunv1.RequestState_REQUEST_STATE_EXPIRED)
				assertQueueExpiry(t, workRequest(t, w, id))
				runner := w.runner(t, warm.GetRunnerId())
				if runner.GetState() != sandrunv1.RunnerState_RUNNER_STATE_WARM || runner.GetRequestId() != "" {
					t.Fatalf("runner after expiry: %v", runner)
				}
				id = w.createSession(t, member, profileID).GetDevSessionId()
			}
			w.waitState(t, id, sandrunv1.RequestState_REQUEST_STATE_RUNNING)
			w.env.Clock.Advance(2 * time.Millisecond)
			w.l.Engine.Pass(context.Background(), w.org.ID)
			req := workRequest(t, w, id)
			if req.GetState() != sandrunv1.RequestState_REQUEST_STATE_RUNNING || req.GetStartAttempts() != 1 || req.GetRunnerId() != warm.GetRunnerId() {
				t.Fatalf("request after warm binding: %v", req)
			}
		})
	}
}

type warmBindingGate struct {
	runnerID    string
	beforeLock  string
	entered     chan struct{}
	resume      chan struct{}
	runnerLocks int
}

func (g *warmBindingGate) TraceQueryStart(ctx context.Context, _ *pgx.Conn, q pgx.TraceQueryStartData) context.Context {
	if ctx.Value(manualDispatchKey{}) == nil {
		return ctx
	}
	const runnerLock = "FROM runners WHERE runner_id = $1 FOR UPDATE"
	if strings.HasSuffix(q.SQL, runnerLock) && q.Args[0] == g.runnerID {
		g.runnerLocks++
	}
	if g.beforeLock != "" && strings.HasSuffix(q.SQL, g.beforeLock) {
		g.beforeLock = ""
		close(g.entered)
		select {
		case <-g.resume:
		case <-ctx.Done():
		}
	}
	return ctx
}

func (*warmBindingGate) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestWarmBindingSamePass(t *testing.T) {
	t.Parallel()
	const (
		queued   = sandrunv1.RequestState_REQUEST_STATE_QUEUED
		starting = sandrunv1.RequestState_REQUEST_STATE_STARTING
		canceled = sandrunv1.RequestState_REQUEST_STATE_CANCELED
		expired  = sandrunv1.RequestState_REQUEST_STATE_EXPIRED
	)
	for _, tc := range []struct {
		name          string
		first, second sandrunv1.RequestState
		staleRunner   bool
		createCalls   int
	}{
		{name: "canceled", first: canceled, second: starting, createCalls: 1},
		{name: "committed_binding", first: starting, second: queued, createCalls: 2},
		{name: "binding_deadline", first: expired, second: starting, createCalls: 1},
		{name: "stale_runner", first: queued, second: queued, staleRunner: true, createCalls: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			w := newWorkWorld(t, devsession.Timing{})
			w.fake.Update(w.osOrg, func(o *ownsandfake.Org) { o.MaxSandboxes = 1 })
			profileID := w.createProfile(t, devProfileProto("warm", 1, 0))
			var runnerID string
			eventually(t, "one warm runner", func() bool {
				err := w.env.DB.Pool.QueryRow(ctx, "SELECT runner_id::text FROM runners WHERE runner_profile_id = $1 AND state = $2",
					profileID, int16(sandrunv1.RunnerState_RUNNER_STATE_WARM)).Scan(&runnerID)
				if err != nil && !errors.Is(err, pgx.ErrNoRows) {
					t.Fatal(err)
				}
				return err == nil
			})

			// Park the lease before creating demand so only the manual pass
			// can dispatch it and the reconciler cannot advance the runner.
			w.env.Stop()
			parked := lease.Lease{DB: w.env.DB, InstanceID: "parked"}
			if held, err := parked.TryAcquire(ctx); err != nil || !held {
				t.Fatalf("park lease: held=%v, err=%v", held, err)
			}
			defer parked.Release()

			g := &warmBindingGate{
				runnerID:   runnerID,
				beforeLock: "FROM requests WHERE request_id = $1 FOR UPDATE",
				entered:    make(chan struct{}),
				resume:     make(chan struct{}),
			}
			if tc.staleRunner {
				g.beforeLock = "FROM runners WHERE runner_id = $1 FOR UPDATE"
			}
			var replica layers.Layers
			w.env = w.env.NewReplica(t, testenv.WithLayers(w.fake, &replica), func(o *app.Options) {
				cfg := o.DB.Pool.Config()
				cfg.ConnConfig.Tracer = g
				pool, err := pgxpool.NewWithConfig(ctx, cfg)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(pool.Close)
				o.DB = &db.DB{Pool: pool}
			})
			member := w.env.User(w.org.Member)
			first := w.createSession(t, member, profileID).GetDevSessionId()
			w.env.Clock.Advance(time.Millisecond)
			second := w.createSession(t, member, profileID).GetDevSessionId()

			dispatched := make(chan struct{})
			go func() {
				defer close(dispatched)
				replica.Engine.Pass(context.WithValue(ctx, manualDispatchKey{}, true), w.org.ID)
			}()
			select {
			case <-g.entered:
			case <-dispatched:
				t.Fatal("dispatch ended before reaching the warm binding")
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			switch {
			case tc.first == canceled:
				if _, err := w.env.User(w.org.Owner).Work.CancelRequest(ctx, connect.NewRequest(&sandrunv1.CancelRequestRequest{
					OrganizationId: w.org.ID, RequestId: first,
				})); err != nil {
					t.Fatal(err)
				}
			case tc.first == expired:
				w.env.Clock.Advance(time.Hour - time.Millisecond)
			case tc.staleRunner:
				if _, err := w.env.DB.Pool.Exec(ctx, "UPDATE runners SET state = $2 WHERE runner_id = $1",
					runnerID, int16(sandrunv1.RunnerState_RUNNER_STATE_PREPARING)); err != nil {
					t.Fatal(err)
				}
			}
			close(g.resume)
			select {
			case <-dispatched:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}

			for _, want := range []struct {
				id    string
				state sandrunv1.RequestState
			}{{first, tc.first}, {second, tc.second}} {
				req := workRequest(t, w, want.id)
				var attempts int32
				var boundRunner string
				if want.state == starting {
					attempts, boundRunner = 1, runnerID
				}
				if req.GetState() != want.state || req.GetStartAttempts() != attempts || req.GetRunnerId() != boundRunner {
					t.Fatalf("request %s after dispatch: state=%s, attempts=%d, runner=%q, want %s, %d, %q",
						want.id, req.GetState(), req.GetStartAttempts(), req.GetRunnerId(), want.state, attempts, boundRunner)
				}
				if want.state == expired {
					assertQueueExpiry(t, req)
				}
				if want.state == canceled && req.GetEndReason() != sandrunv1.RequestEndReason_REQUEST_END_REASON_CANCELED_BY_USER {
					t.Fatalf("canceled request end reason: %s", req.GetEndReason())
				}
			}
			if got := w.fake.Calls("CreateSandbox"); got != tc.createCalls {
				t.Fatalf("CreateSandbox calls: %d, want %d", got, tc.createCalls)
			}
			wantLocks := 1
			if tc.second == starting {
				wantLocks = 2
			}
			if g.runnerLocks != wantLocks {
				t.Fatalf("warm runner locks: %d, want %d", g.runnerLocks, wantLocks)
			}
		})
	}
}

func TestColdBindingQueueDeadline(t *testing.T) {
	t.Parallel()
	const maxQueueWait = time.Second
	for _, tc := range []struct {
		name    string
		advance time.Duration
		retry   bool
	}{
		{"at_deadline", maxQueueWait, false},
		{"recover_lost_response_after_deadline", maxQueueWait + time.Millisecond, true},
		{"before_deadline", maxQueueWait - time.Millisecond, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorkWorld(t, devsession.Timing{})
			profile := devProfileProto("cold", 0, 0)
			profile.MaxQueueWait = durationpb.New(maxQueueWait)
			profileID := w.createProfile(t, profile)
			w.fake.Update(w.osOrg, func(o *ownsandfake.Org) {
				o.StartRunning = false
				if tc.retry {
					o.CreateResponseError = connect.NewError(connect.CodeDeadlineExceeded, errors.New("lost response"))
				}
			})
			call := int32(1)
			if tc.retry {
				call = 2
			}
			wait, resume := holdCreateSandbox(t, w.fake, call)
			id := w.createSession(t, w.env.User(w.org.Member), profileID).GetDevSessionId()
			wait()
			if tc.retry {
				if n := len(w.fake.Sandboxes(w.osOrg)); n != 1 {
					t.Fatalf("sandboxes after the lost response: %d, want 1", n)
				}
				w.fake.Update(w.osOrg, func(o *ownsandfake.Org) { o.CreateResponseError = nil })
			}
			w.env.Clock.Advance(tc.advance)
			resume()

			if tc.advance < maxQueueWait {
				w.waitState(t, id, sandrunv1.RequestState_REQUEST_STATE_STARTING)
				w.env.Clock.Advance(2 * time.Millisecond)
				w.l.Engine.Pass(context.Background(), w.org.ID)
				req := workRequest(t, w, id)
				if req.GetState() != sandrunv1.RequestState_REQUEST_STATE_STARTING || req.GetStartAttempts() != 1 {
					t.Fatalf("request after the queue deadline: %v", req)
				}
				w.fake.SetRunning(w.osOrg, w.runner(t, req.GetRunnerId()).GetOwnsandSandboxId())
				w.waitState(t, id, sandrunv1.RequestState_REQUEST_STATE_RUNNING)
				return
			}

			w.waitState(t, id, sandrunv1.RequestState_REQUEST_STATE_EXPIRED)
			assertQueueExpiry(t, workRequest(t, w, id))
			assertUnusedSandboxDeleted(t, w.fake, w.osOrg)
			if got := w.fake.Calls("CreateSandbox"); got != int(call) {
				t.Fatalf("CreateSandbox calls: %d, want %d", got, call)
			}
		})
	}
}

func workRequest(t *testing.T, w *workWorld, id string) *sandrunv1.Request {
	t.Helper()
	resp, err := w.env.User(w.org.Owner).Work.GetRequest(context.Background(), connect.NewRequest(&sandrunv1.GetRequestRequest{
		OrganizationId: w.org.ID, RequestId: id,
	}))
	if err != nil {
		t.Fatal(err)
	}
	return resp.Msg.GetRequest()
}

func assertQueueExpiry(t *testing.T, req *sandrunv1.Request) {
	t.Helper()
	if req.GetState() != sandrunv1.RequestState_REQUEST_STATE_EXPIRED ||
		req.GetEndReason() != sandrunv1.RequestEndReason_REQUEST_END_REASON_MAX_QUEUE_WAIT ||
		req.GetStartAttempts() != 0 || req.GetRunnerId() != "" {
		t.Fatalf("request after queue expiry: %v", req)
	}
}

func assertUnusedSandboxDeleted(t *testing.T, fake *ownsandfake.Fake, orgID string) {
	t.Helper()
	eventually(t, "the unused sandbox is deleted", func() bool {
		sandboxes := fake.Sandboxes(orgID)
		return len(sandboxes) == 1 && sandboxes[0].GetState() == ownsandv1.SandboxState_SANDBOX_STATE_DELETED
	})
	if got := fake.Calls("Exec"); got != 0 {
		t.Fatalf("guest exec calls: %d, want 0", got)
	}
}

func holdCreateSandbox(t *testing.T, fake *ownsandfake.Fake, call int32) (wait, resume func()) {
	t.Helper()
	entered, released := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	resume = sync.OnceFunc(func() { close(released) })
	t.Cleanup(resume)
	fake.SetCreateHook(func(ctx context.Context) error {
		if calls.Add(1) != call {
			return nil
		}
		close(entered)
		select {
		case <-released:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	return func() {
		t.Helper()
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("CreateSandbox did not reach the barrier")
		}
	}, resume
}
