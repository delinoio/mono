// SPDX-License-Identifier: Apache-2.0

//go:build !linux && !darwin

package sandruntest

import (
	"context"
	"os/exec"
)

func configureExecCommand(context.Context, *exec.Cmd) {}
