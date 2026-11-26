// SPDX-License-Identifier: FSL-1.1-ALv2

package github_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/servers/sandrun-api/internal/ownsandclient"
)

func TestWarmDispatchContinuesAfterRejection(t *testing.T) {
	for _, pooled := range []bool{false, true} {
		t.Run(warmProfileKind(pooled), func(t *testing.T) {
			w, first, later := newWarmDispatchWorld(t, pooled)
			var attempts []string
			w.os.SetCreateRequestHook(func(_ context.Context, req *ownsandv1.CreateSandboxRequest) error {
				id := req.GetLabels()[ownsandclient.LabelRunnerProfileID]
				attempts = append(attempts, id)
				if id == first {
					return connect.NewWireError(connect.CodeUnavailable, errors.New("no device has free capacity for this profile"))
				}
				return nil
			})

			w.l.Engine.Pass(t.Context(), w.org.ID)
			if want := []string{first, later, later}; !slices.Equal(attempts, want) {
				t.Fatalf("first pass attempts = %v, want %v", attempts, want)
			}
			runners := w.runners(t)
			if len(runners) != 2 {
				t.Fatalf("first pass stored %d runners, want 2", len(runners))
			}
			for _, r := range runners {
				if r.GetRunnerProfileId() != later || r.GetOwnsandSandboxId() == "" {
					t.Fatalf("expected a sandbox for the later profile, got %v", r)
				}
			}

			attempts = nil
			w.l.Engine.Pass(t.Context(), w.org.ID)
			if want := []string{first}; !slices.Equal(attempts, want) {
				t.Fatalf("second pass attempts = %v, want %v", attempts, want)
			}
		})
	}
}

func TestWarmDispatchStopsAfterDeadlineOrCancellation(t *testing.T) {
	for _, pooled := range []bool{false, true} {
		t.Run(warmProfileKind(pooled), func(t *testing.T) {
			for _, scenario := range []string{"deadline", "cancellation"} {
				t.Run(scenario, func(t *testing.T) {
					w, first, _ := newWarmDispatchWorld(t, pooled)
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					var attempts []*ownsandv1.CreateSandboxRequest
					w.os.SetCreateRequestHook(func(callCtx context.Context, req *ownsandv1.CreateSandboxRequest) error {
						attempts = append(attempts, req)
						if scenario == "cancellation" {
							cancel()
						}
						<-callCtx.Done()
						return callCtx.Err()
					})

					w.l.Engine.Pass(ctx, w.org.ID)
					if len(attempts) != 1 || attempts[0].GetLabels()[ownsandclient.LabelRunnerProfileID] != first {
						t.Fatalf("expected one create attempt for the first profile, got %v", attempts)
					}
					runners := w.runners(t)
					if len(runners) != 1 || runners[0].GetState() != sandrunv1.RunnerState_RUNNER_STATE_CREATING || runners[0].GetOwnsandSandboxId() != "" {
						t.Fatalf("expected one uncertain CREATING runner, got %v", runners)
					}
					if scenario == "deadline" {
						checkWarmDriverRetry(t, w, attempts[0])
					}
				})
			}
		})
	}
}

func TestWarmDispatchWaitsForUncoveredQueue(t *testing.T) {
	w, _, _ := newWarmDispatchWorld(t, true)
	queuedProfile := w.createProfile(t, ghProfile("queued", 0))
	w.send(t, "workflow_job", job("queued", 501, 9001, []string{"sandrun-queued"}, ""))
	var attempts []string
	w.os.SetCreateRequestHook(func(_ context.Context, req *ownsandv1.CreateSandboxRequest) error {
		attempts = append(attempts, req.GetLabels()[ownsandclient.LabelRunnerProfileID])
		return connect.NewWireError(connect.CodeUnavailable, errors.New("no device has free capacity"))
	})

	w.l.Engine.Pass(t.Context(), w.org.ID)
	if want := []string{queuedProfile}; !slices.Equal(attempts, want) {
		t.Fatalf("create attempts = %v, want only the queued job's profile %v", attempts, want)
	}
	if req := w.requestOfJob(t, 9001); req.GetState() != sandrunv1.RequestState_REQUEST_STATE_QUEUED {
		t.Fatalf("request state = %v, want QUEUED", req.GetState())
	}
}

func newWarmDispatchWorld(t *testing.T, pooled bool) (*ghWorld, string, string) {
	t.Helper()
	w := newGHWorld(t, worldOpts{manualDispatch: true})
	first := ghProfile("first", 2)
	if pooled {
		w.link(t, 501, "acme")
		w.link(t, 502, "other")
	} else {
		first.WorkloadKind = sandrunv1.WorkloadKind_WORKLOAD_KIND_DEV_SESSION
		first.WorkspacePath = "/home/dev/work"
	}
	later := ghProfile("later", 2)
	later.WorkloadKind = sandrunv1.WorkloadKind_WORKLOAD_KIND_DEV_SESSION
	later.WorkspacePath = "/home/dev/work"
	// Profile IDs increase in creation order, which is also dispatch order.
	return w, w.createProfile(t, first), w.createProfile(t, later)
}

func warmProfileKind(pooled bool) string {
	if pooled {
		return "github_pool"
	}
	return "binding"
}

func checkWarmDriverRetry(t *testing.T, w *ghWorld, initial *ownsandv1.CreateSandboxRequest) {
	t.Helper()
	retry := make(chan *ownsandv1.CreateSandboxRequest, 1)
	w.os.SetCreateRequestHook(func(ctx context.Context, req *ownsandv1.CreateSandboxRequest) error {
		select {
		case retry <- req:
		case <-ctx.Done():
			return ctx.Err()
		}
		return nil
	})
	ctx, stop := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = w.l.Engine.RunReconciler(ctx)
	}()
	defer func() {
		stop()
		<-done
	}()
	eventually(t, "driver retries the uncertain create", func() bool { return len(retry) > 0 })
	if req := <-retry; !proto.Equal(req, initial) {
		t.Fatalf("driver retry = %v, want original request %v", req, initial)
	}
}
