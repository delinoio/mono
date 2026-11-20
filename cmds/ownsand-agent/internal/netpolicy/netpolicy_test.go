package netpolicy

import (
	"net/netip"
	"slices"
	"strings"
	"testing"
)

func pfx(s string) netip.Prefix { return netip.MustParsePrefix(s) }

// blocks reports whether one of the prefixes contains addr.
func blocks(ps []netip.Prefix, addr string) bool {
	a := netip.MustParseAddr(addr)
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func blocked(h Host, addr string) bool {
	v4, v6 := Blocked(h)
	if netip.MustParseAddr(addr).Is4() {
		return blocks(v4, addr)
	}
	return blocks(v6, addr)
}

func TestStaticRangesAreBlocked(t *testing.T) {
	h := Host{}
	for _, a := range []string{
		"0.1.2.3", "10.1.2.3", "100.64.0.1", "100.127.255.255", "127.0.0.1", "169.254.169.254",
		"172.16.0.1", "172.31.255.255", "192.168.1.1", "224.0.0.251", "239.255.255.250", "240.0.0.1", "255.255.255.255",
		"::1", "fc00::1", "fd12:3456::1", "fe80::1", "ff02::1", "64:ff9b:1::1",
		// IPv4-mapped and NAT64 forms of the IPv4 ranges.
		"::ffff:10.0.0.1", "::ffff:169.254.169.254", "::ffff:127.0.0.1", "64:ff9b::a00:1", "64:ff9b::c0a8:101",
	} {
		if !blocked(h, a) {
			t.Errorf("%s is not blocked", a)
		}
	}
	for _, a := range []string{"1.1.1.1", "8.8.8.8", "100.128.0.1", "172.32.0.1", "2606:4700::1111", "::ffff:8.8.8.8", "64:ff9b::808:808"} {
		if blocked(h, a) {
			t.Errorf("internet address %s is blocked", a)
		}
	}
}

func TestHostAddressesAndOnLinkPrefixes(t *testing.T) {
	h := Host{
		Addresses: []Address{
			{Interface: "eth0", Prefix: pfx("203.0.113.10/24")},
			// A LAN with global IPv6 addresses.
			{Interface: "eth0", Prefix: pfx("2001:db8:1:2::10/64")},
			{Interface: "lo", Prefix: pfx("127.0.0.1/8")},
			// The agent's own tap devices are not blocked.
			{Interface: "osb3", Prefix: pfx("198.51.100.1/30")},
		},
	}
	for _, a := range []string{"203.0.113.10", "203.0.113.77", "2001:db8:1:2::10", "2001:db8:1:2::ffff", "::ffff:203.0.113.5", "64:ff9b::cb00:7105"} {
		if !blocked(h, a) {
			t.Errorf("%s is not blocked", a)
		}
	}
	for _, a := range []string{"203.0.114.1", "2001:db8:1:3::1", "198.51.100.2"} {
		if blocked(h, a) {
			t.Errorf("%s is blocked", a)
		}
	}
}

func TestOnLinkPrefixChangeChangesRules(t *testing.T) {
	before := Host{Addresses: []Address{{Interface: "wlan0", Prefix: pfx("203.0.113.10/24")}}}
	after := Host{Addresses: []Address{{Interface: "wlan0", Prefix: pfx("198.51.100.20/24")}}}
	b4, _ := Blocked(before)
	a4, _ := Blocked(after)
	if slices.Equal(b4, a4) {
		t.Fatal("the blocked set did not change with the on-link prefix")
	}
	if !blocks(a4, "198.51.100.99") || blocks(a4, "203.0.113.99") {
		t.Fatal("the new on-link prefix is not blocked or the old one still is")
	}
}

func TestNonDefaultRoutesAreBlocked(t *testing.T) {
	h := Host{Routes: []Route{
		{Prefix: pfx("0.0.0.0/0"), Interface: "eth0"},
		{Prefix: pfx("::/0"), Interface: "eth0"},
		// A routed internal network with global addresses.
		{Prefix: pfx("198.18.0.0/15"), Interface: "eth0"},
		{Prefix: pfx("2001:db8:ff::/48"), Interface: "wg0"},
		// A host route.
		{Prefix: pfx("192.0.2.7/32"), Interface: "eth0"},
	}}
	for _, a := range []string{"198.18.5.5", "198.19.255.1", "2001:db8:ff::1", "192.0.2.7", "::ffff:198.18.0.1"} {
		if !blocked(h, a) {
			t.Errorf("%s behind a non-default route is not blocked", a)
		}
	}
	for _, a := range []string{"8.8.8.8", "192.0.2.8", "2606:4700::1"} {
		if blocked(h, a) {
			t.Errorf("%s through the default route is blocked", a)
		}
	}
}

func TestFullTunnelVPNPairCountsAsDefault(t *testing.T) {
	h := Host{Routes: []Route{
		{Prefix: pfx("0.0.0.0/0"), Interface: "eth0"},
		{Prefix: pfx("0.0.0.0/1"), Interface: "tun0"},
		{Prefix: pfx("128.0.0.0/1"), Interface: "tun0"},
		{Prefix: pfx("::/1"), Interface: "tun0"},
		{Prefix: pfx("8000::/1"), Interface: "tun0"},
		// The VPN's separate internal route stays blocked.
		{Prefix: pfx("198.18.0.0/16"), Interface: "tun0"},
	}}
	for _, a := range []string{"8.8.8.8", "200.1.1.1", "2606:4700::1", "9000::1"} {
		if blocked(h, a) {
			t.Errorf("full-tunnel VPN blocks the internet address %s", a)
		}
	}
	if !blocked(h, "198.18.1.1") {
		t.Error("the internal route behind the VPN is not blocked")
	}
}

func TestSingleHalfRouteIsBlocked(t *testing.T) {
	cases := []Host{
		{Routes: []Route{{Prefix: pfx("0.0.0.0/1"), Interface: "tun0"}}},
		// The complement through another interface does not make a pair.
		{Routes: []Route{{Prefix: pfx("0.0.0.0/1"), Interface: "tun0"}, {Prefix: pfx("128.0.0.0/1"), Interface: "tun1"}}},
	}
	for i, h := range cases {
		if !blocked(h, "8.8.8.8") {
			t.Errorf("case %d: a single /1 route is not blocked", i)
		}
	}
	if !blocked(Host{Routes: []Route{{Prefix: pfx("8000::/1"), Interface: "tun0"}}}, "9000::1") {
		t.Error("a single IPv6 /1 route is not blocked")
	}
}

func TestSandboxRoutesAreNotBlocked(t *testing.T) {
	h := Host{Routes: []Route{{Prefix: pfx("198.51.100.0/30"), Interface: "osb0"}}}
	if blocked(h, "198.51.100.2") {
		t.Error("a sandbox tap route is blocked")
	}
}

func TestBlockedSetIsCompact(t *testing.T) {
	h := Host{
		Addresses: []Address{{Interface: "eth0", Prefix: pfx("10.1.2.3/24")}},
		Routes:    []Route{{Prefix: pfx("10.9.0.0/16"), Interface: "eth0"}, {Prefix: pfx("10.9.0.0/16"), Interface: "eth1"}},
	}
	v4, v6 := Blocked(h)
	for _, set := range [][]netip.Prefix{v4, v6} {
		for i := range set {
			for j := range set {
				if i != j && set[i].Overlaps(set[j]) {
					t.Fatalf("overlapping prefixes %v and %v", set[i], set[j])
				}
			}
		}
	}
	if slices.Contains(v4, pfx("10.1.2.0/24")) {
		t.Fatal("a prefix inside 10.0.0.0/8 was kept")
	}
}

func TestRulesetShape(t *testing.T) {
	v4, v6 := Blocked(Host{})
	rs := Ruleset(Config{GuestRange: pfx("100.127.0.0/16"), Blocked4: v4, Blocked6: v6})
	for _, want := range []string{
		"add table inet ownsand\ndelete table inet ownsand\n",
		"set blocked4 {", "set blocked6 {", "flags interval",
		"10.0.0.0/8", "::ffff:10.0.0.0/104", "64:ff9b::a00:0/104",
		`iifname "osb*" ip daddr @blocked4 drop`,
		`iifname "osb*" ip6 daddr @blocked6 drop`,
		`oifname "osb*" ct state established,related accept`,
		`oifname "osb*" drop`,
		"fib daddr . iif type local udp dport 53 accept",
		"udp sport 68 udp dport 67 accept",
		"ip saddr 100.127.0.0/16 oifname != \"osb*\" masquerade",
	} {
		if !strings.Contains(rs, want) {
			t.Errorf("ruleset lacks %q", want)
		}
	}
	// The input chain drops everything else from a tap: no other host port.
	in := rs[strings.Index(rs, "chain input"):strings.Index(rs, "chain forward")]
	if !strings.HasSuffix(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(in), "}")), "drop") {
		t.Errorf("the input chain does not end with drop:\n%s", in)
	}
}

func TestParseIPOutput(t *testing.T) {
	addrs, err := ParseAddresses([]byte(`[{"ifname":"lo","addr_info":[{"family":"inet","local":"127.0.0.1","prefixlen":8}]},
		{"ifname":"eth0","addr_info":[{"family":"inet","local":"192.0.2.5","prefixlen":24},{"family":"inet6","local":"fe80::1","prefixlen":64}]}]`))
	if err != nil || len(addrs) != 3 || addrs[1].Prefix != pfx("192.0.2.5/24") {
		t.Fatalf("addresses %v %v", addrs, err)
	}
	routes, err := ParseRoutes([]byte(`[{"dst":"default","gateway":"192.0.2.1","dev":"eth0"},
		{"dst":"192.0.2.0/24","dev":"eth0","protocol":"kernel"},
		{"type":"local","dst":"192.0.2.5","dev":"eth0","table":"local"},
		{"type":"broadcast","dst":"192.0.2.255","dev":"eth0","table":"local"},
		{"type":"unreachable","dst":"198.18.0.0/15"},
		{"dst":"203.0.113.9","dev":"tun0"}]`), false)
	if err != nil {
		t.Fatal(err)
	}
	want := []Route{{pfx("0.0.0.0/0"), "eth0"}, {pfx("192.0.2.0/24"), "eth0"}, {pfx("203.0.113.9/32"), "tun0"}}
	if !slices.Equal(routes, want) {
		t.Fatalf("routes %v", routes)
	}
	r6, _ := ParseRoutes([]byte(`[{"dst":"default","dev":"eth0"},{"dst":"2001:db8::/64","dev":"eth0"}]`), true)
	if len(r6) != 2 || r6[0].Prefix != pfx("::/0") {
		t.Fatalf("v6 routes %v", r6)
	}
}
