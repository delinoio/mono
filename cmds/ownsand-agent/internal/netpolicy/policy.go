// SPDX-License-Identifier: Apache-2.0

// Package netpolicy computes the OUTBOUND network policy of sandbox guests:
// the destination prefixes that guests must never reach (the fixed private,
// loopback, link-local, multicast and reserved ranges with their
// IPv4-mapped and NAT64 forms, and every on-link or non-default-route
// prefix of the host routing table, where a complementary /1 pair through
// one interface counts as a default route), the per-sandbox addresses, and
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
// through the same interface (for example a full-tunnel VPN); they count
// as a default route.
func halfDefaults(routes []Route) map[Route]bool {
	out := map[Route]bool{}
	for _, a := range routes {
		if a.Prefix.Bits() != 1 {
			continue
		}
		for _, b := range routes {
			if b.Prefix.Bits() == 1 && a.Interface == b.Interface && a.Prefix.Addr().Is4() == b.Prefix.Addr().Is4() && a.Prefix.Masked() != b.Prefix.Masked() {
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

// Ruleset returns the nftables script that replaces the agent's table
// atomically. Guests reach only the internet: packets to blocked prefixes
// are dropped in forward, packets to the host itself are dropped in input
// except DNS to the gateway, DHCP and IPv6 neighbor and router discovery on
// the guest link, inbound connections to guests are dropped, and guest
// traffic leaves through NAT.
func Ruleset(v4, v6 []netip.Prefix) string {
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
		iifname "%[4]s" udp dport 53 accept
		iifname "%[4]s" tcp dport 53 accept
		iifname "%[4]s" udp sport 68 udp dport 67 accept
		iifname "%[4]s" udp sport 546 udp dport 547 accept
		iifname "%[4]s" icmpv6 type { nd-router-solicit, nd-neighbor-solicit, nd-neighbor-advert } accept
		iifname "%[4]s" drop
	}
	chain forward {
		type filter hook forward priority filter - 10; policy accept;
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
`, TableName, joinPrefixes(v4), joinPrefixes(v6), tap, GuestSubnet)
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

// TapName returns the tap device name of a sandbox: TapPrefix and 11 hex
// characters of the sandbox ID, 15 bytes in total (the Linux limit).
func TapName(sandboxID string) string {
	hex := strings.ReplaceAll(sandboxID, "-", "")
	if len(hex) > 11 {
		hex = hex[len(hex)-11:]
	}
	return TapPrefix + hex
}

var errNoLinks = errors.New("netpolicy: no free sandbox link")
