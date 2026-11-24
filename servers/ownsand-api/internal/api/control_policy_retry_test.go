// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/app"
	"github.com/delinoio/mono/servers/ownsand-api/internal/db"
	"github.com/delinoio/mono/servers/ownsand-api/internal/events"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

type agentPolicyLookupFault struct {
	deviceID string
	failed   atomic.Bool
	resume   chan struct{}
}

type agentPolicyLookupTracer struct {
	fault atomic.Pointer[agentPolicyLookupFault]
}

func (tr *agentPolicyLookupTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, query pgx.TraceQueryStartData) context.Context {
	fault := tr.fault.Load()
	if fault == nil || !strings.HasPrefix(query.SQL, "SELECT organization_id::text, agent_version, host_os, arch FROM devices") ||
		query.Args[0] != fault.deviceID {
		return ctx
	}
	if !fault.failed.Swap(true) {
		// Fail only the query context. The event worker stays running.
		queryCtx, cancel := context.WithCancel(ctx)
		cancel()
		return queryCtx
	}
	// Pause the retry until the test has observed the failed delivery and
	// made any intervening changes to the channel or policy.
	select {
	case <-fault.resume:
	case <-ctx.Done():
	}
	return ctx
}

func (*agentPolicyLookupTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestControlRetryAgentUpdatePolicy(t *testing.T) {
	const (
		hold = ownsandv1.AgentVersionPolicyMode_AGENT_VERSION_POLICY_MODE_HOLD
		pin  = ownsandv1.AgentVersionPolicyMode_AGENT_VERSION_POLICY_MODE_PIN
	)
	isWithdrawal := func(e *ownsandv1.AgentEnvelope) bool { return e.GetCommand().GetWithdrawAgentUpdate() != nil }
	heartbeat := func(agent *testenv.FakeAgent, seq uint64) {
		agent.Heartbeat(seq)
		agent.WaitFor("heartbeat acknowledgement", func(e *ownsandv1.AgentEnvelope) bool {
			return e.GetHeartbeatAck().GetSequence() == seq
		})
	}
	decisions := func(agent *testenv.FakeAgent) []string {
		var commands []string
		for _, cmd := range agent.Commands() {
			switch {
			case cmd.GetWithdrawAgentUpdate() != nil:
				commands = append(commands, "withdraw")
			case cmd.GetUpdateAgent().GetRevocationRecovery():
				commands = append(commands, "recover "+cmd.GetUpdateAgent().GetTargetVersion())
			case cmd.GetUpdateAgent() != nil:
				commands = append(commands, cmd.GetUpdateAgent().GetTargetVersion())
			}
		}
		return commands
	}

	for _, scenario := range []struct {
		name        string
		healthy     []string
		affected    []string
		replacement []string
	}{
		{name: "same channel", healthy: []string{"1.1.0", "withdraw"}, affected: []string{"1.1.0", "withdraw"}},
		{name: "latest policy", healthy: []string{"1.1.0", "withdraw", "1.2.0"}, affected: []string{"1.1.0", "1.2.0"}},
		{name: "revoked version", healthy: []string{"1.1.0", "withdraw", "recover 1.1.0"}, affected: []string{"1.1.0", "recover 1.1.0"}},
		{name: "closed channel", healthy: []string{"1.1.0", "withdraw"}, affected: []string{"1.1.0"}},
		{name: "replaced channel", healthy: []string{"1.1.0", "withdraw"}, affected: []string{"1.1.0"}, replacement: []string{"withdraw"}},
		{name: "missing device", healthy: []string{"1.1.0", "withdraw"}, affected: []string{"1.1.0"}},
		{name: "removed device", healthy: []string{"1.1.0", "withdraw"}, affected: []string{"1.1.0"}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx := context.Background()
			tracer := &agentPolicyLookupTracer{}
			env := testenv.New(t, func(o *app.Options) {
				cfg := o.DB.Pool.Config()
				cfg.ConnConfig.Tracer = tracer
				pool, err := pgxpool.NewWithConfig(ctx, cfg)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(pool.Close)
				o.DB = &db.DB{Pool: pool}
			})
			org := env.NewOrg("alice")
			exec := func(sql string, args ...any) {
				t.Helper()
				if _, err := env.DB.Pool.Exec(ctx, sql, args...); err != nil {
					t.Fatal(err)
				}
			}
			exec("INSERT INTO agent_releases (version, host_os, arch, is_default, revoked) VALUES ('1.1.0', 1, 1, true, false), ('1.0.0', 1, 1, false, false)")
			healthyDevice, healthy := setupAgent(t, env, org, nil)
			device, agent := setupAgent(t, env, org, nil)
			for _, stream := range []*testenv.FakeAgent{healthy, agent} {
				stream.WaitFor("initial target", func(e *ownsandv1.AgentEnvelope) bool {
					return e.GetCommand().GetUpdateAgent().GetTargetVersion() == "1.1.0"
				})
				heartbeat(stream, 1)
			}
			hub := env.App.Service.Hub
			original := hub.Live(device.ID)
			control := env.App.Control
			latestID := func() int64 {
				t.Helper()
				id, err := events.LatestID(ctx, env.DB.Pool)
				if err != nil {
					t.Fatal(err)
				}
				return id
			}
			waitForCursor := func(id int64) {
				t.Helper()
				testenv.Eventually(t, fmt.Sprintf("processed cursor %d", id), func() bool {
					got, ok := control.ProcessedCursor()
					return ok && got == id
				})
			}
			setPolicy := func(mode ownsandv1.AgentVersionPolicyMode, version string) {
				t.Helper()
				_, err := org.OwnerClient.Device.UpdateAgentVersionPolicy(ctx, connect.NewRequest(&ownsandv1.UpdateAgentVersionPolicyRequest{
					OrganizationId: org.ID, Mode: mode, PinnedVersion: version,
				}))
				if err != nil {
					t.Fatal(err)
				}
			}
			before := latestID()
			waitForCursor(before)
			fault := &agentPolicyLookupFault{deviceID: device.ID, resume: make(chan struct{})}
			var resumeOnce sync.Once
			resume := func() { resumeOnce.Do(func() { close(fault.resume) }) }
			defer resume()
			tracer.fault.Store(fault)
			setPolicy(hold, "")
			holdID := latestID()
			healthy.WaitFor("withdrawal on the healthy channel", isWithdrawal)
			testenv.Eventually(t, "retry for the HOLD control event", func() bool {
				logs := env.Logs.String()
				return strings.Contains(logs, fmt.Sprintf(`"msg":"control_event_retry","event_id":%d,`, holdID)) &&
					strings.Contains(logs, "context canceled")
			})
			if got, ok := control.ProcessedCursor(); !ok || got != before {
				t.Fatalf("cursor after lookup failure = %d (set %t), want %d before HOLD event %d", got, ok, before, holdID)
			}
			heartbeat(agent, 2)
			if got := decisions(agent); !slices.Equal(got, []string{"1.1.0"}) {
				t.Fatalf("affected channel during lookup failure = %v, want only the initial target", got)
			}
			t.Logf("HOLD event %d pending at cursor %d after one withdrawal on the healthy channel", holdID, before)

			var replacement *testenv.FakeAgent
			switch scenario.name {
			case "latest policy":
				setPolicy(pin, "1.2.0")
			case "revoked version":
				exec("UPDATE agent_releases SET revoked = true WHERE version = '1.0.0'")
				// The healthy channel may have evaluated the retry before
				// the revocation. A fresh event makes it read the new state.
				setPolicy(hold, "")
			case "closed channel":
				agent.Close()
				testenv.Eventually(t, "closed channel", func() bool { return hub.Get(device.ID) == nil })
			case "replaced channel":
				replacement = env.NewFakeAgent(device)
				replacement.OpenChannel()
				testenv.Eventually(t, "replacement channel registered", func() bool {
					current := hub.Get(device.ID)
					return current != nil && current != original
				})
			case "missing device":
				exec("DELETE FROM devices WHERE device_id = $1", device.ID)
			case "removed device":
				exec("UPDATE devices SET state = $2 WHERE device_id = $1", device.ID, int16(ownsandv1.DeviceState_DEVICE_STATE_REMOVED))
			}

			// Write a following event while the retry is still blocked.
			// Its delivery proves that replay did not hold back other events.
			_, err := org.OwnerClient.Device.UpdateDeviceLimits(ctx, connect.NewRequest(&ownsandv1.UpdateDeviceLimitsRequest{
				OrganizationId: org.ID, DeviceId: healthyDevice.ID,
				Limits: &ownsandv1.DeviceLimits{VcpuCount: 6, MemoryMib: 8192, DiskGib: 100, VcpuOvercommitPercent: 100},
			}))
			if err != nil {
				t.Fatal(err)
			}
			last := latestID()
			resume()
			waitForCursor(last)
			healthy.WaitFor("configuration from the later event", func(e *ownsandv1.AgentEnvelope) bool {
				return e.GetConfiguration().GetLimits().GetVcpuCount() == 6
			})
			if got := decisions(healthy); !slices.Equal(got, scenario.healthy) {
				t.Fatalf("healthy channel decisions = %v, want %v", got, scenario.healthy)
			}
			switch scenario.name {
			case "same channel", "latest policy", "revoked version":
				heartbeat(agent, 3)
				if hub.Live(device.ID) != original {
					t.Fatal("the affected channel changed during replay")
				}
			case "missing device", "removed device":
				// The policy evaluation sees a live channel with obsolete
				// inventory. A heartbeat then closes it through the usual
				// path, so the collected decisions cover the entire stream.
				agent.Heartbeat(3)
			}
			if scenario.name == "replaced channel" || scenario.name == "removed device" || scenario.name == "missing device" {
				testenv.Eventually(t, "obsolete stream closed", func() bool {
					select {
					case <-agent.Closed():
						return true
					default:
						return false
					}
				})
			}
			if got := decisions(agent); !slices.Equal(got, scenario.affected) {
				t.Fatalf("affected channel decisions = %v, want %v", got, scenario.affected)
			}
			if replacement != nil {
				replacement.WaitFor("withdrawal on the replacement channel", isWithdrawal)
				heartbeat(replacement, 3)
				if got := decisions(replacement); !slices.Equal(got, scenario.replacement) {
					t.Fatalf("replacement channel decisions = %v, want %v", got, scenario.replacement)
				}
			}
			t.Logf("cursor advanced to %d with healthy decisions %v and affected decisions %v", last, decisions(healthy), decisions(agent))
		})
	}
}
