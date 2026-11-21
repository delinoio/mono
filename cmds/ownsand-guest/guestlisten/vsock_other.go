// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build !linux && !darwin

package guestlisten

import (
	"fmt"
	"net"
)

func listenVsock(uint32) (net.Listener, error) {
	return nil, fmt.Errorf("%w: vsock", ErrUnsupported)
}
