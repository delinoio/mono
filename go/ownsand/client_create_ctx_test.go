// SPDX-License-Identifier: Apache-2.0

package ownsand

import (
	"context"
	"errors"
	"sync"
	"testing"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
)

// expiringContext is a caller context that a test ends with a deadline
// expiry at an exact point, without a timer: Err returns
// context.DeadlineExceeded once expire has been called.
type expiringContext struct {
	context.Context
	once sync.Once
	done chan struct{}
}

func newExpiringContext() *expiringContext {
	return &expiringContext{Context: context.Background(), done: make(chan struct{})}
}

func (c *expiringContext) Done() <-chan struct{} { return c.done }

func (c *expiringContext) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

func (c *expiringContext) expire() { c.once.Do(func() { close(c.done) }) }

// failingCreateClient replaces the Connect sandbox client. Every CreateSandbox
// attempt fails with an unavailable error that does not wrap a context error,
// after the test hook ran. The hook receives the number of the attempt (from
// 1), so a test can end the caller context during a chosen attempt.
//
// The test needs this seam because the real Connect client reports the
// context error itself in nearly every ordering, so an end-to-end fake cannot
// reach the case on purpose: the failure that the RPC reports (a server status
// or a transport error) is the one that CreateSandbox sees, while the caller
// context has already ended. CreateSandbox must return the context error then.
type failingCreateClient struct {
	ownsandv1connect.SandboxServiceClient
	hook func(attempt int)

	mu   sync.Mutex
	keys []string
}

func (f *failingCreateClient) CreateSandbox(_ context.Context, req *connect.Request[ownsandv1.CreateSandboxRequest]) (*connect.Response[ownsandv1.CreateSandboxResponse], error) {
	f.mu.Lock()
	f.keys = append(f.keys, req.Msg.GetIdempotencyKey())
	n := len(f.keys)
	f.mu.Unlock()
	if f.hook != nil {
		f.hook(n)
	}
	return nil, connect.NewError(connect.CodeUnavailable, errors.New("test control plane unavailable"))
}

func (f *failingCreateClient) idempotencyKeys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.keys...)
}

func newFailingCreateClient(t *testing.T, hook func(attempt int)) (*Client, *failingCreateClient) {
	t.Helper()
	c, err := New(Options{
		BaseURL:        "http://127.0.0.1:1",
		APIKey:         testAPIKey,
		OrganizationID: mustID(t),
		Transport:      TransportRelay,
		LookupEnv:      func(string) (string, bool) { return "", false },
	})
	if err != nil {
		t.Fatal(err)
	}
	c.timing = testTiming()
	f := &failingCreateClient{hook: hook}
	c.sandboxes = f
	return c, f
}

// TestCreateSandboxReturnsCallerContextErrorAfterFailedAttempt checks that a
// failed RPC returns the caller's context error when the caller context
// ended during that RPC, whichever error the RPC itself reported, and that no
// further attempt follows.
func TestCreateSandboxReturnsCallerContextErrorAfterFailedAttempt(t *testing.T) {
	t.Run("initial attempt canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		c, f := newFailingCreateClient(t, func(int) { cancel() })
		sb, err := c.CreateSandbox(ctx, validParams())
		if sb != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("CreateSandbox returned %v and an error that is not the caller's context.Canceled (unavailable: %v)", sb, connect.CodeOf(err) == connect.CodeUnavailable)
		}
		if keys := f.idempotencyKeys(); len(keys) != 1 {
			t.Fatalf("%d attempts, want 1", len(keys))
		}
	})

	t.Run("retry attempt past the caller deadline", func(t *testing.T) {
		ctx := newExpiringContext()
		// Attempt 1 fails while the context is live, so the SDK retries
		// with the same key. The deadline passes during attempt 2.
		c, f := newFailingCreateClient(t, func(attempt int) {
			if attempt == 2 {
				ctx.expire()
			}
		})
		sb, err := c.CreateSandbox(ctx, validParams())
		if sb != nil || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("CreateSandbox returned %v and an error that is not the caller's context.DeadlineExceeded (unavailable: %v)", sb, connect.CodeOf(err) == connect.CodeUnavailable)
		}
		keys := f.idempotencyKeys()
		if len(keys) != 2 || keys[0] == "" || keys[0] != keys[1] {
			t.Fatalf("keys %q, want 2 attempts with one idempotency key", keys)
		}
	})
}

// TestCreateSandboxRetryWindowEndsWithLastFailureForLiveCaller checks that a
// retry window that ends while the caller context is live still returns the
// last failure of the window and not a context error, and that every attempt
// uses the same idempotency key. The window of one nanosecond has ended once
// the first failure is recorded, so the test waits for nothing.
func TestCreateSandboxRetryWindowEndsWithLastFailureForLiveCaller(t *testing.T) {
	c, f := newFailingCreateClient(t, nil)
	c.timing.createRetryWindow = 1
	sb, err := c.CreateSandbox(context.Background(), validParams())
	if sb != nil || err == nil {
		t.Fatalf("CreateSandbox returned %v, %v; want the last failure", sb, err)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errCreateDeadline) {
		t.Fatal("CreateSandbox returned a cancellation instead of the last failure of the window")
	}
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("code %v, want the last failure (unavailable)", connect.CodeOf(err))
	}
	keys := f.idempotencyKeys()
	if len(keys) == 0 || keys[0] == "" {
		t.Fatalf("keys %q, want at least one attempt with an idempotency key", keys)
	}
	for _, k := range keys {
		if k != keys[0] {
			t.Fatalf("keys %q, want one idempotency key", keys)
		}
	}
}

// TestCreateSandboxRetriesWithOneKeyWithinWindow checks that retryable
// failures inside the window reuse one idempotency key until the call
// succeeds, and that a live caller context never turns them into a context
// error.
func TestCreateSandboxRetriesWithOneKeyWithinWindow(t *testing.T) {
	c, _ := newFailingCreateClient(t, nil)
	f := &succeedingAfterCreateClient{failures: 2}
	c.sandboxes = f
	sb, err := c.CreateSandbox(context.Background(), validParams())
	if err != nil || sb == nil {
		t.Fatalf("CreateSandbox returned %v, %v; want a sandbox", sb, err)
	}
	f.mu.Lock()
	keys := append([]string(nil), f.keys...)
	f.mu.Unlock()
	if len(keys) != 3 || keys[0] == "" || keys[0] != keys[1] || keys[1] != keys[2] {
		t.Fatalf("keys %q, want 3 attempts with one idempotency key", keys)
	}
}

// succeedingAfterCreateClient fails CreateSandbox with a retryable error
// `failures` times and then answers it.
type succeedingAfterCreateClient struct {
	ownsandv1connect.SandboxServiceClient
	failures int

	mu   sync.Mutex
	keys []string
}

func (f *succeedingAfterCreateClient) CreateSandbox(_ context.Context, req *connect.Request[ownsandv1.CreateSandboxRequest]) (*connect.Response[ownsandv1.CreateSandboxResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys = append(f.keys, req.Msg.GetIdempotencyKey())
	if len(f.keys) <= f.failures {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("test control plane unavailable"))
	}
	return connect.NewResponse(&ownsandv1.CreateSandboxResponse{Sandbox: &ownsandv1.Sandbox{SandboxId: req.Msg.GetIdempotencyKey()}}), nil
}
