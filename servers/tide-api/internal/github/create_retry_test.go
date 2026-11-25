// SPDX-License-Identifier: FSL-1.1-ALv2

package github_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/servers/tide-api/internal/api"
	"github.com/delinoio/mono/servers/tide-api/internal/layers"
	"github.com/delinoio/mono/servers/tide-api/internal/ownsandclient"
	"github.com/delinoio/mono/servers/tide-api/internal/testenv"
)

func TestJobSandboxCreateRejectedAfterRestart(t *testing.T) {
	for _, scenario := range []string{"queued", "canceled", "started", "warm_fill", "legacy", "removed_sponsor"} {
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
				w.send(t, "workflow_job", job("queued", installation, sponsor, tideLinux, ""))
			}

			var calls atomic.Int32
			requests := make(chan *ownsandv1.CreateSandboxRequest, 2)
			firstResponse := make(chan struct{})
			mux := http.NewServeMux()
			mux.Handle(ownsandv1connect.SandboxServiceCreateSandboxProcedure, connect.NewUnaryHandler(
				ownsandv1connect.SandboxServiceCreateSandboxProcedure,
				func(_ context.Context, req *connect.Request[ownsandv1.CreateSandboxRequest]) (*connect.Response[ownsandv1.CreateSandboxResponse], error) {
					call := calls.Add(1)
					if call <= 2 {
						requests <- req.Msg
					}
					if call == 1 {
						<-firstResponse
						return nil, context.DeadlineExceeded
					}
					return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("no eligible device"))
				},
			))
			peer := httptest.NewUnstartedServer(mux)
			peer.Config.Protocols = new(http.Protocols)
			peer.Config.Protocols.SetUnencryptedHTTP2(true)
			peer.Start()
			t.Cleanup(peer.Close)
			factory := ownsandclient.SDKFactory{BaseURL: peer.URL}
			client, err := factory.New(w.osOrg, "osk_"+strings.Repeat("a", 43))
			if err != nil {
				t.Fatal(err)
			}
			initialError := make(chan error, 1)
			w.os.SetCreateRequestHook(func(ctx context.Context, req *ownsandv1.CreateSandboxRequest) error {
				_, err := client.CreateSandbox(ctx, req)
				initialError <- err
				return err
			})
			w.l.Engine.Pass(ctx, w.org.ID)
			close(firstResponse)
			select {
			case err := <-initialError:
				if connect.IsWireError(err) || connect.CodeOf(err) != connect.CodeDeadlineExceeded {
					t.Fatalf("first create error = %v, want a non-wire deadline error", err)
				}
			default:
				t.Fatal("dispatcher did not attempt a create")
			}
			runners := w.runnersIn(t, tidev1.RunnerState_RUNNER_STATE_CREATING)
			if len(runners) != 1 || runners[0].GetRequestId() != "" || runners[0].GetOwnsandSandboxId() != "" {
				t.Fatalf("expected one unbound create reservation: %v", runners)
			}
			runnerID := runners[0].GetRunnerId()

			w.send(t, "workflow_job", job("queued", installation, other, tideLinux, ""))
			switch scenario {
			case "canceled", "removed_sponsor":
				w.send(t, "workflow_job", job("completed", installation, sponsor, tideLinux, ""))
				canceled := w.requestOfJob(t, sponsor)
				if canceled.GetState() != tidev1.RequestState_REQUEST_STATE_CANCELED ||
					canceled.GetEndReason() != tidev1.RequestEndReason_REQUEST_END_REASON_CANCELED_BY_SOURCE {
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
					int16(tidev1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK)); err != nil {
					t.Fatal(err)
				}
				w.send(t, "workflow_job", job("in_progress", installation, sponsor, tideLinux, "tide-"+waitingID))
				if started := w.requestOfJob(t, sponsor); started.GetState() != tidev1.RequestState_REQUEST_STATE_RUNNING ||
					started.GetRunnerId() != waitingID {
					t.Fatalf("job after in_progress webhook: %v", started)
				}
			case "legacy":
				if _, err := w.env.DB.Pool.Exec(ctx, "UPDATE runners SET sponsor_request_id = NULL WHERE runner_id = $1", runnerID); err != nil {
					t.Fatal(err)
				}
			}
			before, otherBefore := w.requestOfJob(t, sponsor), w.requestOfJob(t, other)
			if otherBefore.GetState() != tidev1.RequestState_REQUEST_STATE_QUEUED {
				t.Fatalf("other job before retry: %v", otherBefore)
			}
			if scenario == "queued" || scenario == "legacy" {
				if before.GetState() != tidev1.RequestState_REQUEST_STATE_QUEUED {
					t.Fatalf("sponsor before retry: %v", before)
				}
			}
			w.l.GitHub.Wait()
			w.env.Stop()
			var recovered layers.Layers
			w.env = w.env.NewReplica(t, testenv.WithLayerOptions(layers.Options{OwnSand: factory, GitHub: w.githubOptions(w.env.Clock.Now)}, &recovered))
			// Run only reconciliation on the new instance. An ordinary
			// dispatch pass could obscure which create ended the job.
			reconcileCtx, stop := context.WithCancel(ctx)
			done := make(chan struct{})
			go func() {
				defer close(done)
				_ = recovered.Engine.RunReconciler(reconcileCtx)
			}()
			t.Cleanup(func() { stop(); <-done })

			eventually(t, "the original runner records its rejected create", func() bool {
				return w.runner(t, runnerID).GetState() == tidev1.RunnerState_RUNNER_STATE_FAILED
			})
			runner := w.runner(t, runnerID)
			if runner.GetRequestId() != "" || runner.GetOwnsandSandboxId() != "" ||
				runner.GetEndReason() != tidev1.RunnerEndReason_RUNNER_END_REASON_START_FAILED {
				t.Fatalf("runner after retry: %v", runner)
			}
			after := w.requestOfJob(t, sponsor)
			if scenario == "queued" {
				if after.GetState() != tidev1.RequestState_REQUEST_STATE_FAILED ||
					after.GetEndReason() != tidev1.RequestEndReason_REQUEST_END_REASON_NO_ELIGIBLE_DEVICE ||
					after.GetStartAttempts() != 0 || after.GetRunnerId() != "" {
					t.Fatalf("sponsoring job after retry: %v", after)
				}
			} else if !proto.Equal(after, before) {
				t.Fatalf("retry changed the job: before %v, after %v", before, after)
			}
			if got := w.requestOfJob(t, other); !proto.Equal(got, otherBefore) {
				t.Fatalf("retry changed the other queued job: %v", got)
			}
			if got := calls.Load(); got != 2 || len(requests) != 2 {
				t.Fatalf("expected the initial create and one retry, got %d calls", got)
			}
			first, second := <-requests, <-requests
			if first.GetIdempotencyKey() == "" || !proto.Equal(first, second) {
				t.Fatal("the retry changed the OwnSand create parameters or idempotency key")
			}
			t.Logf("OwnSand received identical creates before and after restart. Recovered runner recorded START_FAILED. Sponsor state: %s. Other job stayed QUEUED.", after.GetState())
		})
	}
}
