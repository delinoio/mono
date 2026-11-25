// SPDX-License-Identifier: FSL-1.1-ALv2

package layers_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/servers/tide-api/internal/api"
	"github.com/delinoio/mono/servers/tide-api/internal/app"
	"github.com/delinoio/mono/servers/tide-api/internal/db"
	"github.com/delinoio/mono/servers/tide-api/internal/devsession"
	"github.com/delinoio/mono/servers/tide-api/internal/layers"
	"github.com/delinoio/mono/servers/tide-api/internal/lease"
	"github.com/delinoio/mono/servers/tide-api/internal/ownsandclient/ownsandfake"
	"github.com/delinoio/mono/servers/tide-api/internal/testenv"
	"github.com/delinoio/mono/servers/tide-api/internal/work"
)

type manualDispatchKey struct{}

// membershipRemovalGate pauses removal just before its first lock of the
// target request, after it has checked for a bound runner.
type membershipRemovalGate struct {
	requestID                      string
	beforeRemoval, resumeRemoval   chan struct{}
	beforeDispatch, resumeDispatch chan struct{}
	paused                         atomic.Bool
	attempts                       atomic.Int32
}

func (g *membershipRemovalGate) TraceQueryStart(ctx context.Context, _ *pgx.Conn, q pgx.TraceQueryStartData) context.Context {
	const lockRequest = "SELECT " + work.RequestColumns + " FROM requests WHERE request_id = $1 FOR UPDATE"
	if q.SQL == "DELETE FROM memberships WHERE membership_id = $1" {
		g.attempts.Add(1)
	}
	if q.SQL != lockRequest || q.Args[0] != g.requestID {
		return ctx
	}
	if ctx.Value(manualDispatchKey{}) != nil {
		close(g.beforeDispatch)
		select {
		case <-ctx.Done():
		case <-g.resumeDispatch:
		}
	} else if g.paused.CompareAndSwap(false, true) {
		close(g.beforeRemoval)
		select {
		case <-ctx.Done():
		case <-g.resumeRemoval:
		}
	}
	return ctx
}

func (*membershipRemovalGate) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestMembershipRemovalRacesWarmDispatch(t *testing.T) {
	for _, tc := range []struct {
		name          string
		dispatchFirst bool
		failDeletion  bool
		selfLeave     bool
	}{
		{name: "removal locks first"},
		{name: "dispatch starts first", dispatchFirst: true},
		{name: "self-leave retries failed sandbox deletion", dispatchFirst: true, failDeletion: true, selfLeave: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			w := newWorkWorld(t, devsession.Timing{})
			profileID := w.createProfile(t, devProfileProto("warm", 1, 0))
			var runnerID string
			eventually(t, "warm runner", func() bool {
				err := w.env.DB.Pool.QueryRow(ctx, "SELECT runner_id::text FROM runners WHERE runner_profile_id = $1 AND state = $2",
					profileID, int16(tidev1.RunnerState_RUNNER_STATE_WARM)).Scan(&runnerID)
				if err != nil && !errors.Is(err, pgx.ErrNoRows) {
					t.Fatal(err)
				}
				return err == nil
			})
			sandboxID := w.runner(t, runnerID).GetOwnsandSandboxId()
			if sandboxID == "" {
				t.Fatal("warm runner has no sandbox")
			}

			// Park the lease so the test controls when dispatch and the
			// reconciler run and can observe the inline deletion attempt.
			w.env.App.Lease.Release()
			parked := lease.Lease{DB: w.env.DB, InstanceID: "parked"}
			if held, err := parked.TryAcquire(ctx); err != nil || !held {
				t.Fatalf("park lease: held=%v, err=%v", held, err)
			}
			defer parked.Release()
			if _, err := w.env.DB.Pool.Exec(ctx, "UPDATE runner_profiles SET warm_count = 0 WHERE runner_profile_id = $1", profileID); err != nil {
				t.Fatal(err)
			}

			_, key := w.env.NewAPIKey(w.org.ID, w.org.Member)
			member := w.env.Bearer(key)
			sessionID := w.createSession(t, member, profileID).GetDevSessionId()
			g := &membershipRemovalGate{
				requestID:     sessionID,
				beforeRemoval: make(chan struct{}), resumeRemoval: make(chan struct{}),
				beforeDispatch: make(chan struct{}), resumeDispatch: make(chan struct{}),
			}
			var replica layers.Layers
			remover := w.env.NewReplica(t, testenv.WithLayers(w.fake, &replica), func(o *app.Options) {
				cfg := o.DB.Pool.Config()
				cfg.ConnConfig.Tracer = g
				pool, err := pgxpool.NewWithConfig(ctx, cfg)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(pool.Close)
				o.DB = &db.DB{Pool: pool}
			})
			who := w.org.Admin
			if tc.selfLeave {
				who = w.org.Member
			}
			removed := make(chan error, 1)
			go func() {
				_, err := remover.User(who).Member.RemoveMember(ctx, connect.NewRequest(&tidev1.RemoveMemberRequest{OrganizationId: w.org.ID, UserId: w.org.Member}))
				removed <- err
			}()
			select {
			case <-g.beforeRemoval:
			case err := <-removed:
				t.Fatalf("removal returned before it locked the session: %v", err)
			case <-ctx.Done():
				t.Fatal("removal did not reach the request lock")
			}

			dispatched := make(chan struct{})
			if tc.dispatchFirst {
				w.l.Engine.Pass(ctx, w.org.ID)
				runCtx, stop := context.WithCancel(ctx)
				done := make(chan struct{})
				go func() {
					defer close(done)
					_ = w.l.Engine.RunReconciler(runCtx)
				}()
				defer func() { stop(); <-done }()
				running := w.waitState(t, sessionID, tidev1.RequestState_REQUEST_STATE_RUNNING)
				if r := w.runner(t, running.GetRunnerId()); r.GetRunnerId() != runnerID || r.GetState() != tidev1.RunnerState_RUNNER_STATE_BUSY {
					t.Fatalf("dispatch did not start the warm runner: %v", r)
				}
				stop()
				<-done
			} else {
				// Let dispatch select the queued request and the warm runner
				// before removal wins the request lock.
				go func() {
					defer close(dispatched)
					replica.Engine.Pass(context.WithValue(ctx, manualDispatchKey{}, true), w.org.ID)
				}()
				select {
				case <-g.beforeDispatch:
				case <-dispatched:
					t.Fatal("dispatch ended before trying to bind the request")
				case <-ctx.Done():
					t.Fatal("dispatch did not reach the request lock")
				}
			}
			if tc.failDeletion {
				w.fake.Update(w.osOrg, func(o *ownsandfake.Org) {
					o.DeleteError = connect.NewWireError(connect.CodeUnavailable, errors.New("device busy"))
				})
			}
			close(g.resumeRemoval)
			if err := <-removed; err != nil {
				t.Fatal(err)
			}

			wantState, wantAttempts := tidev1.RequestState_REQUEST_STATE_CANCELED, int32(1)
			if tc.dispatchFirst {
				wantState, wantAttempts = tidev1.RequestState_REQUEST_STATE_ENDED, 2
			}
			if got := g.attempts.Load(); got != wantAttempts {
				t.Errorf("membership removal attempts = %d, want %d", got, wantAttempts)
			}
			s := w.session(t, sessionID)
			if s.GetState() != wantState || s.GetEndReason() != tidev1.RequestEndReason_REQUEST_END_REASON_MEMBER_REMOVED {
				t.Fatalf("session after removal: %v, want %s/MEMBER_REMOVED", s, wantState)
			}
			_, err := member.DevSession.ListDevSessions(ctx, connect.NewRequest(&tidev1.ListDevSessionsRequest{OrganizationId: w.org.ID}))
			wantCode(t, "revoked key", err, connect.CodeUnauthenticated)
			for _, action := range []tidev1.AuditAction{tidev1.AuditAction_AUDIT_ACTION_MEMBER_REMOVED, tidev1.AuditAction_AUDIT_ACTION_API_KEY_REVOKED} {
				if got := countAudit(t, w.env, w.org.ID, action); got != 1 {
					t.Errorf("audit count for %s = %d, want 1", action, got)
				}
			}

			if !tc.dispatchFirst {
				close(g.resumeDispatch)
				<-dispatched
				s := w.session(t, sessionID)
				r := w.runner(t, runnerID)
				if s.GetRunnerId() != "" || r.GetRequestId() != "" || r.GetState() != tidev1.RunnerState_RUNNER_STATE_WARM || w.fake.Calls("DeleteSandbox") != 0 {
					t.Fatalf("later dispatch modified the warm runner or canceled session: session=%v runner=%v", s, r)
				}
				return
			}
			if got := w.fake.Calls("DeleteSandbox"); got != 1 {
				t.Fatalf("inline DeleteSandbox calls = %d, want 1", got)
			}
			if tc.failDeletion {
				if r := w.runner(t, runnerID); r.GetState() != tidev1.RunnerState_RUNNER_STATE_DELETING || r.GetOwnsandSandboxId() != sandboxID || w.fake.Active(w.osOrg) != 1 {
					t.Fatalf("runner after transient error: %v", r)
				}
				w.fake.Update(w.osOrg, func(o *ownsandfake.Org) { o.DeleteError = nil })
				parked.Release()
				w.env.NewReplica(t, testenv.WithLayers(w.fake, &layers.Layers{}))
			}
			eventually(t, "original sandbox deleted", func() bool {
				sandboxes := w.fake.Sandboxes(w.osOrg)
				return len(sandboxes) == 1 && sandboxes[0].GetSandboxId() == sandboxID &&
					sandboxes[0].GetState() == ownsandv1.SandboxState_SANDBOX_STATE_DELETED &&
					w.runner(t, runnerID).GetState() == tidev1.RunnerState_RUNNER_STATE_DELETED
			})
		})
	}
}

func TestMembershipRemovalRetryExhaustionRollsBack(t *testing.T) {
	w := newWorkWorld(t, devsession.Timing{})
	_, key := w.env.NewAPIKey(w.org.ID, w.org.Member)
	member := w.env.Bearer(key)
	profileID := w.createProfile(t, devProfileProto("linux-dev", 0, 0))
	running := w.createSession(t, member, profileID)
	running = w.waitState(t, running.GetDevSessionId(), tidev1.RequestState_REQUEST_STATE_RUNNING)

	var attempts, afterCommit atomic.Int32
	remover := w.env.NewReplica(t, testenv.WithLayers(w.fake, &layers.Layers{}), testenv.WithExtension(func(r *app.Registry) error {
		r.OnMembershipEnd(api.MembershipEndHook{
			Name: "force-retry-after-session-cleanup",
			InTx: func(ctx context.Context, tx pgx.Tx, _ api.MembershipEnd) error {
				var state int16
				if err := tx.QueryRow(ctx, "SELECT state FROM requests WHERE request_id = $1", running.GetDevSessionId()).Scan(&state); err != nil {
					return err
				}
				if tidev1.RequestState(state) != tidev1.RequestState_REQUEST_STATE_ENDED {
					return errors.New("session was not ended in the removal transaction")
				}
				attempts.Add(1)
				return api.ErrRetryMembershipEnd
			},
			AfterCommit: func(context.Context, api.MembershipEnd) { afterCommit.Add(1) },
		})
		return nil
	}))
	ctx := t.Context()
	_, err := remover.User(w.org.Member).Member.RemoveMember(ctx, connect.NewRequest(&tidev1.RemoveMemberRequest{OrganizationId: w.org.ID, UserId: w.org.Member}))
	wantCode(t, "exhausted removal retries", err, connect.CodeUnavailable)
	if attempts.Load() != 3 || afterCommit.Load() != 0 {
		t.Fatalf("attempts=%d after commit=%d, want 3 and 0", attempts.Load(), afterCommit.Load())
	}
	if r := w.runner(t, running.GetRunnerId()); r.GetState() != tidev1.RunnerState_RUNNER_STATE_BUSY || w.fake.Calls("DeleteSandbox") != 0 {
		t.Fatalf("runner changed after rollback: %v", r)
	}
	s, err := member.DevSession.GetDevSession(ctx, connect.NewRequest(&tidev1.GetDevSessionRequest{OrganizationId: w.org.ID, DevSessionId: running.GetDevSessionId()}))
	if err != nil || s.Msg.GetDevSession().GetState() != tidev1.RequestState_REQUEST_STATE_RUNNING {
		t.Fatalf("session or member key changed after rollback: %v, %v", s, err)
	}
	for _, action := range []tidev1.AuditAction{tidev1.AuditAction_AUDIT_ACTION_MEMBER_REMOVED, tidev1.AuditAction_AUDIT_ACTION_API_KEY_REVOKED} {
		if got := countAudit(t, w.env, w.org.ID, action); got != 0 {
			t.Errorf("audit count for %s = %d, want 0", action, got)
		}
	}
}
