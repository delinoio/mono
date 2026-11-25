// SPDX-License-Identifier: FSL-1.1-ALv2

package processrt

import (
	"errors"
	"log/slog"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/launcher"
	"github.com/delinoio/mono/internal/procgroup"
	"golang.org/x/sys/unix"
)

var errHelperOwnershipUncertain = errors.New("processrt: guest helper PID ownership is uncertain")

func startHelper(cmd *exec.Cmd, logger *slog.Logger) (helperProcess, func(), error) {
	procgroup.Set(cmd)
	if err := launcher.Start(cmd); err != nil {
		return helperProcess{}, nil, err
	}
	process := cmd.Process
	lifecycle := helperLifecycle{send: func(sig syscall.Signal) error {
		return procgroup.Signal(process, sig)
	}}
	observe := func() error {
		var info unix.Siginfo
		return unix.Waitid(unix.P_PID, process.Pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
	}
	wait := func() {
		lifecycle.reap(observe, func() { _ = cmd.Wait() }, logger.With("pid", process.Pid))
	}
	return helperProcess{pid: process.Pid, signal: lifecycle.signal}, wait, nil
}

// helperLifecycle keeps the child PID reserved through every admitted group
// signal. The reaper closes signal admission under the same lock before it
// releases the PID with the command wait.
type helperLifecycle struct {
	mu        sync.Mutex
	signalErr error
	send      func(syscall.Signal) error
}

func (l *helperLifecycle) signal(sig syscall.Signal) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.signalErr != nil {
		return l.signalErr
	}
	return l.send(sig)
}

func (l *helperLifecycle) closeSignals(err error) {
	l.mu.Lock()
	l.signalErr = err
	l.mu.Unlock()
}

func (l *helperLifecycle) reap(observe func() error, wait func(), logger *slog.Logger) {
	// Keep the blocking observation outside the lock so a live helper can
	// still receive a signal. WNOWAIT reserves the PID even after exit.
	for failures := 0; ; {
		err := observe()
		if err == nil {
			break
		}
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		l.closeSignals(errHelperOwnershipUncertain)
		if failures%20 == 0 {
			logger.Error("observe guest helper exit failed", "error", err, "failures", failures+1)
		}
		if errors.Is(err, syscall.ECHILD) {
			// No child remains to observe. Finish the command wait so it
			// can release its resources and publish the exit notification.
			break
		}
		failures++
		time.Sleep(500 * time.Millisecond)
	}
	l.closeSignals(errHelperExited)
	wait()
}
