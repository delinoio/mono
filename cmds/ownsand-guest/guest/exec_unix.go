// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package guest

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// forceAfterTerminate: Linux and macOS guests send SIGTERM first and SIGKILL
// after KillGrace.
const forceAfterTerminate = true

// setProcessGroup starts the command in its own process group, so a timeout
// stops the command together with its children.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminate(p *os.Process) {
	_ = syscall.Kill(-p.Pid, syscall.SIGTERM)
}

func kill(p *os.Process) {
	_ = syscall.Kill(-p.Pid, syscall.SIGKILL)
}

// processGroupAlive reports whether the process group of p still has a
// member. The group id equals the process id of the command, which stays
// reserved while a member lives.
func processGroupAlive(p *os.Process) bool {
	return !errors.Is(syscall.Kill(-p.Pid, 0), syscall.ESRCH)
}

func exitStatus(state *os.ProcessState) (code int64, signal int32, signaled bool) {
	if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 0, int32(ws.Signal()), true
	}
	return int64(state.ExitCode()), 0, false
}
