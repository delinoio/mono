// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build !windows

package guest

import (
	"errors"
	"os"
	"syscall"

	"github.com/delinoio/mono/internal/procgroup"
)

// forceAfterTerminate: Linux and macOS guests send SIGTERM first and SIGKILL
// after KillGrace.
const forceAfterTerminate = true

// envKeyFold: environment variable names are case-sensitive on Unix.
func envKeyFold(key string) string { return key }

// terminate and kill signal the process group that procgroup.Set created,
// so a timeout stops the command together with its children.
func terminate(p *os.Process) {
	_ = procgroup.Signal(p, syscall.SIGTERM)
}

func kill(p *os.Process) {
	_ = procgroup.Signal(p, syscall.SIGKILL)
}

// processGroupAlive reports whether the process group of p still has a
// member. The group id equals the process id of the command, which stays
// reserved while a member lives.
func processGroupAlive(p *os.Process) bool {
	return !errors.Is(syscall.Kill(-p.Pid, 0), syscall.ESRCH)
}

// processExited reports whether the direct child was already reaped by
// cmd.Wait, which happens before the output pipes are drained.
func processExited(p *os.Process) bool {
	return errors.Is(p.Signal(syscall.Signal(0)), os.ErrProcessDone)
}

func exitStatus(state *os.ProcessState) (code int64, signal int32, signaled bool) {
	if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 0, int32(ws.Signal()), true
	}
	return int64(state.ExitCode()), 0, false
}
