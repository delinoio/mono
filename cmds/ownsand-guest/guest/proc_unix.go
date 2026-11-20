//go:build !windows

package guest

import (
	"os"
	"os/exec"
	"syscall"
	"time"
)

// prepareProcess starts the command in its own process group, so a timeout
// stops the command and every process that it started.
func prepareProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// terminateProcess sends SIGTERM to the process group and SIGKILL after grace
// when the group leader has not exited.
func terminateProcess(cmd *exec.Cmd, grace time.Duration) {
	if cmd.Process == nil {
		return
	}
	pgid := cmd.Process.Pid
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	time.AfterFunc(grace, func() {
		// Kill the group even when the leader exited: children that ignored
		// SIGTERM must not outlive the timeout.
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	})
}

// exitSignal returns the signal that ended the process, if any.
func exitSignal(ps *os.ProcessState) (int, bool) {
	if ps == nil {
		return 0, false
	}
	ws, ok := ps.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() {
		return 0, false
	}
	return int(ws.Signal()), true
}
