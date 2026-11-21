// SPDX-License-Identifier: Apache-2.0

//go:build !linux && !darwin && !windows

package netpolicy

import (
	"errors"
	"net/netip"
)

// HostRoutes fails on host OSes without a route reader.
func HostRoutes() ([]Route, []netip.Prefix, error) {
	return nil, nil, errors.New("netpolicy: reading the host routes is not supported on this host OS")
}
