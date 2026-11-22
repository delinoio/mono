// SPDX-License-Identifier: FSL-1.1-ALv2

package guestlisten

import "golang.org/x/sys/unix"

// socketCloexec creates a vsock stream socket that is close-on-exec from its
// creation, so no lock is needed.
func socketCloexec() (int, error) {
	return unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
}

func acceptNonblock(fd int) (int, unix.Sockaddr, error) {
	return unix.Accept4(fd, unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC)
}
