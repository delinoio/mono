// SPDX-License-Identifier: FSL-1.1-ALv2

package guestlisten

import "golang.org/x/sys/unix"

// acceptNonblock accepts a connection. macOS has no accept4, so the flags
// are set after the accept.
func acceptNonblock(fd int) (int, unix.Sockaddr, error) {
	nfd, sa, err := unix.Accept(fd)
	if err != nil {
		return -1, nil, err
	}
	unix.CloseOnExec(nfd)
	if err := unix.SetNonblock(nfd, true); err != nil {
		unix.Close(nfd)
		return -1, nil, err
	}
	return nfd, sa, nil
}
