// SPDX-License-Identifier: FSL-1.1-ALv2

package netpolicy

import (
	"net/netip"
	"strconv"
	"unsafe"

	"golang.org/x/sys/windows"
)

// HostRoutes reads the IPv4 and IPv6 routes of the host routing table
// (GetIpForwardTable2) and the interface prefixes.
func HostRoutes() ([]Route, []netip.Prefix, error) {
	var table *windows.MibIpForwardTable2
	if err := windows.GetIpForwardTable2(windows.AF_UNSPEC, &table); err != nil {
		return nil, nil, err
	}
	defer windows.FreeMibTable(unsafe.Pointer(table))
	var routes []Route
	for _, row := range table.Rows() {
		if row.Loopback != 0 {
			continue
		}
		if p, ok := sockaddrPrefix(&row.DestinationPrefix.Prefix, int(row.DestinationPrefix.PrefixLength)); ok {
			// The interface index names the interface for the /1 pair
			// rule.
			routes = append(routes, Route{Prefix: p, Interface: strconv.FormatUint(uint64(row.InterfaceIndex), 10)})
		}
	}
	onLink, err := InterfacePrefixes(nil)
	if err != nil {
		return nil, nil, err
	}
	return routes, onLink, nil
}

func sockaddrPrefix(sa *windows.RawSockaddrInet, bits int) (netip.Prefix, bool) {
	switch sa.Family {
	case windows.AF_INET:
		in := (*windows.RawSockaddrInet4)(unsafe.Pointer(sa))
		if bits > 32 {
			return netip.Prefix{}, false
		}
		return netip.PrefixFrom(netip.AddrFrom4(in.Addr), bits).Masked(), true
	case windows.AF_INET6:
		in := (*windows.RawSockaddrInet6)(unsafe.Pointer(sa))
		if bits > 128 {
			return netip.Prefix{}, false
		}
		return netip.PrefixFrom(netip.AddrFrom16(in.Addr), bits).Masked(), true
	}
	return netip.Prefix{}, false
}
