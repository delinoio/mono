// SPDX-License-Identifier: FSL-1.1-ALv2

// Package netpolicy computes the OUTBOUND network policy of sandbox guests:
// the destination prefixes that guests must never reach (the fixed private,
// loopback, link-local, multicast and reserved ranges with their
// IPv4-mapped and NAT64 forms, and every on-link or non-default-route
// prefix of the host routing table, where a complementary /1 pair through
// one interface of one routing table counts as a default route), the per-sandbox addresses, and
// the nftables ruleset that enforces it on Linux. The rule generation is
// pure Go so that it is tested on every host.
package netpolicy

import (
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
)

// TapPrefix starts the name of every sandbox tap device; nftables rules
// match it.
const TapPrefix = "osb-"

// TableName is the nftables table that the agent owns.
const TableName = "ownsand"

// GuestSubnet holds the /30 link of every sandbox (gateway .1, guest .2).
var GuestSubnet = netip.MustParsePrefix("172.30.0.0/16")

// staticBlocked4 are the IPv4 ranges of the agent contract.
var staticBlocked4 = []string{
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
	"172.16.0.0/12", "192.168.0.0/16", "224.0.0.0/4", "240.0.0.0/4",
}

// staticBlocked6 are the IPv6 ranges of the agent contract.
var staticBlocked6 = []string{"::1/128", "fc00::/7", "fe80::/10", "ff00::/8", "64:ff9b:1::/48"}

// Well-known prefixes that embed IPv4 addresses.
var (
	mappedPrefix = netip.MustParsePrefix("::ffff:0:0/96")
	nat64Prefix  = netip.MustParsePrefix("64:ff9b::/96")
)

// Route is one host route.
type Route struct {
	Prefix    netip.Prefix
	Interface string
	// Table names the routing table of the route on Linux, where policy
	// routing rules select among several IPv4 tables. It is empty for the
	// IPv6 routes and on the other host OSes, which have one table.
	Table string
}

// embed4 returns the form of an IPv4 prefix inside a /96 IPv6 prefix.
func embed4(base netip.Prefix, p netip.Prefix) netip.Prefix {
	b := base.Addr().As16()
	a := p.Addr().As4()
	copy(b[12:], a[:])
	return netip.PrefixFrom(netip.AddrFrom16(b), 96+p.Bits())
}

// isDefault reports whether r is a default route.
func isDefault(r Route) bool { return r.Prefix.Bits() == 0 }

// halfDefaults returns the /1 routes that form a complementary pair
// through the same interface of the same routing table (for example a
// full-tunnel VPN); they count as a default route. A half whose
// complement lives in another table stays an ordinary route: a packet
// that policy routing sends to one table never uses the other half.
func halfDefaults(routes []Route) map[Route]bool {
	out := map[Route]bool{}
	for _, a := range routes {
		if a.Prefix.Bits() != 1 {
			continue
		}
		for _, b := range routes {
			if b.Prefix.Bits() == 1 && a.Interface == b.Interface && a.Table == b.Table && a.Prefix.Addr().Is4() == b.Prefix.Addr().Is4() && a.Prefix.Masked() != b.Prefix.Masked() {
				out[a] = true
			}
		}
	}
	return out
}

// Blocked returns the sorted, deduplicated IPv4 and IPv6 prefixes that
// guests must not reach, given the host routes (including on-link routes)
// and the prefixes of the host interface addresses.
func Blocked(routes []Route, onLink []netip.Prefix) (v4, v6 []netip.Prefix) {
	set4 := map[netip.Prefix]bool{}
	set6 := map[netip.Prefix]bool{}
	add := func(p netip.Prefix) {
		p = p.Masked()
		if p.Addr().Is4() {
			set4[p] = true
		} else {
			set6[p] = true
		}
	}
	for _, s := range staticBlocked4 {
		add(netip.MustParsePrefix(s))
	}
	for _, s := range staticBlocked6 {
		add(netip.MustParsePrefix(s))
	}
	pairs := halfDefaults(routes)
	for _, r := range routes {
		if isDefault(r) || pairs[r] || !r.Prefix.IsValid() {
			continue
		}
		add(r.Prefix)
	}
	for _, p := range onLink {
		if p.IsValid() && p.Bits() > 0 {
			add(p)
		}
	}
	for p := range set4 {
		set6[embed4(mappedPrefix, p)] = true
		set6[embed4(nat64Prefix, p)] = true
	}
	return sortPrefixes(set4), sortPrefixes(set6)
}

func sortPrefixes(set map[netip.Prefix]bool) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b netip.Prefix) int {
		if c := a.Addr().Compare(b.Addr()); c != 0 {
			return c
		}
		return a.Bits() - b.Bits()
	})
	return out
}

func joinPrefixes(ps []netip.Prefix) string {
	parts := make([]string, len(ps))
	for i, p := range ps {
		parts[i] = p.String()
	}
	return strings.Join(parts, ", ")
}

// TapGateway binds the tap device of one sandbox link to the two addresses
// of that link: the gateway address on which the agent serves the DNS of
// the sandbox, and the guest address that the sandbox owns.
type TapGateway struct {
	Tap     string
	Gateway netip.Addr
	Guest   netip.Addr
}

// valid reports whether the assignment has a tap name and IPv4 gateway and
// guest addresses. The rules render nothing for any other assignment: the
// final drop of the input chain applies, and the forward chain has no
// per-tap source check for it.
func (a TapGateway) valid() bool {
	return a.Tap != "" && a.Gateway.Is4() && a.Guest.Is4()
}

// sortedAssignments returns the valid assignments sorted by tap, gateway
// and guest, without duplicates, so that equal assignments render the same
// text.
func sortedAssignments(assignments []TapGateway) []TapGateway {
	sorted := slices.Clone(assignments)
	slices.SortFunc(sorted, func(a, b TapGateway) int {
		if c := strings.Compare(a.Tap, b.Tap); c != 0 {
			return c
		}
		if c := a.Gateway.Compare(b.Gateway); c != 0 {
			return c
		}
		return a.Guest.Compare(b.Guest)
	})
	sorted = slices.Compact(sorted)
	return slices.DeleteFunc(sorted, func(a TapGateway) bool { return !a.valid() })
}

// dnsAcceptRules returns the input chain rules that accept DNS (UDP and
// TCP port 53) from the guest address of each assigned tap to the gateway
// address of the same assignment only. A packet from that tap with another
// source address matches no rule and falls to the final drop of the input
// chain. The output is sorted by tap. An invalid assignment renders no
// rule.
func dnsAcceptRules(assignments []TapGateway) string {
	var sb strings.Builder
	for _, a := range sortedAssignments(assignments) {
		for _, proto := range []string{"udp", "tcp"} {
			fmt.Fprintf(&sb, "\t\tiifname %q ip saddr %s ip daddr %s %s dport 53 accept\n", a.Tap, a.Guest, a.Gateway, proto)
		}
	}
	return sb.String()
}

// linkAcceptRules returns the input chain rules that accept the link-local
// configuration traffic of each assigned tap, one group of three rules per
// tap sorted by tap, each rule naming its own tap:
//
//   - DHCPv4 from the unspecified address or the guest address of the
//     assignment to the gateway address of the assignment or the limited
//     broadcast address (UDP port 68 to 67);
//   - DHCPv6 from a link-local source to the All_DHCP_Relay_Agents_and_Servers
//     address (UDP port 546 to 547);
//   - IPv6 neighbor and router discovery with hop limit 255 to a link-local
//     or link-scope multicast address, the only form that RFC 4861 accepts.
//
// A packet from a tap that matches none of them, and every packet from a tap
// without a valid assignment, falls to the final drop of the input chain.
// An invalid assignment renders no rule.
func linkAcceptRules(assignments []TapGateway) string {
	var sb strings.Builder
	for _, a := range sortedAssignments(assignments) {
		fmt.Fprintf(&sb, "\t\tiifname %q ip saddr { 0.0.0.0, %s } ip daddr { %s, 255.255.255.255 } udp sport 68 udp dport 67 accept\n", a.Tap, a.Guest, a.Gateway)
		fmt.Fprintf(&sb, "\t\tiifname %q ip6 saddr fe80::/10 ip6 daddr ff02::1:2 udp sport 546 udp dport 547 accept\n", a.Tap)
		fmt.Fprintf(&sb, "\t\tiifname %q ip6 hoplimit 255 ip6 daddr { fe80::/10, ff02::/16 } icmpv6 type { nd-router-solicit, nd-neighbor-solicit, nd-neighbor-advert } accept\n", a.Tap)
	}
	return sb.String()
}

// sourceGuardRules returns the forward chain rules that drop an IPv4 packet
// from an assigned tap whose source address is not the guest address of
// that assignment, so that a guest cannot forward packets with the address
// of another sandbox or of the host. DHCPv4 from 0.0.0.0 is delivered to
// the host and hits the input chain only, and `ip saddr` does not match
// IPv6 packets, so neither is affected (the forward chain drops all IPv6
// packets from a tap in a separate rule). An invalid assignment renders no
// rule.
func sourceGuardRules(assignments []TapGateway) string {
	var sb strings.Builder
	for _, a := range sortedAssignments(assignments) {
		fmt.Fprintf(&sb, "\t\tiifname %q ip saddr != %s drop\n", a.Tap, a.Guest)
	}
	return sb.String()
}

// Ruleset returns the nftables script that replaces the agent's table
// atomically. Guests reach only the internet: packets to blocked prefixes
// are dropped in forward, packets to the host itself are dropped in input
// except DNS from the guest address of each sandbox on its tap to that
// sandbox's own gateway address (assignments), DHCPv4, DHCPv6 and IPv6
// neighbor and router discovery that each name one sandbox tap and match the
// addresses of the guest link (linkAcceptRules), inbound connections to guests are
// dropped, and guest traffic leaves through NAT. DNS to any other host
// address, such as the LAN address or the gateway of another sandbox, and
// DNS from another source address, falls through to the drop. IPv4 packets
// from a sandbox tap with a source other than the guest address of that
// sandbox are dropped in forward, and so are all IPv6 packets from a sandbox
// tap, because guests get no IPv6 address in this version: such a packet
// has a spoofed source, and the blocked6 drop alone would forward it to any
// destination outside the blocked prefixes. IPv6 neighbor and router
// discovery on the guest link hit the input chain only and are unaffected.
// The DNS forwarder listens on the gateway addresses without binding to a
// device, so the input chain also drops every packet to GuestSubnet that
// arrives on an interface other than a sandbox tap or the loopback
// interface (for example a LAN peer that routes the guest subnet through the
// host); the rule follows every accept and precedes the final tap drop.
func Ruleset(v4, v6 []netip.Prefix, assignments []TapGateway) string {
	tap := TapPrefix + "*"
	return fmt.Sprintf(`table inet %[1]s
delete table inet %[1]s
table inet %[1]s {
	set blocked4 {
		type ipv4_addr
		flags interval
		auto-merge
		elements = { %[2]s }
	}
	set blocked6 {
		type ipv6_addr
		flags interval
		auto-merge
		elements = { %[3]s }
	}
	chain input {
		type filter hook input priority filter - 10; policy accept;
%[6]s%[8]s		iifname != "lo" iifname != "%[4]s" ip daddr %[5]s drop
		iifname "%[4]s" drop
	}
	chain forward {
		type filter hook forward priority filter - 10; policy accept;
%[7]s		iifname "%[4]s" meta nfproto ipv6 drop
		iifname "%[4]s" ip daddr @blocked4 drop
		iifname "%[4]s" ip6 daddr @blocked6 drop
		oifname "%[4]s" ct state established,related accept
		oifname "%[4]s" drop
	}
	chain postrouting {
		type nat hook postrouting priority srcnat; policy accept;
		ip saddr %[5]s oifname != "%[4]s" masquerade
	}
}
`, TableName, joinPrefixes(v4), joinPrefixes(v6), tap, GuestSubnet, dnsAcceptRules(assignments), sourceGuardRules(assignments), linkAcceptRules(assignments))
}

// Link is the /30 link of one sandbox.
type Link struct {
	Index   int
	Gateway netip.Addr
	Guest   netip.Addr
	Bits    int
}

// LinkFor returns the link with index i (1-based) inside GuestSubnet.
func LinkFor(i int) (Link, error) {
	maxIndex := 1<<(32-GuestSubnet.Bits())/4 - 1
	if i < 1 || i > maxIndex {
		return Link{}, fmt.Errorf("netpolicy: link index %d outside [1, %d]", i, maxIndex)
	}
	base := GuestSubnet.Addr().As4()
	n := uint32(base[0])<<24 | uint32(base[1])<<16 | uint32(base[2])<<8 | uint32(base[3])
	n += uint32(i) * 4
	addr := func(v uint32) netip.Addr {
		return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
	}
	return Link{Index: i, Gateway: addr(n + 1), Guest: addr(n + 2), Bits: 30}, nil
}

// KernelIPArg returns the kernel `ip=` boot argument that configures the
// guest interface statically, with the gateway as DNS server.
func (l Link) KernelIPArg() string {
	mask := netip.PrefixFrom(l.Gateway, l.Bits)
	m := ^uint32(0) << (32 - mask.Bits())
	return fmt.Sprintf("ip=%s::%s:%d.%d.%d.%d::eth0:off:%s", l.Guest, l.Gateway, byte(m>>24), byte(m>>16), byte(m>>8), byte(m), l.Gateway)
}

// TapName returns the tap device name of the link with the given index:
// TapPrefix and the index as 11 zero-padded lower-case hex digits, 15 bytes
// in total (the Linux interface-name limit). The manager reserves a link
// index for one attachment until its teardown ends, so two live or pending
// attachments never share a tap name.
func TapName(linkIndex int) string {
	return fmt.Sprintf("%s%011x", TapPrefix, linkIndex)
}

var errNoLinks = errors.New("netpolicy: no free sandbox link")
