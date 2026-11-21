// SPDX-License-Identifier: Apache-2.0

package netpolicy

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

// Run refreshes the policy periodically only while a sandbox is attached
// and returns when its context ends.
func TestRunRefreshesWhileAttached(t *testing.T) {
	m := NewManager(slog.New(slog.NewJSONHandler(io.Discard, nil)))
	var n atomic.Int32
	m.interval = time.Millisecond
	m.refresh = func() error { n.Add(1); return nil }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()

	time.Sleep(20 * time.Millisecond)
	if got := n.Load(); got != 0 {
		t.Fatalf("%d refreshes without an attached sandbox", got)
	}
	m.mu.Lock()
	m.attached["s"] = &attachment{}
	m.mu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for n.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("no periodic refresh while a sandbox is attached")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}
