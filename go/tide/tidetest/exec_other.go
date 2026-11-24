// SPDX-License-Identifier: Apache-2.0

//go:build !linux && !darwin

package tidetest

import (
	"context"
	"os/exec"
)

func newExecCommand(ctx context.Context, args []string) *exec.Cmd {
	return exec.CommandContext(ctx, args[0], args[1:]...)
}
