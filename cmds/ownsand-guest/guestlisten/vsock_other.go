// SPDX-License-Identifier: Apache-2.0

//go:build !linux && !darwin

package guestlisten

import (
	"fmt"
	"net"
)

func listenVsock(uint32) (net.Listener, error) {
	return nil, fmt.Errorf("%w: vsock", ErrUnsupported)
}
