// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build !linux

package launcher

import "os/exec"

// start starts cmd directly. macOS VMs run inside the agent process and
// Windows VMs are bound through the Host Compute Service, so no parent-death
// signal is needed there.
func start(cmd *exec.Cmd) error {
	return cmd.Start()
}
