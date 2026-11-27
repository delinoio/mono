// SPDX-License-Identifier: Apache-2.0

//go:build linux || darwin

package sandruntest

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func configureExecCommand(ctx context.Context, cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return cancelExecGroup(cmd.Process, errors.Is(ctx.Err(), context.DeadlineExceeded))
	}
}

func cancelExecGroup(process *os.Process, deadline bool) error {
	// A descendant can keep a pipe open after the direct child exits.
	// Skip the deadline signal even if Wait has yet to reap the child.
	if deadline && (errors.Is(process.Signal(syscall.Signal(0)), os.ErrProcessDone) || execChildExited(process.Pid)) {
		return os.ErrProcessDone
	}
	// The command leads its own group. A negative PID signals every
	// process that stayed in that group.
	err := syscall.Kill(-process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}
