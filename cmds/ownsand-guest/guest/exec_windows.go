// SPDX-License-Identifier: Apache-2.0

package guest

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// forceAfterTerminate: Windows guests terminate the process tree at once.
const forceAfterTerminate = false

// envKeyFold: Windows environment variable names are case-insensitive. Go's
// os/exec folds with strings.ToLower when it removes duplicates, so the merge
// must fold the same way.
func envKeyFold(key string) string { return strings.ToLower(key) }

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

// processGroupAlive is never consulted on Windows because forceAfterTerminate
// is false; taskkill ends the tree at once.
func processGroupAlive(*os.Process) bool { return false }

// exitStatus keeps the full 32-bit exit code; Windows has no signals.
func exitStatus(state *os.ProcessState) (code int64, signal int32, signaled bool) {
	if ws, ok := state.Sys().(syscall.WaitStatus); ok {
		return int64(ws.ExitCode), 0, false
	}
	return int64(state.ExitCode()), 0, false
}
