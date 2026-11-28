// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package agent

import (
	"errors"
	"os"
	"syscall"
)

// groupAlive reports whether the process group whose leader is p still
// has a process. A process that is not reaped yet counts.
func groupAlive(p *os.Process) bool {
	err := syscall.Kill(-p.Pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func exitOf(state *os.ProcessState) Exit {
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return Exit{Code: -1, Signal: status.Signal().String()}
	}
	return Exit{Code: state.ExitCode()}
}
