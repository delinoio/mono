// SPDX-License-Identifier: Apache-2.0

//go:build linux || darwin

package tidetest

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func newExecCommand(ctx context.Context, args []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// The command leads its own group. A negative PID signals every
		// process that stayed in that group.
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	return cmd
}
