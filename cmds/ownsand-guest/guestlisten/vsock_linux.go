// SPDX-License-Identifier: FSL-1.1-ALv2

package guestlisten

import "golang.org/x/sys/unix"

func acceptNonblock(fd int) (int, unix.Sockaddr, error) {
	return unix.Accept4(fd, unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC)
}
