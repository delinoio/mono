// SPDX-License-Identifier: FSL-1.1-ALv2

package fcrt

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// barrierWait bounds every wait of a fake-driven ordering test.
const barrierWait = 5 * time.Second

// settle is how long a test waits before it asserts that something did NOT
// happen.
const settle = 100 * time.Millisecond

// fakeChild is one tracked launch with fake observation, reaping and
// finalization. The test controls each step through a channel.
type fakeChild struct {
	v *vm
	l *launch
	// observeResults feeds the observer: each receive of the fake returns
	// the next value, and a nil value reports the (non-reaping) exit.
	observeResults chan error
	// waitCalled closes when the destructive wait starts; released is set
	// right before it returns, which models the release of the PID.
	waitCalled chan struct{}
	released   atomic.Bool
	waits      atomic.Int32
	// finishRelease unblocks the output/log finalization.
	finishRelease chan struct{}
	finishes      atomic.Int32
}

// fakeLaunch registers a tracked VM whose launch runs with fakes, and starts
// its reaper.
func (f *fixture) fakeLaunch(id string, pid int) *fakeChild {
	f.t.Helper()
	v, exited := f.trackedVM(id, pid)
	c := &fakeChild{
		v: v, observeResults: make(chan error), waitCalled: make(chan struct{}),
		finishRelease: make(chan struct{}),
	}
	c.l = &launch{
		pid: pid, exited: exited,
		observe: func(int) error { return <-c.observeResults },
		wait: func() {
			c.waits.Add(1)
			close(c.waitCalled)
			c.released.Store(true)
		},
		finish: func() {
			<-c.finishRelease
			c.finishes.Add(1)
		},
	}
	v.launch = c.l
	f.r.observeRetry = time.Millisecond
	go c.l.supervise(f.r.logger, id, f.r.observeRetry)
	f.t.Cleanup(func() {
		// Never leave the reaper blocked after the test.
		select {
		case <-c.l.exited:
			return
		default:
		}
		go func() {
			for {
				select {
				case c.observeResults <- nil:
				case <-c.l.exited:
					return
				case <-time.After(barrierWait):
					return
				}
			}
		}()
		c.finish()
	})
	return c
}

// exit delivers the observation of the child's exit.
func (c *fakeChild) exit(t *testing.T) {
	t.Helper()
	select {
	case c.observeResults <- nil:
	case <-time.After(barrierWait):
		t.Fatal("the reaper did not take the exit observation")
	}
}

func (c *fakeChild) finish() {
	select {
	case <-c.finishRelease:
	default:
		close(c.finishRelease)
	}
}

func closed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func recvOrFatal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(barrierWait):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// Scenario 1: stop is inside its group-signal callback when the exit is
// observed. The destructive wait must not run until the callback returned.
func TestStopSignalFinishesBeforeTheReaperReaps(t *testing.T) {
	f := newFixture(t)
	c := f.fakeLaunch("sb1", 321)
	inSignal := make(chan struct{})
	releaseSignal := make(chan struct{})
	var signalled atomic.Int32
	f.r.signalGroup = func(pid int) error {
		signalled.Add(1)
		close(inSignal)
		<-releaseSignal
		if c.waits.Load() != 0 {
			t.Error("the child was reaped while the group signal was in flight")
		}
		return nil
	}
	stopErr := make(chan error, 1)
	go func() { stopErr <- f.r.stop(c.v) }()
	recvOrFatal(t, inSignal, "the signal callback")
	c.exit(t)
	select {
	case <-c.waitCalled:
		t.Fatal("the destructive wait ran inside the signal callback")
	case <-time.After(settle):
	}
	close(releaseSignal)
	recvOrFatal(t, c.waitCalled, "the destructive wait")
	if closed(c.l.exited) {
		t.Fatal("completion published before the finalization")
	}
	c.finish()
	if err := <-stopErr; err != nil {
		t.Fatalf("stop: %v", err)
	}
	recvOrFatal(t, c.l.exited, "the completion")
	if c.waits.Load() != 1 || c.finishes.Load() != 1 || signalled.Load() != 1 {
		t.Fatalf("waits %d, finishes %d, signals %d; want 1 each", c.waits.Load(), c.finishes.Load(), signalled.Load())
	}
}

// Scenarios 2 and 3: the reaper committed to reap and released the child,
// but the finalization keeps exited open. stop must not signal, must time
// out with its retry state retained, and a retry after the finalization
// succeeds without a signal. The released identity never reaches the
// callback; a later launch has its own gate and stays stoppable.
func TestStopAfterReapingCommitDoesNotSignal(t *testing.T) {
	f := newFixture(t)
	f.r.stopTimeout = 150 * time.Millisecond
	c := f.fakeLaunch("sb1", 321)
	other := f.fakeLaunch("sb2", 654)
	var signalled sync.Map
	f.r.signalGroup = func(pid int) error {
		if pid == 321 && c.released.Load() {
			t.Error("a group signal began after the child was released")
		}
		signalled.Store(pid, true)
		return nil
	}
	c.exit(t)
	recvOrFatal(t, c.waitCalled, "the destructive wait")
	err := f.r.stop(c.v)
	if err == nil || !strings.Contains(err.Error(), "did not stop") {
		t.Fatalf("stop while the finalization is pending: %v", err)
	}
	if closed(c.l.exited) {
		t.Fatal("completion published before the finalization")
	}
	if _, ok := signalled.Load(321); ok {
		t.Fatal("signalled a child whose reaping began")
	}
	if c.v.cmd == nil {
		t.Fatal("process reference dropped while the stop failed")
	}
	// The later launch is independent of the pending finalization of A.
	otherDone := make(chan error, 1)
	go func() { otherDone <- f.r.stop(other.v) }()
	deadline := time.Now().Add(barrierWait)
	for {
		if _, ok := signalled.Load(654); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the other launch was not signalled")
		}
		time.Sleep(time.Millisecond)
	}
	other.exit(t)
	other.finish()
	if err := <-otherDone; err != nil {
		t.Fatalf("stop of the other launch: %v", err)
	}
	c.finish()
	recvOrFatal(t, c.l.exited, "the completion")
	if err := f.r.stop(c.v); err != nil {
		t.Fatalf("retry after the finalization: %v", err)
	}
	if c.v.cmd != nil {
		t.Fatal("stopped child still referenced")
	}
	if c.waits.Load() != 1 || c.finishes.Load() != 1 {
		t.Fatalf("waits %d, finishes %d; want 1 each", c.waits.Load(), c.finishes.Load())
	}
}

// Scenario 4: a live child (and its same-group descendants) is stopped with
// the group callback; a leader that does not end keeps the registration,
// the directory handle and the retry state; after the exit, a retry cleans
// up without another signal.
func TestDeleteTrackedLaunchKeepsStateUntilExit(t *testing.T) {
	f := newFixture(t)
	f.r.stopTimeout = 100 * time.Millisecond
	c := f.fakeLaunch("sb1", 321)
	var mu sync.Mutex
	var groups []int
	f.r.signalGroup = func(pid int) error {
		mu.Lock()
		groups = append(groups, pid)
		mu.Unlock()
		return nil // delivered, but the leader does not end
	}
	err := f.r.Delete(context.Background(), "sb1")
	if err == nil || !strings.Contains(err.Error(), "did not stop") {
		t.Fatalf("Delete of an immortal leader: %v", err)
	}
	if _, lerr := f.r.lookup("sb1"); lerr != nil {
		t.Fatal("registration dropped while the leader lives")
	}
	if _, serr := c.v.dirFile.Stat(); serr != nil || !f.exists(c.v.dir) {
		t.Fatal("VM directory or its handle gone while the leader lives")
	}
	c.exit(t)
	c.finish()
	recvOrFatal(t, c.l.exited, "the completion")
	if err := f.r.Delete(context.Background(), "sb1"); err != nil {
		t.Fatalf("Delete after the exit: %v", err)
	}
	if f.exists(c.v.dir) {
		t.Fatal("VM directory kept")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(groups) != 1 || groups[0] != 321 {
		t.Fatalf("group signals %v, want one for the leader's group", groups)
	}
}

// Scenario 5: an interrupted observation is retried and keeps the signal
// path open; another failure disables group signals, is logged, publishes
// no completion and never reaps; the recovered observation reaps once.
func TestObservationErrorsNeitherExitNorReap(t *testing.T) {
	f := newFixture(t)
	f.r.stopTimeout = 100 * time.Millisecond
	c := f.fakeLaunch("sb1", 321)
	var signalled atomic.Int32
	send := func(int) error { signalled.Add(1); return nil }
	push := func(err error) {
		t.Helper()
		select {
		case c.observeResults <- err:
		case <-time.After(barrierWait):
			t.Fatal("the reaper did not take the observation result")
		}
	}
	push(syscall.EINTR)
	push(syscall.EINTR)
	// The reaper is back in its observation: the child is still owned.
	if state, _ := c.l.signal(send); state != signalAllowed {
		t.Fatalf("state %q after interrupted observations", state)
	}
	push(syscall.EINVAL)
	deadline := time.Now().Add(barrierWait)
	for {
		c.l.mu.Lock()
		uncertain := c.l.uncertain
		c.l.mu.Unlock()
		if uncertain {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a failed observation did not disable group signals")
		}
		time.Sleep(time.Millisecond)
	}
	push(syscall.ECHILD)
	f.r.signalGroup = func(int) error { t.Error("signalled with uncertain ownership"); return nil }
	if err := f.r.stop(c.v); err == nil {
		t.Fatal("stop succeeded without an observed exit")
	}
	if closed(c.l.exited) || c.waits.Load() != 0 {
		t.Fatal("an observation error looked like an exit")
	}
	if out := f.logs.String(); !strings.Contains(out, `"sandbox_id":"sb1"`) || !strings.Contains(out, "observe firecracker exit failed") {
		t.Fatalf("no structured diagnostic for the observation failure: %s", out)
	}
	if signalled.Load() != 1 {
		t.Fatalf("%d signals, want the one before the failure", signalled.Load())
	}
	c.exit(t)
	c.finish()
	recvOrFatal(t, c.l.exited, "the completion")
	if c.waits.Load() != 1 || c.finishes.Load() != 1 {
		t.Fatalf("waits %d, finishes %d; want 1 each", c.waits.Load(), c.finishes.Load())
	}
	if err := f.r.stop(c.v); err != nil {
		t.Fatalf("stop after the recovery: %v", err)
	}
}

// Against real children: stop races the exit of the child. A group signal
// must never begin after the reaping transition, and every stop ends with
// the child gone and the completion published.
func TestStopRacesRealChildExit(t *testing.T) {
	bin := helperBinary(t)
	t.Setenv(helperEnv, "1")
	for i := 0; i < 20; i++ {
		vms := t.TempDir()
		r, err := newRuntime(Options{Binary: bin, VMsDir: vms, Logger: quiet()}, func() error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		r.stopTimeout = 5 * time.Second
		dir := filepath.Join(vms, "sb1")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		dirFile, err := os.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		v := &vm{id: "sb1", dir: dir, dirFile: dirFile, network: ownsandv1.NetworkMode_NETWORK_MODE_NONE}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		// The stand-in serves no API socket, so the start fails after the launch.
		_ = r.startProcess(ctx, v)
		cancel()
		if v.cmd == nil {
			t.Fatal("no child started")
		}
		l := v.launch
		realSignal := r.signalGroup
		r.signalGroup = func(pid int) error {
			// The callback runs under the launch gate.
			if l.reaping {
				t.Error("a group signal began after the reaping transition")
			}
			return realSignal(pid)
		}
		proc := v.cmd.Process // stop clears v.cmd
		forced := make(chan struct{})
		go func() {
			defer close(forced)
			// os.Process.Kill never signals a reaped child's PID.
			if err := proc.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				t.Errorf("force the exit: %v", err)
			}
		}()
		if err := r.stop(v); err != nil {
			t.Fatalf("stop %d: %v", i, err)
		}
		<-forced
		if !closed(l.exited) {
			t.Fatalf("stop %d returned before the completion", i)
		}
		dirFile.Close()
	}
}
