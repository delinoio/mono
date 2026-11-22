// SPDX-License-Identifier: FSL-1.1-ALv2

package claude

import (
	"errors"
	"net"
	"os"
	"syscall"
)

// checkPeer accepts a hook connection only from a process of the same
// user as tide-api (SO_PEERCRED). The socket is a file in a directory that
// only that user can open, so no network peer and no other user reaches
// it; the check makes the rule explicit.
func checkPeer(c *net.UnixConn) error {
	raw, err := c.SyscallConn()
	if err != nil {
		return err
	}
	var cred *syscall.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return err
	}
	if credErr != nil {
		return credErr
	}
	if int(cred.Uid) != os.Getuid() {
		return errors.New("the hook process runs as another user")
	}
	return nil
}
