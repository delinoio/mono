// SPDX-License-Identifier: FSL-1.1-ALv2

package netpolicy

import (
	"math/bits"
	"net"
	"net/netip"
	"strconv"
	"syscall"

	"golang.org/x/net/route"
	"golang.org/x/sys/unix"
)

// skippedRouteFlags mark routing table entries that are not routes of the
// policy: entries that the kernel cloned from another route (on macOS,
// every remote destination that the host talked to gets a cloned host
// route through the default route), link-layer (ARP and neighbor) entries,
// and broadcast and multicast entries.
const skippedRouteFlags = unix.RTF_WASCLONED | unix.RTF_LLINFO | unix.RTF_BROADCAST | unix.RTF_MULTICAST

// HostRoutes reads the IPv4 and IPv6 routes of the host routing table
// (sysctl NET_RT_DUMP) and the interface prefixes.
func HostRoutes() ([]Route, []netip.Prefix, error) {
	rib, err := route.FetchRIB(syscall.AF_UNSPEC, route.RIBTypeRoute, 0)
	if err != nil {
		return nil, nil, err
	}
	msgs, err := route.ParseRIB(route.RIBTypeRoute, rib)
	if err != nil {
		return nil, nil, err
	}
	var routes []Route
	for _, m := range msgs {
		rm, ok := m.(*route.RouteMessage)
		if !ok || rm.Flags&unix.RTF_UP == 0 || rm.Flags&skippedRouteFlags != 0 {
			continue
		}
		if p, ok := routePrefix(rm); ok {
			routes = append(routes, Route{Prefix: p, Interface: interfaceName(rm.Index)})
		}
	}
	onLink, err := InterfacePrefixes(nil)
	if err != nil {
		return nil, nil, err
	}
	return routes, onLink, nil
}

// routePrefix returns the destination prefix of a route message: the
// destination with its netmask, or a host prefix for a host route.
func routePrefix(rm *route.RouteMessage) (netip.Prefix, bool) {
	if len(rm.Addrs) <= unix.RTAX_DST {
		return netip.Prefix{}, false
	}
	var mask route.Addr
	if len(rm.Addrs) > unix.RTAX_NETMASK {
		mask = rm.Addrs[unix.RTAX_NETMASK]
	}
	switch dst := rm.Addrs[unix.RTAX_DST].(type) {
	case *route.Inet4Addr:
		addr := netip.AddrFrom4(dst.IP)
		ones := 32
		if rm.Flags&unix.RTF_HOST == 0 {
			ones = 0
			if m, ok := mask.(*route.Inet4Addr); ok {
				ones = maskOnes(m.IP[:])
			}
		}
		return netip.PrefixFrom(addr, ones).Masked(), true
	case *route.Inet6Addr:
		addr := netip.AddrFrom16(dst.IP)
		ones := 128
		if rm.Flags&unix.RTF_HOST == 0 {
			ones = 0
			if m, ok := mask.(*route.Inet6Addr); ok {
				ones = maskOnes(m.IP[:])
			}
		}
		return netip.PrefixFrom(addr, ones).Masked(), true
	}
	return netip.Prefix{}, false
}

// maskOnes counts the leading one bits of a netmask; a mask with a gap
// counts up to its first zero bit.
func maskOnes(mask []byte) int {
	n := 0
	for _, b := range mask {
		if b == 0xff {
			n += 8
			continue
		}
		return n + bits.LeadingZeros8(^b)
	}
	return n
}

func interfaceName(index int) string {
	if iface, err := net.InterfaceByIndex(index); err == nil {
		return iface.Name
	}
	return strconv.Itoa(index)
}
