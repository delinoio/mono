// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package procgroup

import (
	"os"
	"os/exec"
	"syscall"
)

// Set makes cmd start as the leader of a new process group. It replaces
// cmd.SysProcAttr; call it before cmd.Start.
func Set(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// Signal sends sig to every process in the process group whose leader is
// p. A nil p is a no-op.
func Signal(p *os.Process, sig syscall.Signal) error {
	if p == nil {
		return nil
	}
	return syscall.Kill(-p.Pid, sig)
}
