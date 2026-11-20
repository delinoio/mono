// SPDX-License-Identifier: Apache-2.0

package guest

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

// forceAfterTerminate: Windows guests terminate the process tree at once.
const forceAfterTerminate = false

func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// terminate ends the process tree with taskkill, which every supported
// Windows edition includes. A job object would also catch children that
// detach from the tree; replace this with a job object when the Windows
// guest image is implemented and verified.
func terminate(p *os.Process) {
	_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(p.Pid)).Run()
	_ = p.Kill()
}

func kill(p *os.Process) {
	_ = p.Kill()
}

// exitStatus keeps the full 32-bit exit code; Windows has no signals.
func exitStatus(state *os.ProcessState) (code int64, signal int32, signaled bool) {
	if ws, ok := state.Sys().(syscall.WaitStatus); ok {
		return int64(ws.ExitCode), 0, false
	}
	return int64(state.ExitCode()), 0, false
}
