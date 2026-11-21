// SPDX-License-Identifier: Apache-2.0

package procgroup

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// Set makes cmd start in a new process group. It replaces cmd.SysProcAttr;
// call it before cmd.Start.
func Set(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// Signal kills the process p for SIGKILL. Windows cannot signal a process
// group, so every other signal returns errors.ErrUnsupported. A nil p is a
// no-op.
func Signal(p *os.Process, sig syscall.Signal) error {
	if p == nil {
		return nil
	}
	if sig != syscall.SIGKILL {
		return errors.ErrUnsupported
	}
	return p.Kill()
}
