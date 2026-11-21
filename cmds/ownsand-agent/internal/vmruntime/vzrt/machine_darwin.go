// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build darwin && cgo

package vzrt

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/Code-Hex/vz/v3"
)

// MacOSMachine describes a macOS guest VM.
type MacOSMachine struct {
	// Disk is the raw disk image and AuxStorage the auxiliary storage
	// (NVRAM) of the guest.
	Disk       string
	AuxStorage string
	// HardwareModel is the opaque hardware model of the guest.
	HardwareModel []byte
	// MachineID is the machine identifier to use; nil creates a new one.
	MachineID []byte
	CPUs      int32
	MemoryMiB int64
	// Network is the network device of an OUTBOUND guest, or nil.
	Network *vz.VirtioNetworkDeviceConfiguration
}

// WatchStopped returns a channel that is closed when machine stops or fails.
func WatchStopped(machine *vz.VirtualMachine) <-chan struct{} {
	exited := make(chan struct{})
	notify := machine.StateChangedNotify()
	go func() {
		for state := range notify {
			if state == vz.VirtualMachineStateStopped || state == vz.VirtualMachineStateError {
				close(exited)
				return
			}
		}
	}()
	return exited
}

// DialGuest connects to a vsock port of the guest through the virtio socket
// device of machine. It gives up when ctx ends, after dialTimeout or when
// exited is closed.
func DialGuest(ctx context.Context, machine *vz.VirtualMachine, port uint32, exited <-chan struct{}) (net.Conn, error) {
	devices := machine.SocketDevices()
	if len(devices) == 0 {
		return nil, errors.New("vzrt: the VM has no socket device")
	}
	type result struct {
		conn net.Conn
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		c, err := devices[0].Connect(port)
		if err != nil {
			ch <- result{nil, err}
			return
		}
		ch <- result{c, nil}
	}()
	timer := time.NewTimer(dialTimeout)
	defer timer.Stop()
	select {
	case res := <-ch:
		return res.conn, res.err
	case <-ctx.Done():
	case <-timer.C:
	case <-exited:
	}
	// Close a connection that completes after the caller gave up.
	go func() {
		if res := <-ch; res.conn != nil {
			res.conn.Close()
		}
	}()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return nil, errors.New("vzrt: connect to the guest helper timed out")
}
