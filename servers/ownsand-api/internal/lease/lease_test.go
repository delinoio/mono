// SPDX-License-Identifier: FSL-1.1-ALv2

package lease

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

type memBackend struct {
	mu     sync.Mutex
	holder string
}

func (m *memBackend) AcquireLease(_ context.Context, _, holder string, _ time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.holder == "" || m.holder == holder {
		m.holder = holder
		return true, nil
	}
	return false, nil
}

func (m *memBackend) ReleaseLease(_ context.Context, _, holder string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.holder == holder {
		m.holder = ""
	}
	return nil
}

func TestHandover(t *testing.T) {
	b := &memBackend{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var events []string
	var mu sync.Mutex
	rec := func(e string) func() { return func() { mu.Lock(); events = append(events, e); mu.Unlock() } }
	old := &Manager{Backend: b, Holder: "old", Logger: logger, OnAcquired: rec("old+"), OnLost: rec("old-")}
	next := &Manager{Backend: b, Holder: "new", Logger: logger, OnAcquired: rec("new+"), OnLost: rec("new-")}
	ctx := context.Background()
	old.tick(ctx)
	next.tick(ctx)
	if !old.Held() || next.Held() {
		t.Fatal("first holder")
	}
	old.Release(ctx)
	old.tick(ctx) // released managers never take the lease again
	next.tick(ctx)
	if old.Held() || !next.Held() {
		t.Fatal("handover")
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"old+", "old-", "new+"}
	if len(events) != len(want) {
		t.Fatalf("events %v", events)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("events %v", events)
		}
	}
}
