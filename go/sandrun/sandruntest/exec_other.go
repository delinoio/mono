// SPDX-License-Identifier: Apache-2.0

//go:build !linux && !darwin

package sandruntest

import "os/exec"

func configureExecCommand(*exec.Cmd) {}
