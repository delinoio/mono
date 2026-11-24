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
	"google.golang.org/protobuf/types/known/durationpb"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/servers/tide-api/internal/devsession"
	"github.com/delinoio/mono/servers/tide-api/internal/ownsandclient/ownsandfake"
)

func TestWarmBindingQueueDeadline(t *testing.T) {
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
			var warm *tidev1.Runner
			eventually(t, "an idle warm runner", func() bool {
				resp, err := w.env.User(w.org.Owner).Work.ListRunners(context.Background(), connect.NewRequest(&tidev1.ListRunnersRequest{
					OrganizationId: w.org.ID,
					States:         []tidev1.RunnerState{tidev1.RunnerState_RUNNER_STATE_WARM},
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

			commit := holdTx(t, w.env, "SELECT 1 FROM runners WHERE runner_id = $1 FOR UPDATE", warm.GetRunnerId())
			member := w.env.User(w.org.Member)
			id := w.createSession(t, member, profileID).GetDevSessionId()
			waitForLockWaiter(t, w.env)
			w.env.Clock.Advance(tc.advance)
			commit()

			if tc.advance == time.Hour {
				w.waitState(t, id, tidev1.RequestState_REQUEST_STATE_EXPIRED)
				assertQueueExpiry(t, workRequest(t, w, id))
				runner := w.runner(t, warm.GetRunnerId())
				if runner.GetState() != tidev1.RunnerState_RUNNER_STATE_WARM || runner.GetRequestId() != "" {
					t.Fatalf("runner after expiry: %v", runner)
				}
				id = w.createSession(t, member, profileID).GetDevSessionId()
			}
			w.waitState(t, id, tidev1.RequestState_REQUEST_STATE_RUNNING)
			w.env.Clock.Advance(2 * time.Millisecond)
			w.l.Engine.Pass(context.Background(), w.org.ID)
			req := workRequest(t, w, id)
			if req.GetState() != tidev1.RequestState_REQUEST_STATE_RUNNING || req.GetStartAttempts() != 1 || req.GetRunnerId() != warm.GetRunnerId() {
				t.Fatalf("request after warm binding: %v", req)
			}
		})
	}
}

func TestColdBindingQueueDeadline(t *testing.T) {
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
				w.waitState(t, id, tidev1.RequestState_REQUEST_STATE_STARTING)
				w.env.Clock.Advance(2 * time.Millisecond)
				w.l.Engine.Pass(context.Background(), w.org.ID)
				req := workRequest(t, w, id)
				if req.GetState() != tidev1.RequestState_REQUEST_STATE_STARTING || req.GetStartAttempts() != 1 {
					t.Fatalf("request after the queue deadline: %v", req)
				}
				w.fake.SetRunning(w.osOrg, w.runner(t, req.GetRunnerId()).GetOwnsandSandboxId())
				w.waitState(t, id, tidev1.RequestState_REQUEST_STATE_RUNNING)
				return
			}

			w.waitState(t, id, tidev1.RequestState_REQUEST_STATE_EXPIRED)
			assertQueueExpiry(t, workRequest(t, w, id))
			assertUnusedSandboxDeleted(t, w.fake, w.osOrg)
			if got := w.fake.Calls("CreateSandbox"); got != int(call) {
				t.Fatalf("CreateSandbox calls: %d, want %d", got, call)
			}
		})
	}
}

func workRequest(t *testing.T, w *workWorld, id string) *tidev1.Request {
	t.Helper()
	resp, err := w.env.User(w.org.Owner).Work.GetRequest(context.Background(), connect.NewRequest(&tidev1.GetRequestRequest{
		OrganizationId: w.org.ID, RequestId: id,
	}))
	if err != nil {
		t.Fatal(err)
	}
	return resp.Msg.GetRequest()
}

func assertQueueExpiry(t *testing.T, req *tidev1.Request) {
	t.Helper()
	if req.GetState() != tidev1.RequestState_REQUEST_STATE_EXPIRED ||
		req.GetEndReason() != tidev1.RequestEndReason_REQUEST_END_REASON_MAX_QUEUE_WAIT ||
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
