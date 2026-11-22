// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build !linux

package processrt

import (
	"fmt"
	"os"
	"path/filepath"
)

// maxSocketPath is the longest unix socket path that bind and connect accept
// on macOS: sun_path has 104 bytes, including the terminating NUL.
const maxSocketPath = 103

// newSocket returns the socket path of the guest helper of a sandbox and the
// private directory (mode 0700) that holds it. macOS has no abstract unix
// sockets, and a socket in the sandbox directory can exceed the macOS limit
// of unix socket paths: the sandbox directory is below the agent state
// directory, which can be deep, and below a long per-user TMPDIR in tests.
// The directory is a new directory in the system temporary directory (TMPDIR
// on macOS, which is short and private to the user by default), so the path
// stays short whatever the state directory is. The caller removes the
// directory with the sandbox.
func newSocket(string) (address, dir string, err error) {
	dir, err = os.MkdirTemp("", "osb-")
	if err != nil {
		return "", "", fmt.Errorf("processrt: create guest helper socket directory: %w", err)
	}
	// A relative TMPDIR would make the address resolve against the working
	// directory of the caller, while the helper starts in the sandbox root.
	abs, err := filepath.Abs(dir)
	if err != nil {
		_ = os.Remove(dir)
		return "", "", fmt.Errorf("processrt: resolve guest helper socket directory: %w", err)
	}
	dir = abs
	address = filepath.Join(dir, "g.sock")
	if len(address) > maxSocketPath {
		_ = os.Remove(dir)
		return "", "", fmt.Errorf("processrt: guest helper socket path has %d bytes, more than the %d-byte unix socket path limit; set TMPDIR to a shorter directory", len(address), maxSocketPath)
	}
	return address, dir, nil
}
