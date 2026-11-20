//go:build !linux

package update

import (
	"errors"

	"connectrpc.com/connect"
)

// InstallBinary is not implemented on macOS and Windows: the macOS binary
// must be code-signed and notarized, and Windows cannot replace a running
// executable; both need the service-manager integration of the release
// process.
func InstallBinary(string) error {
	return connect.NewError(connect.CodeUnimplemented, errors.New("agent update installation is not implemented on this host OS"))
}
