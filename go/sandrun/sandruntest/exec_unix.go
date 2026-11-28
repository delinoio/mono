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

// execGroup owns the process group until the exec finishes draining its output.
// guest.mu serializes startup and signals for an active group.
type execGroup struct {
	anchor   *exec.Cmd
	hold     *os.File
	process  *os.Process
	signaled bool
}

func (group *execGroup) start(ctx context.Context, cmd *exec.Cmd) error {
	if cmd.Err != nil || ctx.Err() != nil {
		return cmd.Start()
	}
	read, write, err := os.Pipe()
	if err != nil {
		return err
	}
	defer read.Close()

	// Start the anchor first so even a fast-exiting command joins an
	// owned group. The anchor waits on its own pipe and uses no exec output
	// descriptors. Its PID stays reserved until release reaps it.
	anchor := exec.Command("/bin/cat")
	anchor.Stdin = read
	anchor.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := anchor.Start(); err != nil {
		_ = write.Close()
		return err
	}
	group.anchor, group.hold = anchor, write
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: anchor.Process.Pid}
	if err := cmd.Start(); err != nil {
		return err
	}
	group.process = cmd.Process
	return nil
}

func (group *execGroup) close() { _ = group.cancel(false) }

func (group *execGroup) cancel(deadline bool) error {
	if group.signaled {
		return os.ErrProcessDone
	}
	// A descendant can keep a pipe open after the direct child exits.
	// Skip the deadline signal even if Wait has yet to reap the child.
	process := group.process
	if deadline && (errors.Is(process.Signal(syscall.Signal(0)), os.ErrProcessDone) || execChildExited(process.Pid)) {
		return os.ErrProcessDone
	}
	err := syscall.Kill(-group.anchor.Process.Pid, syscall.SIGKILL)
	// Also stop the direct child if it moved out of the group.
	directErr := process.Kill()
	group.signaled = err == nil || errors.Is(err, syscall.ESRCH)
	if errors.Is(err, syscall.ESRCH) {
		return directErr
	}
	return err
}

// release runs after the caller disarms both paths that can signal the group.
func (group *execGroup) release() {
	if group.anchor != nil {
		_ = group.hold.Close()
		// Reap promptly even if the anchor is stopped and cannot read EOF.
		_ = group.anchor.Process.Kill()
		_ = group.anchor.Wait()
	}
}
