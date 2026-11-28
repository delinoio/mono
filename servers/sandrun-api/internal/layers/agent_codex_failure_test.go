// SPDX-License-Identifier: FSL-1.1-ALv2

package layers_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/servers/sandrun-api/internal/agent/codex"
	"github.com/delinoio/mono/servers/sandrun-api/internal/agent/codex/codexfake"
	"github.com/delinoio/mono/servers/sandrun-api/internal/app"
	"github.com/delinoio/mono/servers/sandrun-api/internal/db"
	"github.com/delinoio/mono/servers/sandrun-api/internal/layers"
	"github.com/delinoio/mono/servers/sandrun-api/internal/lease"
	"github.com/delinoio/mono/servers/sandrun-api/internal/testenv"
	"github.com/delinoio/mono/servers/sandrun-api/internal/webhook"
	"github.com/delinoio/mono/servers/sandrun-api/internal/work"
)

func TestCodexSessionFailedReleasesComputeAfterAcknowledgment(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		h2c     bool
		restart bool
	}{
		{name: "HTTP_1.1_retry"},
		{name: "h2c_lease_takeover", h2c: true, restart: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newCodexWebhookWorld(t)
			const sessionID, deliveryID = "sess_ack", "wh_failed_ack"
			w.openai.AddSession(sessionID, "ccarenv_ack")
			if code := w.deliver(t, "wh_connect", codexfake.ActionRequired(sessionID)); code != http.StatusOK {
				t.Fatal(code)
			}
			requestID := w.requestBySource(t, sessionID+":ccarenv_ack").GetRequestId()
			var runnerID string
			eventually(t, "bound runner with a sandbox", func() bool {
				request := w.request(t, requestID)
				runnerID = request.GetRunnerId()
				if request.GetState() != sandrunv1.RequestState_REQUEST_STATE_STARTING || runnerID == "" {
					return false
				}
				runner := w.runner(t, runnerID)
				return runner.GetState() == sandrunv1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK && runner.GetOwnsandSandboxId() != ""
			})

			ctx := t.Context()
			observer, err := pgx.Connect(ctx, w.env.DB.Pool.Config().ConnString())
			if err != nil {
				t.Fatal(err)
			}
			defer observer.Close(context.Background())
			var attempts atomic.Int32
			allowDelete := make(chan struct{})
			release := sync.OnceFunc(func() { close(allowDelete) })
			defer release()
			w.fake.SetDeleteHook(func(ctx context.Context) error {
				attempt := attempts.Add(1)
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-allowDelete:
					if attempt == 1 {
						return connect.NewError(connect.CodeUnavailable, errors.New("temporary OwnSand failure"))
					}
					return nil
				}
			})

			var protocols http.Protocols
			protocols.SetHTTP1(!tc.h2c)
			protocols.SetUnencryptedHTTP2(tc.h2c)
			server := httptest.NewUnstartedServer(w.env.App.Handler())
			server.Config.Protocols = &protocols
			server.Start()
			t.Cleanup(server.Close)
			client := &http.Client{Transport: &http.Transport{Protocols: &protocols}, Timeout: 5 * time.Second}
			t.Cleanup(client.CloseIdleConnections)
			w.openai.Update(sessionID, func(s *codexfake.Session) { s.Status, s.NeedsConnection = "failed", false })
			body := codexfake.Event("agent.session.failed", sessionID)
			key, err := codex.SecretKey(codexWebhookSecret)
			if err != nil {
				t.Fatal(err)
			}
			timestamp := strconv.FormatInt(w.env.Clock.Now().Unix(), 10)
			signature := "v1," + codex.Sign(key, deliveryID, timestamp, body)
			deliver := func() {
				t.Helper()
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/webhooks/openai/"+w.integrationID, bytes.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("webhook-id", deliveryID)
				req.Header.Set("webhook-timestamp", timestamp)
				req.Header.Set("webhook-signature", signature)
				resp, err := client.Do(req)
				if err != nil {
					t.Fatalf("acknowledge failure before OwnSand deletion: %v", err)
				}
				defer resp.Body.Close()
				gotBody, err := io.ReadAll(resp.Body)
				wantProtocol := "HTTP/1.1"
				if tc.h2c {
					wantProtocol = "HTTP/2.0"
				}
				if err != nil || resp.StatusCode != http.StatusOK || resp.Proto != wantProtocol || len(gotBody) != 0 {
					t.Fatalf("acknowledgment = %s %d, body %q, read error %v", resp.Proto, resp.StatusCode, gotBody, err)
				}
				t.Logf("read complete empty %s 200 with the OwnSand delete gate closed", resp.Proto)
			}
			assertPending := func() [2]int64 {
				t.Helper()
				if seen, err := webhook.Seen(ctx, observer, webhook.SenderOpenAI, deliveryID); err != nil || !seen {
					t.Fatalf("committed delivery: seen=%v, err=%v", seen, err)
				}
				request, err := w.l.Engine.GetRequest(ctx, observer, w.org.ID, requestID)
				if err != nil {
					t.Fatal(err)
				}
				runner, err := w.l.Engine.GetRunner(ctx, observer, w.org.ID, runnerID)
				if err != nil {
					t.Fatal(err)
				}
				if request.State != sandrunv1.RequestState_REQUEST_STATE_FAILED ||
					request.EndReason != sandrunv1.RequestEndReason_REQUEST_END_REASON_CANCELED_BY_SOURCE ||
					runner.State != sandrunv1.RunnerState_RUNNER_STATE_DELETING ||
					runner.EndReason != sandrunv1.RunnerEndReason_RUNNER_END_REASON_CLEAN_UP || w.fake.Active(w.osOrg) != 1 {
					t.Fatalf("pending cleanup: request=%+v runner=%+v active=%d", request, runner, w.fake.Active(w.osOrg))
				}
				return [2]int64{request.ChangeSeq, runner.ChangeSeq}
			}

			deliver()
			committed := assertPending()
			eventually(t, "blocked OwnSand deletion attempt", func() bool { return attempts.Load() == 1 })
			reads := w.openai.Reads()
			deliver()
			if w.openai.Reads() != reads || assertPending() != committed {
				t.Fatal("duplicate delivery repeated the session lookup or changed the request or runner")
			}
			t.Log("separate connection observed all three committed records and unchanged states after replay")

			if tc.restart {
				w.env.Stop()
				assertPending()
				replica := w.env.NewReplica(t, testenv.WithLayers(w.fake, &layers.Layers{}))
				eventually(t, "replacement lease holder attempting pending cleanup", func() bool {
					return replica.App.Lease.Held() && attempts.Load() >= 2
				})
				t.Log("replacement lease holder picked up pending deletion before OwnSand recovered")
			}
			release()
			eventually(t, "sandbox deletion without a new delivery", func() bool {
				runner, err := w.l.Engine.GetRunner(ctx, observer, w.org.ID, runnerID)
				if err != nil {
					t.Fatal(err)
				}
				return runner.State == sandrunv1.RunnerState_RUNNER_STATE_DELETED &&
					runner.EndReason == sandrunv1.RunnerEndReason_RUNNER_END_REASON_CLEAN_UP &&
					w.fake.Active(w.osOrg) == 0 && attempts.Load() >= 2
			})
			t.Logf("cleanup completed in %d OwnSand attempts", attempts.Load())
		})
	}
}

type codexFailureGate struct {
	requestID       atomic.Pointer[string]
	query           string
	afterRead       bool
	reached, resume chan struct{}
}

type codexFailureGateKey struct{}

func (g *codexFailureGate) TraceQueryStart(ctx context.Context, _ *pgx.Conn, q pgx.TraceQueryStartData) context.Context {
	id := g.requestID.Load()
	if id == nil || q.SQL != g.query || q.Args[len(q.Args)-1] != *id || !g.requestID.CompareAndSwap(id, nil) {
		return ctx
	}
	if g.afterRead {
		return context.WithValue(ctx, codexFailureGateKey{}, true)
	}
	g.pause(ctx)
	return ctx
}

func (g *codexFailureGate) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, q pgx.TraceQueryEndData) {
	if q.Err == nil && ctx.Value(codexFailureGateKey{}) != nil {
		g.pause(ctx)
	}
}

func (g *codexFailureGate) pause(ctx context.Context) {
	close(g.reached)
	select {
	case <-g.resume:
	case <-ctx.Done():
	}
}

func TestCodexSessionFailedReleasesComputeDuringDispatch(t *testing.T) {
	t.Parallel()
	for _, dispatchFirst := range []bool{true, false} {
		name := "failure first"
		query := "SELECT " + work.RequestColumns + " FROM requests WHERE request_id = $1 FOR UPDATE"
		if dispatchFirst {
			name = "dispatch first"
			query = "SELECT " + work.RequestColumns + " FROM requests WHERE organization_id = $1 AND request_id = $2"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			gate := &codexFailureGate{query: query, afterRead: dispatchFirst, reached: make(chan struct{}), resume: make(chan struct{})}
			resume := sync.OnceFunc(func() { close(gate.resume) })
			defer resume()
			w := newCodexWorld(t, sandrunv1.GuestOs_GUEST_OS_LINUX, func(o *app.Options) {
				cfg := o.DB.Pool.Config()
				cfg.ConnConfig.Tracer = gate
				pool, err := pgxpool.NewWithConfig(ctx, cfg)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(pool.Close)
				o.DB = &db.DB{Pool: pool}
			})
			_, err := w.env.User(w.org.Admin).Profile.UpdateRunnerProfile(ctx, connect.NewRequest(&sandrunv1.UpdateRunnerProfileRequest{
				OrganizationId: w.org.ID, RunnerProfileId: w.profileID, Profile: &sandrunv1.RunnerProfile{WarmCount: 1},
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"warm_count"}},
			}))
			if err != nil {
				t.Fatal(err)
			}
			var warm *sandrunv1.Runner
			eventually(t, "warm Codex runner", func() bool {
				resp, err := w.env.User(w.org.Owner).Work.ListRunners(ctx, connect.NewRequest(&sandrunv1.ListRunnersRequest{
					OrganizationId: w.org.ID, States: []sandrunv1.RunnerState{sandrunv1.RunnerState_RUNNER_STATE_WARM},
				}))
				if err != nil {
					t.Fatal(err)
				}
				if runners := resp.Msg.GetRunners(); len(runners) == 1 {
					warm = runners[0]
				}
				return warm != nil
			})
			runnerID := warm.GetRunnerId()

			// Serialize dispatcher passes so neither background dispatch nor
			// warm replenishment can change the fixture during the delivery.
			w.env.App.Lease.Release()
			parked := lease.Lease{DB: w.env.DB, InstanceID: "parked"}
			if held, err := parked.TryAcquire(ctx); err != nil || !held {
				t.Fatalf("park lease: held=%v, err=%v", held, err)
			}
			defer parked.Release()

			const sid = "sess_dispatch"
			w.openai.AddSession(sid, "ccarenv_dispatch")
			if code := w.deliver(t, "wh_connect", codexfake.ActionRequired(sid)); code != http.StatusOK {
				t.Fatal(code)
			}
			id := w.requestBySource(t, sid+":ccarenv_dispatch").GetRequestId()
			w.openai.Update(sid, func(s *codexfake.Session) { s.Status, s.NeedsConnection = "failed", false })
			gate.requestID.Store(&id)
			body := codexfake.Event("agent.session.failed", sid)
			if dispatchFirst {
				delivered := make(chan int, 1)
				go func() { delivered <- w.deliver(t, "wh_failed", body) }()
				select {
				case <-gate.reached:
				case code := <-delivered:
					t.Fatalf("delivery returned %d before reading the queued request", code)
				case <-ctx.Done():
					t.Fatal("delivery did not read the queued request")
				}
				w.l.Engine.Pass(ctx, w.org.ID)
				runCtx, stop := context.WithCancel(ctx)
				reconciled := make(chan struct{})
				go func() {
					defer close(reconciled)
					_ = w.l.Engine.RunReconciler(runCtx)
				}()
				defer func() { stop(); <-reconciled }()
				w.waitRunner(t, runnerID, sandrunv1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK)
				resume()
				select {
				case code := <-delivered:
					if code != http.StatusOK {
						t.Fatal(code)
					}
				case <-ctx.Done():
					t.Fatal("failure delivery did not finish")
				}
				req := w.request(t, id)
				if req.GetState() != sandrunv1.RequestState_REQUEST_STATE_FAILED || req.GetRunnerId() != runnerID ||
					req.GetEndReason() != sandrunv1.RequestEndReason_REQUEST_END_REASON_CANCELED_BY_SOURCE {
					t.Fatalf("request after dispatch won: %v", req)
				}
				r := w.waitRunner(t, runnerID, sandrunv1.RunnerState_RUNNER_STATE_DELETED)
				if r.GetEndReason() != sandrunv1.RunnerEndReason_RUNNER_END_REASON_CLEAN_UP || w.fake.Active(w.osOrg) != 0 {
					t.Fatalf("runner after failure: %v, active sandboxes: %d", r, w.fake.Active(w.osOrg))
				}
				return
			}

			dispatched := make(chan struct{})
			go func() {
				defer close(dispatched)
				w.l.Engine.Pass(ctx, w.org.ID)
			}()
			select {
			case <-gate.reached:
			case <-dispatched:
				t.Fatal("dispatch ended before trying to lock the request")
			case <-ctx.Done():
				t.Fatal("dispatch did not reach the request lock")
			}
			if code := w.deliver(t, "wh_failed", body); code != http.StatusOK {
				t.Fatal(code)
			}
			resume()
			<-dispatched
			req, runner := w.request(t, id), w.runner(t, runnerID)
			if req.GetState() != sandrunv1.RequestState_REQUEST_STATE_CANCELED || req.GetRunnerId() != "" ||
				req.GetEndReason() != sandrunv1.RequestEndReason_REQUEST_END_REASON_CANCELED_BY_SOURCE {
				t.Fatalf("request after failure won: %v", req)
			}
			if runner.GetState() != sandrunv1.RunnerState_RUNNER_STATE_WARM || runner.GetRequestId() != "" ||
				w.fake.Active(w.osOrg) != 1 || w.fake.Calls("DeleteSandbox") != 0 {
				t.Fatalf("dispatch modified the warm runner: %v", runner)
			}
		})
	}
}

func TestCodexSessionFailedReleasesComputeAfterRedelivery(t *testing.T) {
	t.Parallel()
	w := newCodexWorld(t, sandrunv1.GuestOs_GUEST_OS_LINUX)
	ctx := t.Context()
	const sid = "sess_rollback"
	w.openai.AddSession(sid, "ccarenv_rollback")
	if code := w.deliver(t, "wh_connect", codexfake.ActionRequired(sid)); code != http.StatusOK {
		t.Fatal(code)
	}
	id := w.requestBySource(t, sid+":ccarenv_rollback").GetRequestId()
	var runnerID string
	eventually(t, "runner waits for work", func() bool {
		runnerID = w.request(t, id).GetRunnerId()
		return runnerID != "" && w.runner(t, runnerID).GetState() == sandrunv1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK
	})
	w.openai.Update(sid, func(s *codexfake.Session) { s.Status, s.NeedsConnection = "failed", false })
	_, err := w.env.DB.Pool.Exec(ctx, fmt.Sprintf(`
		CREATE FUNCTION fail_codex_deletion() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			RAISE EXCEPTION 'test deletion failure';
		END $$;
		CREATE TRIGGER fail_codex_deletion BEFORE UPDATE OF state ON runners
		FOR EACH ROW WHEN (NEW.state = %d) EXECUTE FUNCTION fail_codex_deletion();`,
		sandrunv1.RunnerState_RUNNER_STATE_DELETING))
	if err != nil {
		t.Fatal(err)
	}
	const deliveryID = "wh_failed"
	body := codexfake.Event("agent.session.failed", sid)
	if code := w.deliver(t, deliveryID, body); code != http.StatusInternalServerError {
		t.Fatal(code)
	}
	if seen, err := webhook.Seen(ctx, w.env.DB.Pool, webhook.SenderOpenAI, deliveryID); err != nil || seen {
		t.Fatalf("rolled-back delivery: seen=%v, err=%v", seen, err)
	}
	req, runner := w.request(t, id), w.runner(t, runnerID)
	if req.GetState() != sandrunv1.RequestState_REQUEST_STATE_STARTING || runner.GetState() != sandrunv1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK ||
		w.fake.Calls("DeleteSandbox") != 0 {
		t.Fatalf("failed transaction changed request or runner: request=%v runner=%v", req, runner)
	}
	t.Log("runner transition failed with HTTP 500 and rolled back the delivery, request and runner changes")

	if _, err := w.env.DB.Pool.Exec(ctx, "DROP TRIGGER fail_codex_deletion ON runners"); err != nil {
		t.Fatal(err)
	}
	if code := w.deliver(t, deliveryID, body); code != http.StatusOK {
		t.Fatal(code)
	}
	if seen, err := webhook.Seen(ctx, w.env.DB.Pool, webhook.SenderOpenAI, deliveryID); err != nil || !seen {
		t.Fatalf("committed delivery: seen=%v, err=%v", seen, err)
	}
	req = w.waitRequest(t, id, sandrunv1.RequestState_REQUEST_STATE_FAILED)
	runner = w.waitRunner(t, runnerID, sandrunv1.RunnerState_RUNNER_STATE_DELETED)
	if req.GetEndReason() != sandrunv1.RequestEndReason_REQUEST_END_REASON_CANCELED_BY_SOURCE ||
		runner.GetEndReason() != sandrunv1.RunnerEndReason_RUNNER_END_REASON_CLEAN_UP || w.fake.Active(w.osOrg) != 0 {
		t.Fatalf("redelivery did not release compute: request=%v runner=%v", req, runner)
	}
	reads := w.openai.Reads()
	if code := w.deliver(t, deliveryID, body); code != http.StatusOK || w.openai.Reads() != reads {
		t.Fatalf("duplicate delivery: status=%d, reads=%d -> %d", code, reads, w.openai.Reads())
	}
	if code := w.deliver(t, "wh_terminal", body); code != http.StatusOK {
		t.Fatal(code)
	}
	if w.request(t, id).GetState() != sandrunv1.RequestState_REQUEST_STATE_FAILED {
		t.Fatal("delivery for the terminal request changed its outcome")
	}
	if got := strings.Count(w.env.Logs.String(), `"state":"DELETING"`); got != 1 {
		t.Fatalf("runner deletion was scheduled %d times, want 1", got)
	}
}
