//go:build windows

package guest

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

// prepareProcess starts the command in a new process group so console
// control events of the helper do not reach it.
func prepareProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// terminateProcess terminates the process tree of the command.
//
// Workaround: the standard library has no job object API, and the approved
// dependencies do not include golang.org/x/sys/windows, so the helper runs
// the built-in taskkill tool with /T (tree) and /F (force). Windows has no
// SIGTERM, so the grace period does not apply. Remove this when the guest
// helper starts each exec in a job object (with
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE) through an approved Windows binding.
func terminateProcess(cmd *exec.Cmd, _ time.Duration) {
	if cmd.Process == nil {
		return
	}
	kill := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
	if err := kill.Run(); err != nil {
		_ = cmd.Process.Kill()
	}
}

// exitSignal reports no signal: Windows processes end with an exit code.
func exitSignal(*os.ProcessState) (int, bool) { return 0, false }
