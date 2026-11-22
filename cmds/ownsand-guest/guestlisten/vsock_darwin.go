// SPDX-License-Identifier: FSL-1.1-ALv2

package guestlisten

import "golang.org/x/sys/unix"

// socketCloexec creates a vsock stream socket. macOS has no SOCK_CLOEXEC, so
// the socket is marked close-on-exec under syscall.ForkLock (see
// createCloexec).
func socketCloexec() (int, error) {
	return createCloexec(func() (int, error) {
		return unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	}, setCloseOnExec)
}

// acceptNonblock accepts a connection. macOS has no accept4, so the
// connection is marked close-on-exec under syscall.ForkLock right after the
// accept (see createCloexec) and made non-blocking after the lock is
// released. The listener must be non-blocking, because the lock is held for
// reading during the accept.
func acceptNonblock(fd int) (int, unix.Sockaddr, error) {
	var sa unix.Sockaddr
	nfd, err := createCloexec(func() (int, error) {
		nfd, peer, err := unix.Accept(fd)
		sa = peer
		return nfd, err
	}, setCloseOnExec)
	if err != nil {
		return -1, nil, err
	}
	if err := unix.SetNonblock(nfd, true); err != nil {
		unix.Close(nfd)
		return -1, nil, err
	}
	return nfd, sa, nil
}
