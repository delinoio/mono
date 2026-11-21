// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build !windows

package guest

import (
	"os"
	"syscall"

	"github.com/delinoio/mono/internal/procgroup"
)

// forceAfterTerminate: Linux and macOS guests send SIGTERM first and SIGKILL
// after KillGrace.
const forceAfterTerminate = true

// terminate and kill signal the process group that procgroup.Set created,
// so a timeout stops the command together with its children.
func terminate(p *os.Process) {
	_ = procgroup.Signal(p, syscall.SIGTERM)
}

func kill(p *os.Process) {
	_ = procgroup.Signal(p, syscall.SIGKILL)
}

func exitStatus(state *os.ProcessState) (code int64, signal int32, signaled bool) {
	if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 0, int32(ws.Signal()), true
	}
	return int64(state.ExitCode()), 0, false
}
