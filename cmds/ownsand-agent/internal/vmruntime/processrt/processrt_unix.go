// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build linux || darwin

package processrt

import (
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
