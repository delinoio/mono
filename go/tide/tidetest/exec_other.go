// SPDX-License-Identifier: Apache-2.0

//go:build !linux && !darwin

package tidetest

import "os/exec"

func configureExecCommand(*exec.Cmd) {}
