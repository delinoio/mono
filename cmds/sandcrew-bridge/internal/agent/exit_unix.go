// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package agent

import (
	"os"
	"syscall"
)

func exitOf(state *os.ProcessState) Exit {
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return Exit{Code: -1, Signal: status.Signal().String()}
	}
	return Exit{Code: state.ExitCode()}
}
