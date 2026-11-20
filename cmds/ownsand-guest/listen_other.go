//go:build !linux

package main

import (
	"errors"
	"net"
)

// listenHypervisorSocket is not implemented for macOS (virtio socket) and
// Windows (Hyper-V socket) guests yet: those guests are unsupported in this
// version.
func listenHypervisorSocket(uint32) (net.Listener, error) {
	return nil, errors.New("the hypervisor socket listener is not implemented on this guest OS")
}
