// SPDX-License-Identifier: FSL-1.1-ALv2

package guestlisten

import (
	"errors"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// failureBound only bounds a test that would otherwise hang when the code
// under test is broken. A passing test never waits for it.
const failureBound = 30 * time.Second

// restoreLockSeams resets the seams of the lock wait when the test ends.
func restoreLockSeams(t *testing.T) {
	t.Helper()
	sleep, budget, hook := unixLockSleep, unixLockBudget, beforeUnixRemove
	t.Cleanup(func() {
		unixLockSleep, unixLockBudget, beforeUnixRemove = sleep, budget, hook
	})
}

// makeStaleSocket leaves a socket file with no listener at path, like a
// helper that was killed.
func makeStaleSocket(t *testing.T, path string) {
	t.Helper()
	l, err := Listen("unix://" + path)
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(path); err != nil || info.Mode().Type() != os.ModeSocket {
		t.Fatalf("stale socket file missing: %v", err)
	}
}

type listenResult struct {
	l   net.Listener
	err error
}

func listenAsync(spec string) <-chan listenResult {
	out := make(chan listenResult, 1)
	go func() {
		l, err := Listen(spec)
		out <- listenResult{l, err}
	}()
	return out
}

func receive[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(failureBound):
		t.Fatalf("timed out waiting for %s", what)
		panic("unreachable")
	}
}

// TestListenUnixSecondStartWaitsForStaleSocketReplacement pauses the first
// start between the probe of a stale socket and its removal, starts a second
// one, and checks that the second start waits for the lock instead of running
// its own probe, removal and bind. After the first start has bound, the
// second one reports the path as served and leaves the path of the first.
func TestListenUnixSecondStartWaitsForStaleSocketReplacement(t *testing.T) {
	restoreLockSeams(t)
	t.Chdir(t.TempDir())
	const path = "g.sock"
	makeStaleSocket(t, path)

	reached := make(chan struct{})
	proceed := make(chan struct{})
	release := sync.OnceFunc(func() { close(proceed) })
	var once sync.Once
	beforeUnixRemove = func() {
		once.Do(func() {
			close(reached)
			<-proceed
		})
	}
	// Signal the first poll and keep the lock wait bounded if it never clears.
	unixLockBudget = failureBound
	polled := make(chan struct{})
	var pollOnce sync.Once
	unixLockSleep = func(d time.Duration) {
		pollOnce.Do(func() { close(polled) })
		time.Sleep(d)
	}

	type listenerStart struct {
		done   chan struct{}
		result listenResult
	}
	var started []*listenerStart
	start := func() *listenerStart {
		worker := &listenerStart{done: make(chan struct{})}
		started = append(started, worker)
		go func() {
			l, err := Listen("unix://" + path)
			worker.result = listenResult{l, err}
			close(worker.done)
		}()
		return worker
	}
	// Join all starts before restoring the hooks or working directory. Keep
	// the first listener open until the second start has finished its probe.
	t.Cleanup(func() {
		release()
		for _, worker := range started {
			<-worker.done
		}
		for _, worker := range started {
			if l := worker.result.l; l != nil {
				l.Close()
			}
		}
	})

	first := start()
	receive(t, reached, "the first start to reach the removal")
	second := start()
	receive(t, polled, "the second start to wait for the lock")
	select {
	case <-second.done:
		t.Fatalf("the second start finished while the first held the lock: %v", second.result.err)
	default:
	}
	release()

	receive(t, first.done, "the first start")
	f := first.result
	if f.err != nil {
		t.Fatalf("first start: %v", f.err)
	}
	go serveOnce(f.l)
	receive(t, second.done, "the second start")
	s := second.result
	if s.err == nil {
		t.Fatal("the second start took the path of the first listener")
	}
	if !errors.Is(s.err, ErrAddressInUse) {
		t.Fatalf("second start err = %v, want ErrAddressInUse", s.err)
	}
	if strings.Contains(s.err.Error(), path) {
		t.Fatalf("error contains the path: %v", s.err)
	}
	requireAnswers(t, path)
}

// TestListenUnixConcurrentStartsOnStaleSocket starts several listeners on one
// stale socket file at once. Exactly one wins, the others report the path as
// served, and the winner stays reachable.
func TestListenUnixConcurrentStartsOnStaleSocket(t *testing.T) {
	t.Chdir(t.TempDir())
	const path = "g.sock"
	makeStaleSocket(t, path)

	const starts = 6
	results := make([]<-chan listenResult, starts)
	for i := range results {
		results[i] = listenAsync("unix://" + path)
	}
	var winners []net.Listener
	for i, ch := range results {
		r := receive(t, ch, "a start")
		if r.err == nil {
			winners = append(winners, r.l)
			continue
		}
		if !errors.Is(r.err, ErrAddressInUse) {
			t.Errorf("start %d err = %v, want ErrAddressInUse", i, r.err)
		}
	}
	for _, l := range winners {
		defer l.Close()
	}
	if len(winners) != 1 {
		t.Fatalf("%d starts won the path, want exactly 1", len(winners))
	}
	go serveOnce(winners[0])
	requireAnswers(t, path)
}

// TestListenUnixLockWaitTimesOut checks that a start gives up after its wait
// budget when another start never releases the lock, without a path in the
// error, and that it leaves the socket path untouched. The wait counts its
// pauses, so the test replaces them with a counter.
func TestListenUnixLockWaitTimesOut(t *testing.T) {
	restoreLockSeams(t)
	t.Chdir(t.TempDir())
	const path = "g.sock"
	makeStaleSocket(t, path)
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}

	release, err := lockUnixAddress(path)
	if err != nil {
		t.Fatal(err)
	}
	released := false
	defer func() {
		if !released {
			release()
		}
	}()

	const polls = 3
	unixLockBudget = polls * unixLockPollInterval
	var pauses int
	unixLockSleep = func(d time.Duration) {
		if d != unixLockPollInterval {
			t.Errorf("pause = %v, want %v", d, unixLockPollInterval)
		}
		pauses++
	}
	l, err := Listen("unix://" + path)
	if err == nil {
		l.Close()
		t.Fatal("Listen succeeded while another start held the lock")
	}
	if !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("err = %v, want ErrLockTimeout", err)
	}
	if strings.Contains(err.Error(), path) {
		t.Fatalf("error contains the path: %v", err)
	}
	if pauses != polls {
		t.Fatalf("paused %d times, want %d", pauses, polls)
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("socket file changed: %v", err)
	}

	// Once the holder releases the lock, a start proceeds and replaces the
	// stale socket file.
	release()
	released = true
	l, err = Listen("unix://" + path)
	if err != nil {
		t.Fatalf("Listen after the release: %v", err)
	}
	defer l.Close()
	go serveOnce(l)
	requireAnswers(t, path)
}

// TestListenUnixReleasesLockAfterFailure checks that a start that fails
// (here: the path holds a regular file) releases the lock for the next one.
func TestListenUnixReleasesLockAfterFailure(t *testing.T) {
	restoreLockSeams(t)
	t.Chdir(t.TempDir())
	const path = "g.sock"
	if err := os.WriteFile(path, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A held lock would make a later start wait; a zero budget turns that
	// wait into an immediate ErrLockTimeout.
	unixLockBudget = 0
	unixLockSleep = func(time.Duration) { t.Error("start waited for a lock that was not held") }
	for range 2 {
		if l, err := Listen("unix://" + path); err == nil {
			l.Close()
			t.Fatal("Listen replaced a non-socket entry")
		} else if !errors.Is(err, ErrAddressOccupied) {
			t.Fatalf("err = %v, want ErrAddressOccupied", err)
		}
	}
}

// TestLockUnixAddressSkipsAddressesWithoutLockFile checks the two cases that
// need no lock: a Linux abstract address, and a path in a missing directory.
func TestLockUnixAddressSkipsAddressesWithoutLockFile(t *testing.T) {
	t.Chdir(t.TempDir())
	if runtime.GOOS == "linux" {
		release, err := lockUnixAddress("@guestlisten-lock-test")
		if err != nil {
			t.Fatal(err)
		}
		release()
	}
	release, err := lockUnixAddress("no-such-dir/g.sock")
	if err != nil {
		t.Fatalf("a missing directory must leave the error to the bind: %v", err)
	}
	release()
	if _, err := os.Lstat("no-such-dir"); err == nil {
		t.Fatal("the lock created a directory")
	}
}

// TestListenUnixLockFileIsSidecar checks that the sidecar lock
// file stays beside the socket and that TCP listeners create none.
func TestListenUnixLockFileIsSidecar(t *testing.T) {
	t.Chdir(t.TempDir())
	l, err := Listen("tcp://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
	if entries, _ := os.ReadDir("."); len(entries) != 0 {
		t.Fatalf("a TCP listener created files: %v", entries)
	}
	u, err := Listen("unix://g.sock")
	if err != nil {
		t.Fatal(err)
	}
	u.Close()
	if _, err := os.Lstat("g.sock" + unixLockSuffix); err != nil {
		t.Fatalf("lock file missing: %v", err)
	}
}
