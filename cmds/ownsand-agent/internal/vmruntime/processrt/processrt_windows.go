// SPDX-License-Identifier: Apache-2.0

package processrt

import (
	"os/exec"
	"syscall"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// The process runtime needs process-group signals (pause, resume, kill),
// which Windows does not provide; New fails on Windows.
const supported = false

var (
	sigStop = syscall.Signal(0)
	sigCont = syscall.Signal(0)
	sigKill = syscall.SIGKILL
)

var hostGuestOS, hostBaseImage = ownsandv1.GuestOs_GUEST_OS_WINDOWS, ownsandv1.BaseImage_BASE_IMAGE_WINDOWS

func setProcessGroup(*exec.Cmd) {}

func signalGroup(cmd *exec.Cmd, sig syscall.Signal) error {
	if cmd == nil || cmd.Process == nil || sig != sigKill {
		return nil
	}
	return cmd.Process.Kill()
}
