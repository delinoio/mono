// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// TestConcurrentCreateIdempotencyForLastSlot sends two identical create
// requests for the last slot of a device while a test transaction holds the
// device row lock. Both requests pass the first key lookup (nothing is
// committed yet) and wait on a lock, so the outcome does not depend on
// timing: the second must recover the sandbox of the first instead of
// failing with unavailable because the first took the slot.
func TestConcurrentCreateIdempotencyForLastSlot(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	_, agent := setupAgent(t, env, org, &ownsandv1.DeviceLimits{VcpuCount: 2, MemoryMib: 1024, DiskGib: 10, VcpuOvercommitPercent: 100})
	c := org.OwnerClient

	// Hold the device row lock, as a concurrent placement would.
	blocker, err := env.DB.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	released := false
	release := func() {
		if !released {
			released = true
			_ = blocker.Rollback(ctx)
		}
	}
	defer release()
	if _, err := blocker.Exec(ctx, "SELECT 1 FROM devices WHERE organization_id = $1 FOR UPDATE", org.ID); err != nil {
		t.Fatal(err)
	}

	req := createReq(org.ID)
	type result struct {
		id  string
		err error
	}
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(req))
			r := result{err: err}
			if err == nil {
				r.id = resp.Msg.GetSandbox().GetSandboxId()
			}
			results <- r
		}()
	}
	// Both requests are inside the server and blocked on a lock: one on the
	// device row lock, the other on the key's advisory lock (or, without the
	// key lock, on the device row lock as well).
	testenv.Eventually(t, "two create requests waiting on locks", func() bool {
		var waiting int
		err := env.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'").Scan(&waiting)
		return err == nil && waiting >= 2
	})
	release()
	wg.Wait()
	close(results)
	var ids []string
	for r := range results {
		if r.err != nil {
			t.Fatalf("create: %v", r.err)
		}
		ids = append(ids, r.id)
	}
	if len(ids) != 2 || ids[0] == "" || ids[0] != ids[1] {
		t.Fatalf("sandbox ids = %v, want the same one twice", ids)
	}

	var sandboxes, audit int
	if err := env.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM sandboxes WHERE organization_id = $1", org.ID).Scan(&sandboxes); err != nil {
		t.Fatal(err)
	}
	if err := env.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM audit_events WHERE organization_id = $1 AND action = $2",
		org.ID, int16(ownsandv1.AuditAction_AUDIT_ACTION_SANDBOX_CREATED)).Scan(&audit); err != nil {
		t.Fatal(err)
	}
	if sandboxes != 1 || audit != 1 {
		t.Fatalf("sandboxes = %d, SANDBOX_CREATED events = %d, want 1 and 1", sandboxes, audit)
	}
	agent.Eventually("create command", func() bool { return countCreateCommands(agent) >= 1 })
	// Give a duplicate command the chance to arrive before counting.
	time.Sleep(200 * time.Millisecond)
	if n := countCreateCommands(agent); n != 1 {
		t.Fatalf("create commands = %d, want 1", n)
	}

	// The same key with other valid parameters is already_exists once the
	// first request committed.
	changed := createReq(org.ID)
	changed.IdempotencyKey = req.IdempotencyKey
	changed.Labels = map[string]string{"purpose": "other"}
	if _, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(changed)); testenv.Code(err) != connect.CodeAlreadyExists {
		t.Fatalf("other parameters: %v", err)
	}
	// A distinct key finds no capacity.
	if _, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(createReq(org.ID))); testenv.Code(err) != connect.CodeUnavailable {
		t.Fatalf("new key: %v", err)
	}
	// A sequential retry still returns the stored sandbox while the device
	// is offline.
	agent.Close()
	testenv.Eventually(t, "device offline", func() bool { return env.App.Service.Hub.Get(agent.Device.ID) == nil })
	again, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(req))
	if err != nil || again.Msg.GetSandbox().GetSandboxId() != ids[0] {
		t.Fatalf("retry with the device offline: %v %v", again, err)
	}
}

func countCreateCommands(agent *testenv.FakeAgent) int {
	n := 0
	for _, cmd := range agent.Commands() {
		if cmd.GetCreateSandbox() != nil {
			n++
		}
	}
	return n
}
