// SPDX-License-Identifier: FSL-1.1-ALv2

package netpolicy

import (
	"net/netip"
	"slices"
	"strings"
	"testing"
)

func pfx(s string) netip.Prefix { return netip.MustParsePrefix(s) }

func has(ps []netip.Prefix, s string) bool { return slices.Contains(ps, pfx(s)) }

func TestBlockedStaticAndRoutes(t *testing.T) {
	routes := []Route{
		{Prefix: pfx("0.0.0.0/0"), Interface: "eth0"},
		{Prefix: pfx("203.0.113.0/24"), Interface: "eth0"},  // on-link LAN with global addresses
		{Prefix: pfx("198.51.100.0/24"), Interface: "wg0"},  // internal network behind a VPN route
		{Prefix: pfx("2001:db8:1::/64"), Interface: "eth0"}, // IPv6 LAN with global addresses
		{Prefix: pfx("::/0"), Interface: "eth0"},
	}
	v4, v6 := Blocked(routes, []netip.Prefix{pfx("192.0.2.0/24")})
	for _, s := range []string{"10.0.0.0/8", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12", "203.0.113.0/24", "198.51.100.0/24", "192.0.2.0/24", "240.0.0.0/4"} {
		if !has(v4, s) {
			t.Errorf("v4 lacks %s", s)
		}
	}
	if has(v4, "0.0.0.0/0") || has(v6, "::/0") {
		t.Fatal("default route blocked")
	}
	for _, s := range []string{"::1/128", "fc00::/7", "fe80::/10", "ff00::/8", "64:ff9b:1::/48", "2001:db8:1::/64", "::ffff:10.0.0.0/104", "64:ff9b::a00:0/104", "::ffff:203.0.113.0/120"} {
		if !has(v6, s) {
			t.Errorf("v6 lacks %s", s)
		}
	}
}

func TestFullTunnelVPNPairIsDefault(t *testing.T) {
	routes := []Route{
		{Prefix: pfx("0.0.0.0/1"), Interface: "tun0"}, {Prefix: pfx("128.0.0.0/1"), Interface: "tun0"},
		{Prefix: pfx("::/1"), Interface: "tun0"}, {Prefix: pfx("8000::/1"), Interface: "tun0"},
		{Prefix: pfx("10.8.0.0/16"), Interface: "tun0"},
	}
	v4, v6 := Blocked(routes, nil)
	if has(v4, "0.0.0.0/1") || has(v4, "128.0.0.0/1") || has(v6, "::/1") {
		t.Fatal("complementary /1 pair blocked")
	}
	// A single /1 without its complement is an ordinary route.
	v4, _ = Blocked([]Route{{Prefix: pfx("0.0.0.0/1"), Interface: "tun0"}, {Prefix: pfx("128.0.0.0/1"), Interface: "eth0"}}, nil)
	if !has(v4, "0.0.0.0/1") || !has(v4, "128.0.0.0/1") {
		t.Fatal("/1 routes through different interfaces not blocked")
	}
}

func TestRulesetAndLinks(t *testing.T) {
	v4, v6 := Blocked(nil, nil)
	rs := Ruleset(v4, v6, nil)
	for _, want := range []string{"delete table inet ownsand", `iifname "osb-*" ip daddr @blocked4 drop`, `oifname "osb-*" drop`, "masquerade", "10.0.0.0/8"} {
		if !strings.Contains(rs, want) {
			t.Errorf("ruleset lacks %q", want)
		}
	}
	l, err := LinkFor(1)
	if err != nil || l.Gateway.String() != "172.30.0.5" || l.Guest.String() != "172.30.0.6" {
		t.Fatalf("link %v %v", l, err)
	}
	if got := l.KernelIPArg(); got != "ip=172.30.0.6::172.30.0.5:255.255.255.252::eth0:off:172.30.0.5" {
		t.Fatalf("ip arg %s", got)
	}
	if _, err := LinkFor(0); err == nil {
		t.Fatal("index 0 accepted")
	}
	if _, err := LinkFor(16384); err == nil {
		t.Fatal("index beyond the subnet accepted")
	}
}

// inputChain returns the lines of the input chain of a ruleset.
func inputChain(t *testing.T, rs string) []string {
	t.Helper()
	_, rest, ok := strings.Cut(rs, "chain input {")
	if !ok {
		t.Fatal("ruleset has no input chain")
	}
	body, _, _ := strings.Cut(rest, "\n\t}")
	var lines []string
	for _, l := range strings.Split(body, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// DNS accept rules match each tap and the gateway of that tap only; the
// DHCP and neighbor discovery exceptions stay, and the wildcard drop is the
// last input rule, so DNS to any other host address falls through to it.
func TestRulesetDNSMatchesTapAndGateway(t *testing.T) {
	v4, v6 := Blocked(nil, nil)
	a := TapGateway{Tap: TapName(2), Gateway: netip.MustParseAddr("172.30.0.9")}
	b := TapGateway{Tap: TapName(1), Gateway: netip.MustParseAddr("172.30.0.5")}
	rs := Ruleset(v4, v6, []TapGateway{a, b})
	in := inputChain(t, rs)
	wantDNS := []string{
		`iifname "osb-00000000001" ip daddr 172.30.0.5 udp dport 53 accept`,
		`iifname "osb-00000000001" ip daddr 172.30.0.5 tcp dport 53 accept`,
		`iifname "osb-00000000002" ip daddr 172.30.0.9 udp dport 53 accept`,
		`iifname "osb-00000000002" ip daddr 172.30.0.9 tcp dport 53 accept`,
	}
	// Preamble line (type filter hook ...) first, then sorted DNS rules.
	if len(in) < 1+len(wantDNS) || !slices.Equal(in[1:1+len(wantDNS)], wantDNS) {
		t.Fatalf("input chain DNS rules = %q, want %q", in, wantDNS)
	}
	// No DNS rule without a destination match, for any tap.
	for _, l := range in {
		if strings.Contains(l, "dport 53") && !strings.Contains(l, "ip daddr 172.30.0.") {
			t.Errorf("DNS rule without gateway match: %q", l)
		}
	}
	// Controls: the other exceptions and the final drop stay.
	for _, want := range []string{
		`iifname "osb-*" udp sport 68 udp dport 67 accept`,
		`iifname "osb-*" udp sport 546 udp dport 547 accept`,
		`iifname "osb-*" icmpv6 type { nd-router-solicit, nd-neighbor-solicit, nd-neighbor-advert } accept`,
	} {
		if !slices.Contains(in, want) {
			t.Errorf("input chain lacks %q", want)
		}
	}
	if last := in[len(in)-1]; last != `iifname "osb-*" drop` {
		t.Fatalf("last input rule = %q, want the wildcard drop", last)
	}
	// The cross-tap and wildcard DNS forms never appear.
	for _, bad := range []string{`iifname "osb-*" udp dport 53`, `iifname "osb-*" tcp dport 53`, `osb-00000000001" ip daddr 172.30.0.9`, `osb-00000000002" ip daddr 172.30.0.5`} {
		if strings.Contains(rs, bad) {
			t.Errorf("ruleset contains %q", bad)
		}
	}
	// The output does not depend on the assignment order.
	if rs != Ruleset(v4, v6, []TapGateway{b, a}) {
		t.Error("ruleset depends on the assignment order")
	}
	// Removing one assignment removes only its two DNS rules.
	without := inputChain(t, Ruleset(v4, v6, []TapGateway{b}))
	if want := slices.DeleteFunc(slices.Clone(in), func(l string) bool { return strings.Contains(l, "osb-00000000002") }); !slices.Equal(without, want) {
		t.Fatalf("after removing one assignment: %q, want %q", without, want)
	}
	// No assignment: no DNS exception at all.
	for _, l := range inputChain(t, Ruleset(v4, v6, nil)) {
		if strings.Contains(l, "dport 53") {
			t.Errorf("DNS rule without an assignment: %q", l)
		}
	}
	// An invalid assignment renders no rule.
	for _, l := range inputChain(t, Ruleset(v4, v6, []TapGateway{{Tap: "", Gateway: a.Gateway}, {Tap: a.Tap}, {Tap: a.Tap, Gateway: netip.MustParseAddr("fe80::1")}})) {
		if strings.Contains(l, "dport 53") {
			t.Errorf("DNS rule from an invalid assignment: %q", l)
		}
	}
}

// The tap name derives from the link index: it keeps the prefix and the
// 15-byte Linux limit for every valid index, and differs for every index.
func TestTapName(t *testing.T) {
	if got := TapName(1); got != "osb-00000000001" {
		t.Fatalf("TapName(1) = %q, want osb-00000000001", got)
	}
	if got := TapName(16383); got != "osb-00000003fff" {
		t.Fatalf("TapName(16383) = %q, want osb-00000003fff", got)
	}
	seen := map[string]int{}
	for i := 1; i < 1<<14; i++ {
		n := TapName(i)
		if len(n) != 15 || !strings.HasPrefix(n, TapPrefix) {
			t.Fatalf("TapName(%d) = %q: want prefix %q and 15 bytes", i, n, TapPrefix)
		}
		if j, dup := seen[n]; dup {
			t.Fatalf("TapName(%d) and TapName(%d) are both %q", j, i, n)
		}
		seen[n] = i
	}
}

// ipRouteShowFixture is a synthetic `ip -4 -o route show table all` dump:
// the main table, a policy routing table 100, the local table and the route
// types that carry no reachable destination.
const ipRouteShowFixture = `default via 192.0.2.254 dev eth0 proto dhcp src 192.0.2.1 metric 100
192.0.2.0/24 dev eth0 proto kernel scope link src 192.0.2.1 metric 100
203.0.113.9 dev eth0 scope link
198.51.100.0/24 dev wg0 table 100 scope link
0.0.0.0/1 dev wg0 table 100 scope link
198.18.0.0/24 proto static \    nexthop via 192.0.2.7 dev eth0 weight 1 \    nexthop via 192.0.2.8 dev eth1 weight 1
unreachable 203.0.113.64/26 table 100
blackhole 203.0.113.128/26 table 100
throw 10.9.0.0/16 table 100
prohibit 100.100.0.0/16
multicast 224.5.0.0/16 dev wg0 table 100
broadcast 192.0.2.255 dev eth0 table local proto kernel scope link src 192.0.2.1
local 192.0.2.1 dev eth0 table local proto kernel scope host src 192.0.2.1
local 127.0.0.0/8 dev lo table local proto kernel scope host src 127.0.0.1
`

func TestParseIPRouteShow(t *testing.T) {
	routes, err := ParseIPRouteShow(strings.NewReader(ipRouteShowFixture))
	if err != nil {
		t.Fatal(err)
	}
	want := []Route{
		{Prefix: pfx("0.0.0.0/0"), Interface: "eth0", Table: "main"},
		{Prefix: pfx("192.0.2.0/24"), Interface: "eth0", Table: "main"},
		{Prefix: pfx("203.0.113.9/32"), Interface: "eth0", Table: "main"},
		{Prefix: pfx("198.51.100.0/24"), Interface: "wg0", Table: "100"},
		{Prefix: pfx("0.0.0.0/1"), Interface: "wg0", Table: "100"},
		{Prefix: pfx("198.18.0.0/24"), Interface: "eth0", Table: "main"},
	}
	if !slices.Equal(routes, want) {
		t.Fatalf("routes\n got %v\nwant %v", routes, want)
	}
}

func TestParseIPRouteShowRejectsBadPrefix(t *testing.T) {
	for _, line := range []string{"banana dev eth0", "10.0.0.0/33 dev eth0", "2001:db8::/32 dev eth0", "local", ""} {
		routes, err := ParseIPRouteShow(strings.NewReader(line + "\n"))
		if line == "" {
			if err != nil || len(routes) != 0 {
				t.Errorf("empty line: %v %v", routes, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%q parsed as %v", line, routes)
		}
	}
}

// A prefix that the host reaches only through a policy routing table is
// blocked with its IPv4-mapped and NAT64 forms; default routes of any table
// add nothing.
func TestBlockedIncludesSecondaryTableRoutes(t *testing.T) {
	routes, err := ParseIPRouteShow(strings.NewReader(ipRouteShowFixture))
	if err != nil {
		t.Fatal(err)
	}
	v4, v6 := Blocked(routes, nil)
	for _, s := range []string{"198.51.100.0/24", "203.0.113.9/32", "198.18.0.0/24", "192.0.2.0/24"} {
		if !has(v4, s) {
			t.Errorf("v4 lacks %s", s)
		}
	}
	for _, s := range []string{"::ffff:198.51.100.0/120", "64:ff9b::c633:6400/120"} {
		if !has(v6, s) {
			t.Errorf("v6 lacks %s", s)
		}
	}
	// A lone 0.0.0.0/1 of table 100 is an ordinary route.
	if !has(v4, "0.0.0.0/1") {
		t.Error("v4 lacks the lone 0.0.0.0/1 of table 100")
	}
	// Routes without a destination add no prefix.
	for _, s := range []string{"0.0.0.0/0", "203.0.113.64/26", "203.0.113.128/26", "10.9.0.0/16", "100.100.0.0/16", "192.0.2.255/32", "192.0.2.1/32"} {
		if has(v4, s) {
			t.Errorf("v4 holds %s", s)
		}
	}
}

func TestHalfDefaultsNeedSameTableAndInterface(t *testing.T) {
	// The pair of one table and interface keeps default-route treatment.
	routes := []Route{
		{Prefix: pfx("0.0.0.0/1"), Interface: "tun0", Table: "100"},
		{Prefix: pfx("128.0.0.0/1"), Interface: "tun0", Table: "100"},
	}
	v4, _ := Blocked(routes, nil)
	if has(v4, "0.0.0.0/1") || has(v4, "128.0.0.0/1") {
		t.Fatal("complementary /1 pair of one table blocked")
	}
	// A half whose complement lives in another table stays an ordinary
	// route, also through the same interface.
	routes = []Route{
		{Prefix: pfx("0.0.0.0/1"), Interface: "tun0", Table: "100"},
		{Prefix: pfx("128.0.0.0/1"), Interface: "tun0", Table: "main"},
	}
	v4, _ = Blocked(routes, nil)
	if !has(v4, "0.0.0.0/1") || !has(v4, "128.0.0.0/1") {
		t.Fatal("/1 halves of different tables not blocked")
	}
}

func TestParseProcIPv6Route(t *testing.T) {
	v6 := `20010db8000100000000000000000000 40 00000000000000000000000000000000 00 00000000000000000000000000000000 00000100 00000001 00000000 00000001     eth0
00000000000000000000000000000001 80 00000000000000000000000000000000 00 00000000000000000000000000000000 00000000 00000002 00000000 80200001       lo
`
	routes, err := ParseProcIPv6Route(strings.NewReader(v6))
	if err != nil || len(routes) != 1 || routes[0].Prefix != pfx("2001:db8:1::/64") {
		t.Fatalf("v6 routes %v %v", routes, err)
	}
}
