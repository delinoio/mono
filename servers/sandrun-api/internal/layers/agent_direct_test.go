// SPDX-License-Identifier: FSL-1.1-ALv2

package layers_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"connectrpc.com/connect"

	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/servers/sandrun-api/internal/agent/claude/claudefake"
	"github.com/delinoio/mono/servers/sandrun-api/internal/agent/codex/codexfake"
	"github.com/delinoio/mono/servers/sandrun-api/internal/ownsandclient/ownsandfake"
	"github.com/delinoio/mono/servers/sandrun-api/internal/testenv"
	"github.com/delinoio/mono/servers/sandrun-api/internal/webhook"
)

const concurrentDemandID = "sess_concurrent"

// holdDirectAdmission pauses the first transaction at commit, after it has
// written the request, claim and any work order. A Claude hook can also ask
// the fixture to fail the commit so another hook must create the request.
func holdDirectAdmission(t *testing.T, env *testenv.Env) func() {
	t.Helper()
	_, err := env.DB.Pool.Exec(t.Context(), `
		CREATE FUNCTION pause_direct_admission() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.vendor_request_id = TG_ARGV[0] THEN
				PERFORM pg_advisory_xact_lock(42);
				IF NEW.demand_marker = 'rollback' THEN
					RAISE EXCEPTION 'fixture failed direct admission at commit';
				END IF;
			END IF;
			RETURN NULL;
		END;
		$$;
		CREATE CONSTRAINT TRIGGER pause_direct_admission
			AFTER INSERT ON agent_claims DEFERRABLE INITIALLY DEFERRED
			FOR EACH ROW EXECUTE FUNCTION pause_direct_admission('sess_concurrent');
	`)
	if err != nil {
		t.Fatal(err)
	}
	return holdTx(t, env, "SELECT pg_advisory_xact_lock(42)").commit
}

func waitForDirectAdmission(t *testing.T, env *testenv.Env, query string) {
	t.Helper()
	eventually(t, "an advisory lock wait for "+query, func() bool {
		var waiting bool
		err := env.DB.Pool.QueryRow(t.Context(), `SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity WHERE datname = current_database()
			AND wait_event_type = 'Lock' AND wait_event = 'advisory' AND query LIKE $1
		)`, query).Scan(&waiting)
		if err != nil {
			t.Fatal(err)
		}
		return waiting
	})
}

func requireDirectAdmissionRows(t *testing.T, w *agentWorld, requests, workOrders int) {
	t.Helper()
	var nr, nc, nw int
	if err := w.env.DB.Pool.QueryRow(t.Context(), `SELECT count(*), count(c.request_id), count(r.work_order_ciphertext)
		FROM requests r LEFT JOIN agent_claims c USING (request_id) WHERE r.organization_id = $1`,
		w.org.ID).Scan(&nr, &nc, &nw); err != nil {
		t.Fatal(err)
	}
	if nr != requests || nc != requests || nw != workOrders {
		t.Fatalf("requests = %d, claims = %d, stored orders = %d, want %d, %d, %d", nr, nc, nw, requests, requests, workOrders)
	}
}

func TestClaudeDirectAdmission(t *testing.T) {
	for _, name := range []string{"commit", "rollback"} {
		t.Run(name, func(t *testing.T) {
			w := newClaudeWorld(t, sandrunv1.GuestOs_GUEST_OS_LINUX, 0)
			w.full()
			proc := w.proc(t)
			resume := holdDirectAdmission(t, w.env)
			type result struct {
				code    int
				message string
			}
			spawn := func(orderID, reason string) <-chan result {
				done := make(chan result, 1)
				go func() {
					defer close(done)
					code, message := proc.Spawn(claudefake.Spawn{
						OrderID: orderID, SessionID: "session_concurrent", Reason: reason, WorkOrder: claudeWorkOrder,
					})
					done <- result{code: code, message: message}
				}()
				return done
			}
			first := spawn(concurrentDemandID, name)
			waitForDirectAdmission(t, w.env, "commit")
			second := spawn(concurrentDemandID, "redelivery")
			waitForDirectAdmission(t, w.env, "SELECT pg_advisory_xact_lock(%")

			// A different order in the same organization and profile can
			// finish while both calls for the shared order are waiting.
			other := spawn("other_order", "session_created")
			select {
			case got := <-other:
				if got.code != 0 || !strings.Contains(got.message, "queued") {
					t.Fatalf("unrelated hook = %+v, want a queued request", got)
				}
			case <-first:
				t.Fatal("the first hook finished before the unrelated hook")
			case <-second:
				t.Fatal("the second hook finished before the unrelated hook")
			}
			requireDirectAdmissionRows(t, w.agentWorld, 1, 1)

			resume()
			a, b := <-first, <-second
			if name == "rollback" {
				if a.code != 1 || b.code != 0 || !strings.Contains(b.message, "queued") {
					t.Fatalf("hook results = %+v and %+v, want retry and queued", a, b)
				}
			} else if a.code != 0 || !strings.Contains(a.message, "queued") || b.code != 0 || !strings.Contains(b.message, "already") {
				t.Fatalf("hook results = %+v and %+v, want queued and duplicate", a, b)
			}
			requireDirectAdmissionRows(t, w.agentWorld, 2, 2)
			req := w.requestBySource(t, concurrentDemandID)
			// The stored and duplicate results must both name the same request.
			want := 2
			if name == "rollback" {
				want = 1
			}
			got := 0
			for line := range strings.SplitSeq(w.env.Logs.String(), "\n") {
				if strings.Contains(line, `"msg":"claude_spawn_request"`) &&
					strings.Contains(line, `"request_id":"`+req.GetRequestId()+`"`) {
					got++
				}
			}
			if got != want {
				t.Fatalf("hook results naming request %s = %d, want %d", req.GetRequestId(), got, want)
			}
		})
	}
}

func TestCodexDirectAdmission(t *testing.T) {
	w := newCodexWorld(t, sandrunv1.GuestOs_GUEST_OS_LINUX)
	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) {
		o.CreateError = connect.NewWireError(connect.CodeUnavailable, errors.New("full"))
	})
	const envID = "ccarenv_concurrent"
	w.openai.AddSession(concurrentDemandID, envID)
	body := codexfake.ActionRequired(concurrentDemandID)
	resume := holdDirectAdmission(t, w.env)
	deliver := func(id string) <-chan int {
		done := make(chan int, 1)
		go func() {
			defer close(done)
			done <- w.deliver(t, id, body)
		}()
		return done
	}
	first := deliver("wh_concurrent_1")
	waitForDirectAdmission(t, w.env, "commit")
	second := deliver("wh_concurrent_2")
	waitForDirectAdmission(t, w.env, "SELECT pg_advisory_xact_lock(%")
	requireDirectAdmissionRows(t, w.agentWorld, 0, 0)
	resume()
	a, b := <-first, <-second
	if a != http.StatusOK || b != http.StatusOK {
		t.Fatalf("delivery status = %d and %d, want 200 for both", a, b)
	}
	for _, id := range []string{"wh_concurrent_1", "wh_concurrent_2"} {
		seen, err := webhook.Seen(t.Context(), w.env.DB.Pool, webhook.SenderOpenAI, id)
		if err != nil || !seen {
			t.Fatalf("delivery %s recorded = %t, error = %v", id, seen, err)
		}
	}
	requireDirectAdmissionRows(t, w.agentWorld, 1, 0)

	const baseKey = concurrentDemandID + ":" + envID
	req := w.requestBySource(t, baseKey)
	w.cancel(t, req.GetRequestId())
	w.waitRequest(t, req.GetRequestId(), sandrunv1.RequestState_REQUEST_STATE_CANCELED)
	if code := w.deliver(t, "wh_concurrent_3", body); code != http.StatusOK {
		t.Fatalf("follow-up status = %d, want 200", code)
	}
	w.requestBySource(t, baseKey+"#2")
	requireDirectAdmissionRows(t, w.agentWorld, 2, 0)
}
