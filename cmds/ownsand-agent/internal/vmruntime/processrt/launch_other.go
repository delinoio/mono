// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build !darwin && !linux

package processrt

import (
	"log/slog"
	"os/exec"
	"syscall"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/launcher"
	"github.com/delinoio/mono/internal/procgroup"
)

func startHelper(cmd *exec.Cmd, _ *slog.Logger) (helperProcess, func(), error) {
	procgroup.Set(cmd)
	if err := launcher.Start(cmd); err != nil {
		return helperProcess{}, nil, err
	}
	helper := helperProcess{
		pid:    cmd.Process.Pid,
		signal: func(sig syscall.Signal) error { return procgroup.Signal(cmd.Process, sig) },
	}
	return helper, func() { _ = cmd.Wait() }, nil
}
