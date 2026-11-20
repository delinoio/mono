// Package netpolicy computes the OUTBOUND network policy of Linux sandboxes:
// the blocked destination prefixes derived from the host addresses and
// routing table, and the nftables ruleset of the agent-owned tables. Rule
// generation is a pure function of its inputs, so tests cover it without
// root privileges.
package netpolicy

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"
)

// StaticBlockedV4 are the IPv4 ranges that every OUTBOUND guest is denied.
var StaticBlockedV4 = mustPrefixes(
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
	"172.16.0.0/12", "192.168.0.0/16", "224.0.0.0/4", "240.0.0.0/4",
)

// StaticBlockedV6 are the IPv6 ranges that every OUTBOUND guest is denied.
var StaticBlockedV6 = mustPrefixes(
	"::1/128", "fc00::/7", "fe80::/10", "ff00::/8", "64:ff9b:1::/48",
)

var (
	mappedPrefix = netip.MustParsePrefix("::ffff:0:0/96")
	nat64Prefix  = netip.MustParsePrefix("64:ff9b::/96")
)

func mustPrefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

// Address is a host interface address with its prefix length.
type Address struct {
	Interface string
	Prefix    netip.Prefix // address with the on-link prefix length
}

// Route is a host route that the host uses to reach a prefix.
type Route struct {
	Prefix    netip.Prefix
	Interface string
}

// Host is a snapshot of the host network state.
type Host struct {
	Addresses []Address
	Routes    []Route
}

// SandboxInterfacePrefix starts the name of every sandbox tap device. Their
// addresses and routes are the agent's own and are not blocked.
const SandboxInterfacePrefix = "osb"

func isSandboxInterface(name string) bool { return strings.HasPrefix(name, SandboxInterfacePrefix) }

// IsDefault reports whether p is a default route prefix.
func IsDefault(p netip.Prefix) bool { return p.Bits() == 0 }

// complementOfHalf returns the other /1 half of the address space.
func complementOfHalf(p netip.Prefix) netip.Prefix {
	b := p.Addr().AsSlice()
	b[0] ^= 0x80
	a, _ := netip.AddrFromSlice(b)
	return netip.PrefixFrom(a, 1)
}

// Blocked returns the blocked destination prefixes of each family, compacted
// so that no prefix contains another:
//   - the static ranges;
//   - every host address and its on-link prefix, on every interface except
//     the sandbox tap devices;
//   - every prefix that the host reaches through a route other than the
//     default route; a complementary pair of /1 routes through the same
//     interface counts as a default route (a full-tunnel VPN), and a single
//     /1 route without its complement is an ordinary route;
//   - the IPv4-mapped (::ffff:0:0/96) and NAT64 (64:ff9b::/96) forms of every
//     blocked IPv4 prefix.
func Blocked(h Host) (v4, v6 []netip.Prefix) {
	add := func(p netip.Prefix) {
		p = p.Masked()
		if p.Addr().Is4() || p.Addr().Is4In6() {
			p = netip.PrefixFrom(p.Addr().Unmap(), max(0, p.Bits()-boolInt(p.Addr().Is4In6())*96)).Masked()
			v4 = append(v4, p)
		} else {
			v6 = append(v6, p)
		}
	}
	for _, p := range StaticBlockedV4 {
		add(p)
	}
	for _, p := range StaticBlockedV6 {
		add(p)
	}
	for _, a := range h.Addresses {
		if isSandboxInterface(a.Interface) || !a.Prefix.IsValid() {
			continue
		}
		add(netip.PrefixFrom(a.Prefix.Addr(), a.Prefix.Addr().BitLen()))
		add(a.Prefix)
	}
	for _, r := range h.Routes {
		if isSandboxInterface(r.Interface) || !r.Prefix.IsValid() || IsDefault(r.Prefix) {
			continue
		}
		if r.Prefix.Bits() == 1 && hasRoute(h.Routes, complementOfHalf(r.Prefix.Masked()), r.Interface) {
			continue
		}
		add(r.Prefix)
	}
	for _, p := range compact(v4) {
		v6 = append(v6, embed(mappedPrefix, p), embed(nat64Prefix, p))
	}
	return compact(v4), compact(v6)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func hasRoute(routes []Route, p netip.Prefix, iface string) bool {
	for _, r := range routes {
		if r.Interface == iface && r.Prefix.Masked() == p {
			return true
		}
	}
	return false
}

// embed returns the IPv6 form of an IPv4 prefix inside a /96 IPv6 prefix.
func embed(base netip.Prefix, p netip.Prefix) netip.Prefix {
	b := base.Addr().As16()
	v4 := p.Addr().As4()
	copy(b[12:], v4[:])
	return netip.PrefixFrom(netip.AddrFrom16(b), 96+p.Bits()).Masked()
}

// compact sorts and removes prefixes that another prefix contains. Prefixes
// either nest or are disjoint, so the result is disjoint, which nftables
// interval sets require.
func compact(ps []netip.Prefix) []netip.Prefix {
	ps = slices.Clone(ps)
	slices.SortFunc(ps, func(a, b netip.Prefix) int {
		if c := a.Addr().Compare(b.Addr()); c != 0 {
			return c
		}
		return a.Bits() - b.Bits()
	})
	out := ps[:0]
	for _, p := range ps {
		if len(out) > 0 && out[len(out)-1].Overlaps(p) && out[len(out)-1].Bits() <= p.Bits() {
			continue
		}
		out = append(out, p)
	}
	return out
}

// Guest is the host side of one sandbox network.
type Guest struct {
	// Tap is the tap device name.
	Tap string
	// Gateway is the host address on the tap device (the guest gateway and
	// DNS server).
	Gateway netip.Addr
	// Subnet is the guest link prefix.
	Subnet netip.Prefix
}

// Config is the input of Ruleset.
type Config struct {
	// GuestRange covers every guest link; NAT applies to it.
	GuestRange netip.Prefix
	Blocked4   []netip.Prefix
	Blocked6   []netip.Prefix
}

// TableName is the agent-owned nftables table.
const TableName = "ownsand"

// Ruleset returns an nftables script that replaces the agent-owned table:
//
//   - input from a sandbox tap: only DNS (UDP and TCP 53) to the tap's
//     gateway address, DHCPv4 and DHCPv6, and ICMPv6 router and neighbor
//     discovery are accepted; everything else is dropped, so the host's own
//     addresses are unreachable;
//   - forwarding from a sandbox tap: blocked destinations, other sandbox
//     taps and DHCP leaving the link are dropped; the rest is accepted;
//   - forwarding to a sandbox tap: only established and related traffic, so
//     inbound connections to guests are not allowed;
//   - outbound NAT (masquerade) for the guest range.
//
// The script is applied with `nft -f -`; the leading add and delete make it
// idempotent.
func Ruleset(c Config) string {
	var b strings.Builder
	tap := SandboxInterfacePrefix + "*"
	fmt.Fprintf(&b, "add table inet %s\ndelete table inet %s\n", TableName, TableName)
	fmt.Fprintf(&b, "table inet %s {\n", TableName)
	writeSet(&b, "blocked4", "ipv4_addr", c.Blocked4)
	writeSet(&b, "blocked6", "ipv6_addr", c.Blocked6)
	fmt.Fprintf(&b, "\tchain input {\n\t\ttype filter hook input priority filter - 10; policy accept;\n")
	fmt.Fprintf(&b, "\t\tiifname != %q accept\n", tap)
	fmt.Fprintf(&b, "\t\tct state established,related accept\n")
	fmt.Fprintf(&b, "\t\tfib daddr . iif type local udp dport 53 accept\n")
	fmt.Fprintf(&b, "\t\tfib daddr . iif type local tcp dport 53 accept\n")
	fmt.Fprintf(&b, "\t\tudp sport 68 udp dport 67 accept\n")
	fmt.Fprintf(&b, "\t\tudp sport 546 udp dport 547 accept\n")
	fmt.Fprintf(&b, "\t\ticmpv6 type { nd-router-solicit, nd-neighbor-solicit, nd-neighbor-advert } accept\n")
	fmt.Fprintf(&b, "\t\tdrop\n\t}\n")
	fmt.Fprintf(&b, "\tchain forward {\n\t\ttype filter hook forward priority filter - 10; policy accept;\n")
	fmt.Fprintf(&b, "\t\toifname %q ct state established,related accept\n", tap)
	fmt.Fprintf(&b, "\t\toifname %q drop\n", tap)
	fmt.Fprintf(&b, "\t\tiifname %q ip daddr @blocked4 drop\n", tap)
	fmt.Fprintf(&b, "\t\tiifname %q ip6 daddr @blocked6 drop\n", tap)
	fmt.Fprintf(&b, "\t\tiifname %q ip daddr 255.255.255.255 drop\n", tap)
	fmt.Fprintf(&b, "\t\tiifname %q accept\n\t}\n", tap)
	fmt.Fprintf(&b, "\tchain output {\n\t\ttype filter hook output priority filter - 10; policy accept;\n")
	fmt.Fprintf(&b, "\t\toifname %q ct state established,related accept\n", tap)
	fmt.Fprintf(&b, "\t\toifname %q udp sport { 53, 67, 547 } accept\n", tap)
	fmt.Fprintf(&b, "\t\toifname %q tcp sport 53 accept\n", tap)
	fmt.Fprintf(&b, "\t\toifname %q icmpv6 type { nd-router-advert, nd-neighbor-solicit, nd-neighbor-advert } accept\n", tap)
	fmt.Fprintf(&b, "\t\toifname %q drop\n\t}\n", tap)
	if c.GuestRange.IsValid() {
		fmt.Fprintf(&b, "\tchain postrouting {\n\t\ttype nat hook postrouting priority srcnat; policy accept;\n")
		fmt.Fprintf(&b, "\t\tip saddr %s oifname != %q masquerade\n\t}\n", c.GuestRange.Masked(), tap)
	}
	b.WriteString("}\n")
	return b.String()
}

func writeSet(b *strings.Builder, name, typ string, ps []netip.Prefix) {
	fmt.Fprintf(b, "\tset %s {\n\t\ttype %s\n\t\tflags interval\n", name, typ)
	if len(ps) > 0 {
		elems := make([]string, len(ps))
		for i, p := range ps {
			elems[i] = p.String()
		}
		fmt.Fprintf(b, "\t\telements = { %s }\n", strings.Join(elems, ", "))
	}
	b.WriteString("\t}\n")
}
