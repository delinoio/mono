// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

func TestCreatePlacementPendingIdle(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		advance   time.Duration
		explicit  bool
		fallback  bool
		wantDrain bool
	}{
		{name: "explicit before cutoff", explicit: true},
		{name: "explicit at cutoff", advance: time.Millisecond, explicit: true},
		{name: "explicit after cutoff", advance: 2 * time.Millisecond, explicit: true, wantDrain: true},
		{name: "automatic without fallback", advance: 2 * time.Millisecond, wantDrain: true},
		{name: "automatic with busier fallback", advance: 2 * time.Millisecond, fallback: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := testenv.New(t)
			org := env.NewOrg("alice")
			device, agent := setupAgent(t, env, org, nil)
			client := org.OwnerClient.Sandbox
			ctx := t.Context()

			chosen, chosenAgent := device, agent
			if tc.fallback {
				// Give the fallback a real reservation so the idle device
				// would win automatic placement before the cutoff.
				chosen, chosenAgent = setupAgent(t, env, org, nil)
				preload := createReq(org.ID)
				preload.Placement = &ownsandv1.Placement{DeviceId: &chosen.ID}
				if _, err := client.CreateSandbox(ctx, connect.NewRequest(preload)); err != nil {
					t.Fatal(err)
				}
			}

			requestTime := env.Clock.Now()
			const pendingIdle = ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_PENDING_IDLE
			if _, err := env.DB.Pool.Exec(ctx, `UPDATE devices SET update_state = $2, pending_idle_since = $3
				WHERE device_id = $1`, device.ID, int16(pendingIdle), requestTime.Add(-24*time.Hour+time.Millisecond)); err != nil {
				t.Fatal(err)
			}
			lock := holdPlacementDevice(t, env, device.ID)
			req := createReq(org.ID)
			if tc.explicit {
				req.Placement = &ownsandv1.Placement{DeviceId: &device.ID}
			}

			type result struct {
				sandbox *ownsandv1.Sandbox
				err     error
			}
			done := make(chan result, 1)
			go func() {
				resp, err := client.CreateSandbox(ctx, connect.NewRequest(req))
				if err != nil {
					done <- result{err: err}
					return
				}
				done <- result{sandbox: resp.Msg.GetSandbox()}
			}()
			waitForLockWaiter(t, env, "SELECT 1 FROM devices WHERE organization_id", "FOR UPDATE")
			env.Clock.Advance(tc.advance)
			if err := lock.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			got := <-done

			if tc.wantDrain {
				var rpcErr *connect.Error
				if !errors.As(got.err, &rpcErr) || rpcErr.Code() != connect.CodeUnavailable || !strings.Contains(rpcErr.Message(), "the device is draining for an agent update") {
					t.Fatalf("create after cutoff: got %v, want unavailable for draining", got.err)
				}
				var reservations, keys, audits int
				if err := env.DB.Pool.QueryRow(ctx, `SELECT
					(SELECT count(*) FROM sandboxes WHERE organization_id = $1),
					(SELECT count(*) FROM sandbox_create_keys WHERE organization_id = $1),
					(SELECT count(*) FROM audit_events WHERE organization_id = $1 AND action = $2)`,
					org.ID, int16(ownsandv1.AuditAction_AUDIT_ACTION_SANDBOX_CREATED)).Scan(&reservations, &keys, &audits); err != nil {
					t.Fatal(err)
				}
				if reservations != 0 || keys != 0 || audits != 0 {
					t.Fatalf("rejected create wrote %d reservations, %d keys, %d audits", reservations, keys, audits)
				}
				// Read through a heartbeat acknowledgement on the same channel.
				agent.Heartbeat(1)
				agent.WaitFor("heartbeat ack", func(e *ownsandv1.AgentEnvelope) bool { return e.GetHeartbeatAck().GetSequence() == 1 })
				for _, cmd := range agent.Commands() {
					if cmd.GetCreateSandbox() != nil {
						t.Fatal("rejected create sent a create command")
					}
				}
				return
			}

			if got.err != nil || got.sandbox.GetDeviceId() != chosen.ID {
				t.Fatalf("create: got device %s, err %v, want device %s", got.sandbox.GetDeviceId(), got.err, chosen.ID)
			}
			chosenAgent.WaitFor("create command", func(e *ownsandv1.AgentEnvelope) bool {
				return e.GetCommand().GetCreateSandbox().GetSandboxId() == got.sandbox.GetSandboxId()
			})
			if !got.sandbox.GetCreateTime().AsTime().Equal(requestTime) ||
				!got.sandbox.GetIdleDeadline().AsTime().Equal(requestTime.Add(req.GetIdleTtl().AsDuration())) ||
				!got.sandbox.GetMaxDeadline().AsTime().Equal(requestTime.Add(req.GetMaxTtl().AsDuration())) {
				t.Fatalf("create time and deadlines do not use request time %s: got %s, %s, %s", requestTime,
					got.sandbox.GetCreateTime().AsTime(), got.sandbox.GetIdleDeadline().AsTime(), got.sandbox.GetMaxDeadline().AsTime())
			}
			var expiry time.Time
			if err := env.DB.Pool.QueryRow(ctx, "SELECT expire_time FROM sandbox_create_keys WHERE organization_id = $1 AND idempotency_key = $2",
				org.ID, req.GetIdempotencyKey()).Scan(&expiry); err != nil {
				t.Fatal(err)
			}
			if want := requestTime.Add(24 * time.Hour); !expiry.Equal(want) {
				t.Fatalf("key expiry: got %s, want %s", expiry, want)
			}
		})
	}
}
