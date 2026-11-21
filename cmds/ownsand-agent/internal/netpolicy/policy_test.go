// SPDX-License-Identifier: Apache-2.0

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
		{pfx("0.0.0.0/0"), "eth0"},
		{pfx("203.0.113.0/24"), "eth0"},  // on-link LAN with global addresses
		{pfx("198.51.100.0/24"), "wg0"},  // internal network behind a VPN route
		{pfx("2001:db8:1::/64"), "eth0"}, // IPv6 LAN with global addresses
		{pfx("::/0"), "eth0"},
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
		{pfx("0.0.0.0/1"), "tun0"}, {pfx("128.0.0.0/1"), "tun0"},
		{pfx("::/1"), "tun0"}, {pfx("8000::/1"), "tun0"},
		{pfx("10.8.0.0/16"), "tun0"},
	}
	v4, v6 := Blocked(routes, nil)
	if has(v4, "0.0.0.0/1") || has(v4, "128.0.0.0/1") || has(v6, "::/1") {
		t.Fatal("complementary /1 pair blocked")
	}
	// A single /1 without its complement is an ordinary route.
	v4, _ = Blocked([]Route{{pfx("0.0.0.0/1"), "tun0"}, {pfx("128.0.0.0/1"), "eth0"}}, nil)
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

func TestParseProcRoutes(t *testing.T) {
	v4 := `Iface	Destination	Gateway 	Flags	RefCnt	Use	Metric	Mask		MTU	Window	IRTT
eth0	00000000	0102A8C0	0003	0	0	100	00000000	0	0	0
eth0	0002A8C0	00000000	0001	0	0	100	00FFFFFF	0	0	0
`
	routes, err := ParseProcRoute(strings.NewReader(v4))
	if err != nil || len(routes) != 2 || routes[1].Prefix != pfx("192.168.2.0/24") || routes[0].Prefix.Bits() != 0 {
		t.Fatalf("v4 routes %v %v", routes, err)
	}
	v6 := `20010db8000100000000000000000000 40 00000000000000000000000000000000 00 00000000000000000000000000000000 00000100 00000001 00000000 00000001     eth0
00000000000000000000000000000001 80 00000000000000000000000000000000 00 00000000000000000000000000000000 00000000 00000002 00000000 80200001       lo
`
	routes, err = ParseProcIPv6Route(strings.NewReader(v6))
	if err != nil || len(routes) != 1 || routes[0].Prefix != pfx("2001:db8:1::/64") {
		t.Fatalf("v6 routes %v %v", routes, err)
	}
}
