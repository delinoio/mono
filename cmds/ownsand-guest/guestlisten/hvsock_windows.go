// SPDX-License-Identifier: FSL-1.1-ALv2

package guestlisten

import (
	"fmt"
	"net"

	"github.com/Microsoft/go-winio"
	"github.com/Microsoft/go-winio/pkg/guid"
)

func init() { listenHvsock = listenHvsockWindows }

// listenHvsockWindows listens on the Hyper-V socket service ID of address
// for connections from any partition (the wildcard VM ID), which inside a
// guest means the host. The Hyper-V runtime connects to the service ID
// that winio.VsockServiceID(1024) gives, so a Windows guest listens on
// hvsock://00000400-facb-11e6-bd58-64006a7986d3.
func listenHvsockWindows(address string) (net.Listener, error) {
	service, err := guid.FromString(address)
	if err != nil {
		return nil, fmt.Errorf("guestlisten: invalid Hyper-V socket service ID %q: %w", address, err)
	}
	l, err := winio.ListenHvsock(&winio.HvsockAddr{VMID: winio.HvsockGUIDWildcard(), ServiceID: service})
	if err != nil {
		return nil, fmt.Errorf("guestlisten: Hyper-V socket listen on %s: %w", service, err)
	}
	return l, nil
}
