// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/servers/ownsand-api/internal/app"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// createSandboxClock is the app clock of a test that places the time samples
// of one create request on both sides of a key's expire_time, which a
// monotonic clock cannot do. By default it reads the test clock. While armed
// with a script of offsets, it plays them back in order to the calls that
// CreateSandbox makes (the last offset repeats), as an offset from the test
// clock. Calls from other code always read the test clock. It is installed
// through a testenv option, because background workers read the app clock
// concurrently and the field must not change after the app starts.
type createSandboxClock struct {
	mu      sync.Mutex
	base    func() time.Time
	offsets []time.Duration
	calls   int
}

func (c *createSandboxClock) arm(offsets ...time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offsets, c.calls = offsets, 0
}

func (c *createSandboxClock) disarm() { c.arm() }

func (c *createSandboxClock) now() time.Time {
	c.mu.Lock()
	armed := len(c.offsets) > 0
	c.mu.Unlock()
	if !armed || !calledFromCreateSandbox() {
		return c.base()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.offsets) == 0 {
		return c.base()
	}
	i := min(c.calls, len(c.offsets)-1)
	c.calls++
	return c.base().Add(c.offsets[i])
}

func calledFromCreateSandbox() bool {
	pcs := make([]uintptr, 32)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(1, pcs)])
	for {
		f, more := frames.Next()
		if strings.Contains(f.Function, "SandboxHandler.CreateSandbox") {
			return true
		}
		if !more {
			return false
		}
	}
}

// TestCreateIdempotencyExpiryBoundary checks that one create request decides
// on a key's expiry with a single time sample: a key that the locked lookup
// sees as expired stays reusable by the write, and an active key still
// replays.
func TestCreateIdempotencyExpiryBoundary(t *testing.T) {
	script := &createSandboxClock{}
	env := testenv.New(t, func(o *app.Options) {
		script.base = o.Now
		o.Now = script.now
	})
	ctx := context.Background()
	org := env.NewOrg("alice")
	setupAgent(t, env, org, nil)
	c := org.OwnerClient
	req := createReq(org.ID)
	first, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(req))
	if err != nil {
		t.Fatal(err)
	}
	firstID := first.Msg.GetSandbox().GetSandboxId()

	// Active key: a replay returns the original sandbox, and another request
	// body under the same key still conflicts.
	again, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(req))
	if err != nil || again.Msg.GetSandbox().GetSandboxId() != firstID {
		t.Fatalf("active replay: %v %v", again, err)
	}
	changed := createReq(org.ID)
	changed.IdempotencyKey = req.IdempotencyKey
	changed.Resources.VcpuCount = 1
	if _, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(changed)); testenv.Code(err) != connect.CodeAlreadyExists {
		t.Fatalf("active conflict: %v", err)
	}

	// The key's expire_time is the creation time plus 24 hours; the test
	// clock has not moved since the first request.
	expireOffset := 24 * time.Hour

	// A request whose samples straddle the expiry. The script gives the
	// samples of CreateSandbox in order: the billing check, the
	// pre-transaction lookup (at the expiry, so the key reads as expired),
	// the request's own time (one millisecond before the expiry, as if the
	// samples were taken in the other order) and every later sample at the
	// expiry. With one sample per request the key reads as active inside the
	// transaction and replays. With separate samples the locked lookup reads
	// the key as expired while the upsert still reads it as active, so the
	// upsert matches no row and the request fails with an internal error.
	script.arm(expireOffset, expireOffset, expireOffset-time.Millisecond, expireOffset)
	got, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(req))
	if err != nil {
		t.Fatalf("request across the expiry boundary: %v", err)
	}
	if got.Msg.GetSandbox().GetSandboxId() != firstID {
		t.Fatalf("request with an active key: sandbox %s, want replay of %s", got.Msg.GetSandbox().GetSandboxId(), firstID)
	}

	// A request whose samples are all at the expiry reuses the key for a new
	// sandbox, and the key then replays that sandbox.
	script.disarm()
	env.Clock.Advance(expireOffset)
	reused, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(req))
	if err != nil {
		t.Fatalf("reuse after expiry: %v", err)
	}
	reusedID := reused.Msg.GetSandbox().GetSandboxId()
	if reusedID == firstID {
		t.Fatalf("reuse after expiry returned the old sandbox %s", firstID)
	}
	replay, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(req))
	if err != nil || replay.Msg.GetSandbox().GetSandboxId() != reusedID {
		t.Fatalf("replay after reuse: %v %v", replay, err)
	}
}
