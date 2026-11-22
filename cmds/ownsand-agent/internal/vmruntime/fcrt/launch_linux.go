// SPDX-License-Identifier: FSL-1.1-ALv2

package fcrt

import (
	"errors"
	"log/slog"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// observeRetryDelay is the pause between two failed non-reaping observations
// of a child; observeLogEvery limits how often the repeated failure of one
// launch is logged.
const (
	observeRetryDelay = 500 * time.Millisecond
	observeLogEvery   = 20
)

// launch is the lifecycle of one firecracker child that the agent tracks. Its
// reaper and every group signal share the gate below, so that the agent
// signals the process group of the child only while it still owns the PID.
//
// The invariant is signal-before-reap: once the launch committed its reaping
// transition (reaping) or lost track of the child (uncertain), no group
// signal begins, and a signal that began earlier has finished before the
// destructive wait runs. The kernel keeps the PID of an exited but unreaped
// child reserved, so a signal that wins the gate never reaches a process
// that reused the PID.
//
// A launch belongs to exactly one start: a later launch of the same VM (the
// cold-boot fallback of a failed restore) has its own gate and completion.
type launch struct {
	pid int
	// exited closes after the child was reaped, its output was drained and
	// the console log was closed. An open channel therefore does not prove
	// that the child is still unreaped; reaping is the gate's state.
	exited chan struct{}
	// observe blocks until the child exited without reaping it (Linux waitid
	// with WEXITED and WNOWAIT). wait reaps the child and drains its output
	// (exec.Cmd.Wait); finish closes the console log. Only the reaper calls
	// them, and it calls wait at most once.
	observe func(pid int) error
	wait    func()
	finish  func()

	mu sync.Mutex // the gate; never held across observe, wait or finish
	// reaping: the reaper committed to reap the child, so no further group
	// signal may begin.
	reaping bool
	// uncertain: an observation failed, so the agent cannot tell whether it
	// still owns the PID and must not signal it. The reaper keeps retrying
	// and never falls back to an uncoordinated wait.
	uncertain bool
}

// waitExitNoReap blocks until the child pid exited and leaves it as a
// zombie, so that the PID stays reserved until the reaper's own wait.
func waitExitNoReap(pid int) error {
	var info unix.Siginfo
	return unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
}

// signalState says why signal did not call the callback.
type signalState string

const (
	signalAllowed   signalState = "allowed"
	signalReaping   signalState = "reaping"
	signalUncertain signalState = "ownership_uncertain"
)

// signal calls send under the gate when the launch still owns the PID. It
// returns the callback result and the state that it saw; a state other than
// signalAllowed means that send was not called.
func (l *launch) signal(send func(pid int) error) (signalState, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case l.reaping:
		return signalReaping, nil
	case l.uncertain:
		return signalUncertain, nil
	}
	return signalAllowed, send(l.pid)
}

// commitReaping ends group signalling. It waits for a signal that is in
// flight, because that signal holds the gate.
func (l *launch) commitReaping() {
	l.mu.Lock()
	l.reaping = true
	l.mu.Unlock()
}

func (l *launch) markUncertain() {
	l.mu.Lock()
	l.uncertain = true
	l.mu.Unlock()
}

// supervise reaps the child exactly once and publishes the completion. It
// first observes the exit without reaping, outside the gate, so that a live
// child never holds the gate while it waits for a stop signal. An
// interrupted observation is retried at once. Any other failure is logged
// and retried after a pause, and until it recovers the launch forbids group
// signals: it never calls the destructive wait without an observed exit and
// never publishes completion.
func (l *launch) supervise(logger *slog.Logger, sandboxID string, retry time.Duration) {
	for failures := 0; ; {
		err := l.observe(l.pid)
		if err == nil {
			break
		}
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		l.markUncertain()
		if failures%observeLogEvery == 0 {
			logger.Error("observe firecracker exit failed; group signals are disabled until the exit is observed",
				"sandbox_id", sandboxID, "pid", l.pid, "error", err, "failures", failures+1)
		}
		failures++
		time.Sleep(retry)
	}
	l.commitReaping()
	l.wait()
	l.finish()
	close(l.exited)
}
