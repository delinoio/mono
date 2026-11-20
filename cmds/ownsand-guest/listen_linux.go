//go:build linux

package main

import (
	"net"

	"github.com/delinoio/mono/cmds/ownsand-guest/vsock"
)

// listenHypervisorSocket listens on vsock on Firecracker-class microVMs.
func listenHypervisorSocket(port uint32) (net.Listener, error) {
	return vsock.Listen(port)
}
