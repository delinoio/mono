// SPDX-License-Identifier: FSL-1.1-ALv2

package github_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/servers/sandrun-api/internal/api"
	"github.com/delinoio/mono/servers/sandrun-api/internal/layers"
	"github.com/delinoio/mono/servers/sandrun-api/internal/ownsandclient"
	"github.com/delinoio/mono/servers/sandrun-api/internal/testenv"
)

func TestJobSandboxCreateRejectedAfterRestart(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"queued", "expired", "canceled", "started", "warm_fill", "legacy", "removed_sponsor", "capacity"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := t.Context()
			w := newGHWorld(t, worldOpts{manualDispatch: true})
			const installation, sponsor, other = 501, 9001, 9002
			w.link(t, installation, "acme")
			warm := int32(0)
			if scenario == "warm_fill" {
				warm = 1
			}
			w.createProfile(t, ghProfile("linux", warm))
			if scenario != "warm_fill" {
				w.send(t, "workflow_job", job("queued", installation, sponsor, sandRunLinux, ""))
			}

			type createArrival struct {
				request *ownsandv1.CreateSandboxRequest
				at      time.Time
			}
			var initialCalls atomic.Int32
			initialArrivals := make(chan createArrival, 1)
			firstPassCtx, cancelFirstPass := context.WithCancel(ctx)
			defer cancelFirstPass()
			firstResponse := make(chan struct{})
			releaseFirstResponse := sync.OnceFunc(func() { close(firstResponse) })
			initialFactory := newCreateSandboxPeer(t, func(_ context.Context, req *ownsandv1.CreateSandboxRequest) error {
				arrival := createArrival{request: req, at: time.Now()}
				if initialCalls.Add(1) == 1 {
					initialArrivals <- arrival
				}
				select {
				case <-firstResponse:
				case <-firstPassCtx.Done():
				}
				return context.DeadlineExceeded
			})
			// Cleanup runs in reverse order. Release the handler before closing the peer.
			t.Cleanup(releaseFirstResponse)
			client, err := initialFactory.New(w.osOrg, "osk_"+strings.Repeat("a", 43))
			if err != nil {
				t.Fatal(err)
			}
			type createResult struct {
				deadline time.Time
				err      error
			}
			initialResults := make(chan createResult, 1)
			w.os.SetCreateRequestHook(func(ctx context.Context, req *ownsandv1.CreateSandboxRequest) error {
				deadline, _ := ctx.Deadline()
				_, err := client.CreateSandbox(ctx, req)
				select {
				case initialResults <- createResult{deadline: deadline, err: err}:
				default:
				}
				return err
			})
			// Bound the pass even if the RPC deadline is missing and the peer never sees a request.
			const watchdogTimeout = 5 * time.Second
			watchdog := time.AfterFunc(watchdogTimeout, cancelFirstPass)
			defer watchdog.Stop()
			w.l.Engine.Pass(firstPassCtx, w.org.ID)
			releaseFirstResponse()
			if firstPassCtx.Err() != nil {
				t.Fatalf("first create exceeded the %s watchdog while waiting for the RPC deadline", watchdogTimeout)
			}
			var initialResult createResult
			select {
			case initialResult = <-initialResults:
			default:
				t.Fatal("dispatcher did not attempt a create")
			}
			if initialResult.deadline.IsZero() {
				t.Fatal("fixture: dispatcher create context had no deadline")
			}
			if err := initialResult.err; connect.IsWireError(err) || connect.CodeOf(err) != connect.CodeDeadlineExceeded {
				t.Fatalf("first create error = %v, want a non-wire deadline error", err)
			}
			var firstArrival createArrival
			select {
			case firstArrival = <-initialArrivals:
			case <-firstPassCtx.Done():
				t.Fatal("fixture: initial OwnSand endpoint did not record the dispatcher create")
			}
			watchdog.Stop()
			if !firstArrival.at.Before(initialResult.deadline) {
				t.Fatalf("fixture: initial OwnSand endpoint recorded the dispatcher create %s after its deadline", firstArrival.at.Sub(initialResult.deadline))
			}
			if got := initialCalls.Load(); got != 1 {
				t.Fatalf("fixture: initial OwnSand endpoint received %d creates, want 1", got)
			}
			runners := w.runnersIn(t, sandrunv1.RunnerState_RUNNER_STATE_CREATING)
			if len(runners) != 1 || runners[0].GetRequestId() != "" || runners[0].GetOwnsandSandboxId() != "" {
				t.Fatalf("expected one unbound create reservation: %v", runners)
			}
			runnerID := runners[0].GetRunnerId()

			if scenario == "expired" {
				w.env.Clock.Advance(2 * time.Hour)
			}
			w.send(t, "workflow_job", job("queued", installation, other, sandRunLinux, ""))
			switch scenario {
			case "canceled", "removed_sponsor":
				w.send(t, "workflow_job", job("completed", installation, sponsor, sandRunLinux, ""))
				canceled := w.requestOfJob(t, sponsor)
				if canceled.GetState() != sandrunv1.RequestState_REQUEST_STATE_CANCELED ||
					canceled.GetEndReason() != sandrunv1.RequestEndReason_REQUEST_END_REASON_CANCELED_BY_SOURCE {
					t.Fatalf("job after completion webhook: %v", canceled)
				}
				if scenario == "removed_sponsor" {
					if _, err := w.env.DB.Pool.Exec(ctx, "DELETE FROM requests WHERE request_id = $1", canceled.GetRequestId()); err != nil {
						t.Fatal(err)
					}
				}
			case "started":
				// Seed another waiting runner so GitHub can assign it the
				// job while the original runner has no sandbox.
				waitingID := api.NewID()
				if _, err := w.env.DB.Pool.Exec(ctx, `INSERT INTO runners
					(runner_id, organization_id, runner_profile_id, kind, guest_os, state, pool_key, idempotency_key,
						create_request, ownsand_sandbox_id, create_time, update_time)
					SELECT $2, organization_id, runner_profile_id, kind, guest_os, $3, pool_key,
						gen_random_uuid(), create_request, gen_random_uuid(), create_time, update_time
					FROM runners WHERE runner_id = $1`, runnerID, waitingID,
					int16(sandrunv1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK)); err != nil {
					t.Fatal(err)
				}
				w.send(t, "workflow_job", job("in_progress", installation, sponsor, sandRunLinux, "sandrun-"+waitingID))
				if started := w.requestOfJob(t, sponsor); started.GetState() != sandrunv1.RequestState_REQUEST_STATE_RUNNING ||
					started.GetRunnerId() != waitingID {
					t.Fatalf("job after in_progress webhook: %v", started)
				}
			case "legacy":
				if _, err := w.env.DB.Pool.Exec(ctx, "UPDATE runners SET sponsor_request_id = NULL WHERE runner_id = $1", runnerID); err != nil {
					t.Fatal(err)
				}
			}
			before, otherBefore := w.requestOfJob(t, sponsor), w.requestOfJob(t, other)
			if otherBefore.GetState() != sandrunv1.RequestState_REQUEST_STATE_QUEUED {
				t.Fatalf("other job before retry: %v", otherBefore)
			}
			if scenario == "queued" || scenario == "expired" || scenario == "legacy" || scenario == "capacity" {
				if before.GetState() != sandrunv1.RequestState_REQUEST_STATE_QUEUED {
					t.Fatalf("sponsor before retry: %v", before)
				}
			}
			w.l.GitHub.Wait()
			w.env.Stop()
			var recoveryCalls atomic.Int32
			recoveryRequests := make(chan *ownsandv1.CreateSandboxRequest, 1)
			recoveryFactory := newCreateSandboxPeer(t, func(_ context.Context, req *ownsandv1.CreateSandboxRequest) error {
				if recoveryCalls.Add(1) == 1 {
					recoveryRequests <- req
				}
				if scenario == "capacity" {
					return connect.NewError(connect.CodeUnavailable, errors.New("no free capacity"))
				}
				return connect.NewError(connect.CodeFailedPrecondition, errors.New("no eligible device"))
			})
			var recovered layers.Layers
			w.env = w.env.NewReplica(t, testenv.WithLayerOptions(layers.Options{OwnSand: recoveryFactory, GitHub: w.githubOptions(w.env.Clock.Now)}, &recovered))
			// Run only reconciliation on the new instance. An ordinary
			// dispatch pass could obscure which create ended the job.
			reconcileCtx, stop := context.WithCancel(ctx)
			done := make(chan struct{})
			go func() {
				defer close(done)
				_ = recovered.Engine.RunReconciler(reconcileCtx)
			}()
			t.Cleanup(func() { stop(); <-done })

			if scenario == "capacity" {
				eventually(t, "the capacity rejection removes the reservation", func() bool {
					return len(w.runners(t)) == 0
				})
			} else {
				eventually(t, "the original runner records its rejected create", func() bool {
					return w.runner(t, runnerID).GetState() == sandrunv1.RunnerState_RUNNER_STATE_FAILED
				})
				runner := w.runner(t, runnerID)
				if runner.GetRequestId() != "" || runner.GetOwnsandSandboxId() != "" ||
					runner.GetEndReason() != sandrunv1.RunnerEndReason_RUNNER_END_REASON_START_FAILED {
					t.Fatalf("runner after retry: %v", runner)
				}
			}
			after := w.requestOfJob(t, sponsor)
			if scenario == "queued" || scenario == "expired" {
				state, reason := sandrunv1.RequestState_REQUEST_STATE_FAILED, sandrunv1.RequestEndReason_REQUEST_END_REASON_NO_ELIGIBLE_DEVICE
				if scenario == "expired" {
					state, reason = sandrunv1.RequestState_REQUEST_STATE_EXPIRED, sandrunv1.RequestEndReason_REQUEST_END_REASON_MAX_QUEUE_WAIT
				}
				if after.GetState() != state || after.GetEndReason() != reason ||
					after.GetStartAttempts() != 0 || after.GetRunnerId() != "" {
					t.Fatalf("sponsoring job after retry: %v", after)
				}
			} else if !proto.Equal(after, before) {
				t.Fatalf("retry changed the job: before %v, after %v", before, after)
			}
			if got := w.requestOfJob(t, other); !proto.Equal(got, otherBefore) {
				t.Fatalf("retry changed the other queued job: %v", got)
			}
			if got := recoveryCalls.Load(); got != 1 || len(recoveryRequests) != 1 {
				t.Fatalf("fixture: recovery OwnSand endpoint received %d creates, want 1", got)
			}
			first, second := firstArrival.request, <-recoveryRequests
			if first.GetIdempotencyKey() == "" || !proto.Equal(first, second) {
				t.Fatal("the retry changed the OwnSand create parameters or idempotency key")
			}
			t.Logf("OwnSand received identical creates before and after restart. Sponsor state after rejection: %s. Other job stayed QUEUED.", after.GetState())
		})
	}
}

func newCreateSandboxPeer(t *testing.T, create func(context.Context, *ownsandv1.CreateSandboxRequest) error) ownsandclient.SDKFactory {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(ownsandv1connect.SandboxServiceCreateSandboxProcedure, connect.NewUnaryHandler(
		ownsandv1connect.SandboxServiceCreateSandboxProcedure,
		func(ctx context.Context, req *connect.Request[ownsandv1.CreateSandboxRequest]) (*connect.Response[ownsandv1.CreateSandboxResponse], error) {
			return nil, create(ctx, req.Msg)
		},
	))
	peer := httptest.NewUnstartedServer(mux)
	peer.Config.Protocols = new(http.Protocols)
	peer.Config.Protocols.SetUnencryptedHTTP2(true)
	peer.Start()
	t.Cleanup(peer.Close)
	return ownsandclient.SDKFactory{BaseURL: peer.URL}
}
