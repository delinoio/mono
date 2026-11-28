// SPDX-License-Identifier: FSL-1.1-ALv2

package dbtestslot

import (
	"context"
	"errors"
	"math/rand/v2"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestSlots(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ maxConnections, want int }{
		{0, 1},
		{9, 1},
		{27, 1},
		{36, 2},
		{100, 10},
		{400, 47},
	} {
		if got := Slots(tc.maxConnections); got != tc.want {
			t.Errorf("Slots(%d) = %d, want %d", tc.maxConnections, got, tc.want)
		}
	}
}

// fakeLocker counts the slots that are taken and not yet released.
type fakeLocker struct {
	locks, held atomic.Int32
	err         error
}

func (f *fakeLocker) lock(context.Context) (func(), error) {
	f.locks.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	f.held.Add(1)
	return func() { f.held.Add(-1) }, nil
}

func TestSubtestsShareTheSlotOfTheirTopLevelTest(t *testing.T) {
	t.Parallel()
	var l Limiter
	f := &fakeLocker{}
	t.Run("root", func(t *testing.T) {
		l.Acquire(t, f.lock)
		// A second database of the same test shares the slot.
		l.Acquire(t, f.lock)
		// Cleanups run in reverse order: this one runs after the parallel
		// subtests ended and before the releases of the Acquire calls above.
		t.Cleanup(func() {
			if got := f.held.Load(); got != 1 {
				t.Errorf("held slots after the subtests = %d, want 1", got)
			}
		})
		for _, name := range []string{"a", "b"} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				l.Acquire(t, f.lock)
			})
		}
	})
	if got := f.locks.Load(); got != 1 {
		t.Fatalf("slot locks = %d, want 1", got)
	}
	if got := f.held.Load(); got != 0 {
		t.Fatalf("held slots after the test = %d, want 0", got)
	}
}

func TestEachTopLevelTestTakesItsOwnSlot(t *testing.T) {
	t.Parallel()
	var l Limiter
	f := &fakeLocker{}
	// Subtests of this test stand in for two top-level tests. Their names
	// share the first element, so name the top-level test explicitly.
	for _, root := range []string{"first", "second"} {
		t.Run(root, func(t *testing.T) {
			l.acquireAs(t, root, f.lock)
			if got := f.held.Load(); got != 1 {
				t.Errorf("held slots in %s = %d, want 1", root, got)
			}
		})
	}
	if got := f.locks.Load(); got != 2 {
		t.Fatalf("slot locks = %d, want 2", got)
	}
	if got := f.held.Load(); got != 0 {
		t.Fatalf("held slots after the tests = %d, want 0", got)
	}
}

func TestLockFailureFailsTheTestAndFreesTheEntry(t *testing.T) {
	t.Parallel()
	var l Limiter
	f := &fakeLocker{err: errors.New("no server")}
	var failed bool
	t.Run("root", func(t *testing.T) {
		ft := &fatalRecorder{TB: t}
		func() {
			defer func() { _ = recover() }()
			l.Acquire(ft, f.lock)
		}()
		failed = ft.fatal
	})
	if !failed {
		t.Fatal("Acquire did not fail the test")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.holds) != 0 {
		t.Fatalf("holds after a failed lock = %d, want 0", len(l.holds))
	}
}

// fatalRecorder records Fatalf and stops the caller with a panic instead of
// failing the real test.
type fatalRecorder struct {
	testing.TB
	fatal bool
}

func (r *fatalRecorder) Fatalf(string, ...any) {
	r.fatal = true
	panic("fatal")
}

// adminURL returns the admin URL of a server test database, or skips the
// test. The servers that use this package each read their own variable.
func adminURL(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"ACCOUNTS_TEST_DATABASE_URL", "OWNSAND_TEST_DATABASE_URL", "SANDRUN_TEST_DATABASE_URL"} {
		if v := os.Getenv(name); v != "" {
			return v
		}
	}
	t.Skip("no server test database URL is set; skipping a Postgres-backed test")
	return ""
}

// newTestServer returns a slot connection with two slots in a random lock
// namespace, so that the slots of tests that run at the same time in other
// processes do not count.
func newTestServer(t *testing.T, admin string, namespace int32) *server {
	t.Helper()
	config, err := pgxpool.ParseConfig(admin)
	if err != nil {
		t.Fatal("invalid admin database URL")
	}
	s := &server{config: config.ConnConfig, namespace: namespace, held: map[int32]bool{}}
	s.mu.Lock()
	err = s.connect(t.Context())
	s.slots = 2
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.conn != nil {
			s.closeLocked()
		}
	})
	return s
}

func TestServerSlotsAreExclusive(t *testing.T) {
	t.Parallel()
	admin := adminURL(t)
	namespace := rand.Int32()
	// Two slot connections stand in for two test processes.
	a := newTestServer(t, admin, namespace)
	b := newTestServer(t, admin, namespace)

	// One session does not take a slot that it already holds again.
	releaseA1, err := a.lock(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	releaseA2, err := a.lock(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(a.held) != 2 {
		t.Fatalf("slots held by the first session = %d, want 2", len(a.held))
	}

	// The other session waits while both slots are taken.
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	_, err = b.lock(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock with every slot taken = %v, want the deadline error", err)
	}

	// A released slot goes to the waiting session.
	got := make(chan error, 1)
	go func() {
		release, err := b.lock(t.Context())
		if err == nil {
			release()
		}
		got <- err
	}()
	releaseA1()
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the waiting session did not get the released slot")
	}
	releaseA2()
	if len(a.held) != 0 {
		t.Fatalf("slots held after the releases = %d, want 0", len(a.held))
	}
}
