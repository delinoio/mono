// SPDX-License-Identifier: Apache-2.0

//go:build !linux && !darwin

package sandruntest

import (
	"context"
	"os"
	"os/exec"
)

type execGroup struct {
	process *os.Process
}

func (group *execGroup) start(_ context.Context, cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	group.process = cmd.Process
	return nil
}

func (group *execGroup) cancel(bool) error { return group.process.Kill() }

func (group *execGroup) close() {}

func (group *execGroup) release() {}
