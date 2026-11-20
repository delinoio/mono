// SPDX-License-Identifier: Apache-2.0

//go:build linux || darwin

package processrt

import (
	"os/exec"
	"runtime"
	"syscall"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

const supported = true

var (
	sigStop = syscall.SIGSTOP
	sigCont = syscall.SIGCONT
	sigKill = syscall.SIGKILL
)

var hostGuestOS, hostBaseImage = func() (ownsandv1.GuestOs, ownsandv1.BaseImage) {
	if runtime.GOOS == "darwin" {
		return ownsandv1.GuestOs_GUEST_OS_MACOS, ownsandv1.BaseImage_BASE_IMAGE_MACOS
	}
	return ownsandv1.GuestOs_GUEST_OS_LINUX, ownsandv1.BaseImage_BASE_IMAGE_LINUX
}()

func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func signalGroup(cmd *exec.Cmd, sig syscall.Signal) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, sig)
}
