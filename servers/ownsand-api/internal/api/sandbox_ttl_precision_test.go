// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// TestSandboxTTLKeepsSubMillisecondPrecision creates sandboxes with a TTL
// below one millisecond and with a fraction of a millisecond. The API
// accepts every positive duration, so create, get, an idempotent replay and
// the command that the agent receives must report the same TTL.
func TestSandboxTTLKeepsSubMillisecondPrecision(t *testing.T) {
	cases := []struct {
		name      string
		idle, max time.Duration
	}{
		{"sub-millisecond idle TTL", 500 * time.Microsecond, time.Hour},
		{"fractional-millisecond TTLs", 1500*time.Microsecond + 7*time.Nanosecond, 2*time.Hour + 250*time.Microsecond},
		{"whole-millisecond TTLs", 90 * time.Second, 3 * time.Hour},
		{"one nanosecond", time.Nanosecond, time.Nanosecond},
		{"maximum TTL", time.Hour, 7 * 24 * time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := testenv.New(t)
			ctx := context.Background()
			org := env.NewOrg("alice")
			_, agent := setupAgent(t, env, org, &ownsandv1.DeviceLimits{VcpuCount: 4, MemoryMib: 4096, DiskGib: 100, VcpuOvercommitPercent: 100})
			c := org.OwnerClient

			req := createReq(org.ID)
			req.IdleTtl, req.MaxTtl = durationpb.New(tc.idle), durationpb.New(tc.max)
			created, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(req))
			if err != nil {
				t.Fatal(err)
			}
			id := created.Msg.GetSandbox().GetSandboxId()
			wantTTL := func(where string, sb *ownsandv1.Sandbox) {
				t.Helper()
				if got := sb.GetIdleTtl().AsDuration(); got != tc.idle {
					t.Errorf("%s idle_ttl = %v, want %v", where, got, tc.idle)
				}
				if got := sb.GetMaxTtl().AsDuration(); got != tc.max {
					t.Errorf("%s max_ttl = %v, want %v", where, got, tc.max)
				}
			}
			wantTTL("create", created.Msg.GetSandbox())
			wantTTL("get", getSandbox(t, c, org.ID, id))
			replay, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(req))
			if err != nil || replay.Msg.GetSandbox().GetSandboxId() != id {
				t.Fatalf("replay: %v %v", replay, err)
			}
			wantTTL("replay", replay.Msg.GetSandbox())
			list, err := c.Sandbox.ListSandboxes(ctx, connect.NewRequest(&ownsandv1.ListSandboxesRequest{OrganizationId: org.ID}))
			if err != nil || len(list.Msg.GetSandboxes()) != 1 {
				t.Fatalf("list: %v %v", list, err)
			}
			wantTTL("list", list.Msg.GetSandboxes()[0])

			agent.Eventually("create command", func() bool { return countCreateCommands(agent) >= 1 })
			for _, cmd := range agent.Commands() {
				if cc := cmd.GetCreateSandbox(); cc != nil {
					if cc.GetIdleTtl().AsDuration() != tc.idle || cc.GetMaxTtl().AsDuration() != tc.max {
						t.Fatalf("command TTLs = %v and %v, want %v and %v", cc.GetIdleTtl().AsDuration(), cc.GetMaxTtl().AsDuration(), tc.idle, tc.max)
					}
				}
			}
		})
	}
}

// TestSandboxTTLOfLegacyRows reads sandbox rows that an earlier release
// wrote with the millisecond columns only (the nanosecond columns are NULL)
// and checks that the stored whole-millisecond TTLs keep their values, also
// for a row that the previous release inserts after the migration during a
// rolling deployment. A row with both column sets reads the exact value.
func TestSandboxTTLOfLegacyRows(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	dev, _ := setupAgent(t, env, org, &ownsandv1.DeviceLimits{VcpuCount: 8, MemoryMib: 8192, DiskGib: 100, VcpuOvercommitPercent: 100})
	c := org.OwnerClient

	insert := func(idleMs, maxMs int64, idleNs, maxNs *int64) string {
		t.Helper()
		id, _ := uuidv7.NewString()
		if _, err := env.DB.Pool.Exec(ctx, `INSERT INTO sandboxes (sandbox_id, organization_id, device_id, agent_state, vcpu_count, memory_mib, disk_gib,
			guest_os, host_os, arch, network_mode, idle_ttl_ms, max_ttl_ms, idle_ttl_ns, max_ttl_ns, max_deadline, created_by_kind, created_by_id, create_time)
			VALUES ($1, $2, $3, 2, 1, 512, 1, 1, 1, 1, 1, $4, $5, $6, $7, now() + interval '1 hour', 1, 'alice', now())`,
			id, org.ID, dev.ID, idleMs, maxMs, idleNs, maxNs); err != nil {
			t.Fatal(err)
		}
		return id
	}
	legacy := insert(90000, 3600500, nil, nil)
	exact := int64(1500007)
	both := insert(1, 3600500, &exact, &exact)

	sb := getSandbox(t, c, org.ID, legacy)
	if sb.GetIdleTtl().AsDuration() != 90*time.Second || sb.GetMaxTtl().AsDuration() != 3600500*time.Millisecond {
		t.Fatalf("legacy row TTLs = %v and %v, want 1m30s and 1h0m0.5s", sb.GetIdleTtl().AsDuration(), sb.GetMaxTtl().AsDuration())
	}
	sb = getSandbox(t, c, org.ID, both)
	if sb.GetIdleTtl().AsDuration() != 1500007*time.Nanosecond || sb.GetMaxTtl().AsDuration() != 1500007*time.Nanosecond {
		t.Fatalf("exact row TTLs = %v and %v, want 1.500007ms", sb.GetIdleTtl().AsDuration(), sb.GetMaxTtl().AsDuration())
	}
}
