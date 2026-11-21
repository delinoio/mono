// SPDX-License-Identifier: FSL-1.1-ALv2

package netpolicy

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"io"
	"net/netip"
	"strconv"
	"strings"
)

// ipRouteType is the optional route type keyword that `ip route` prints
// before the prefix of a route that is not a plain unicast route.
type ipRouteType string

const (
	ipRouteUnicast     ipRouteType = "unicast"
	ipRouteLocal       ipRouteType = "local"
	ipRouteBroadcast   ipRouteType = "broadcast"
	ipRouteMulticast   ipRouteType = "multicast"
	ipRouteAnycast     ipRouteType = "anycast"
	ipRouteBlackhole   ipRouteType = "blackhole"
	ipRouteUnreachable ipRouteType = "unreachable"
	ipRouteProhibit    ipRouteType = "prohibit"
	ipRouteThrow       ipRouteType = "throw"
	ipRouteNAT         ipRouteType = "nat"
	ipRouteXResolve    ipRouteType = "xresolve"
)

// isKnown reports whether t is a type keyword of `ip route`.
func (t ipRouteType) isKnown() bool {
	switch t {
	case ipRouteUnicast, ipRouteLocal, ipRouteBroadcast, ipRouteMulticast, ipRouteAnycast,
		ipRouteBlackhole, ipRouteUnreachable, ipRouteProhibit, ipRouteThrow, ipRouteNAT, ipRouteXResolve:
		return true
	}
	return false
}

// forwards reports whether a route of type t sends packets to a next hop or
// on-link destination. The host cannot reach a prefix through any other
// type, so such a route adds no prefix: local, broadcast and anycast routes
// name the host's own addresses (the input chain drops those), a multicast
// route is covered by the fixed multicast range, and blackhole,
// unreachable, prohibit and throw routes deliver nothing (a throw route
// only falls through to the next routing table).
func (t ipRouteType) forwards() bool { return t == ipRouteUnicast }

// mainRouteTable is the table name that ParseIPRouteShow assigns to a route
// that `ip route show table all` prints without a table field, which is how
// iproute2 prints the main table.
const mainRouteTable = "main"

// ParseIPRouteShow parses the output of `ip -4 -o route show table all`: one
// route per line, an optional type keyword, the prefix (`default`, a
// prefix or a bare host address) and key-value attributes such as `dev` and
// `table`. It returns the unicast routes of every routing table, the main
// table, the tables that routing policy rules select and the tables that no
// rule selects, so that a prefix that the host reaches only through a policy
// routing table is part of the block set. Route.Table names the table
// (`main` when the line has no `table` field); Route.Interface is the first
// `dev` of the line, also for a multipath route. An unparsable prefix is an
// error, because a silently skipped route would leave its prefix open.
func ParseIPRouteShow(r io.Reader) ([]Route, error) {
	var out []Route
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 0 {
			continue
		}
		typ := ipRouteUnicast
		if t := ipRouteType(f[0]); t.isKnown() {
			typ = t
			f = f[1:]
			if len(f) == 0 {
				return nil, fmt.Errorf("netpolicy: route line %q has no prefix", sc.Text())
			}
		}
		var prefix netip.Prefix
		switch dst := f[0]; {
		case dst == "default":
			prefix = netip.PrefixFrom(netip.IPv4Unspecified(), 0)
		case strings.Contains(dst, "/"):
			p, err := netip.ParsePrefix(dst)
			if err != nil || !p.Addr().Is4() {
				return nil, fmt.Errorf("netpolicy: route prefix %q", dst)
			}
			prefix = p.Masked()
		default:
			a, err := netip.ParseAddr(dst)
			if err != nil || !a.Is4() {
				return nil, fmt.Errorf("netpolicy: route prefix %q", dst)
			}
			prefix = netip.PrefixFrom(a, 32)
		}
		if !typ.forwards() {
			continue
		}
		route := Route{Prefix: prefix, Table: mainRouteTable}
		for i := 1; i+1 < len(f); i++ {
			switch f[i] {
			case "dev":
				if route.Interface == "" {
					route.Interface = f[i+1]
				}
			case "table":
				route.Table = f[i+1]
			}
		}
		out = append(out, route)
	}
	return out, sc.Err()
}

// ParseProcIPv6Route parses /proc/net/ipv6_route.
func ParseProcIPv6Route(r io.Reader) ([]Route, error) {
	var out []Route
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 10 {
			continue
		}
		raw, err := hex.DecodeString(f[0])
		if err != nil || len(raw) != 16 {
			return nil, fmt.Errorf("netpolicy: ipv6 route destination %q", f[0])
		}
		bits, err := strconv.ParseUint(f[1], 16, 8)
		if err != nil {
			return nil, fmt.Errorf("netpolicy: ipv6 route length %q", f[1])
		}
		iface := f[9]
		if iface == "lo" {
			// Local and multicast routes of the loopback table: the host's
			// own addresses are dropped in the input chain.
			continue
		}
		out = append(out, Route{Prefix: netip.PrefixFrom(netip.AddrFrom16([16]byte(raw)), int(bits)).Masked(), Interface: iface})
	}
	return out, sc.Err()
}
