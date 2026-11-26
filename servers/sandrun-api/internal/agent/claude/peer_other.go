// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build !linux

package claude

import "net"

// checkPeer: outside Linux (local development on macOS), only the
// permissions of the run directory (0700) and of the socket (0600) limit
// the hook socket to processes of the same user.
func checkPeer(*net.UnixConn) error { return nil }
