// SPDX-License-Identifier: FSL-1.1-ALv2

package netpolicy

import (
	"net/netip"
	"slices"
	"strings"
	"testing"
)

// Every DHCP and neighbor discovery accept names one tap and the addresses
// of its guest link. No accept matches only the tap wildcard with a DHCP
// port or an ICMPv6 type; the rules follow the DNS rules, come in sorted tap
// order and precede the final drop, which stays last.
func TestRulesetLinkAcceptsAreScopedPerTap(t *testing.T) {
	v4, v6 := Blocked(nil, nil)
	a := TapGateway{Tap: TapName(2), Gateway: netip.MustParseAddr("172.30.0.9"), Guest: netip.MustParseAddr("172.30.0.10")}
	b := TapGateway{Tap: TapName(1), Gateway: netip.MustParseAddr("172.30.0.5"), Guest: netip.MustParseAddr("172.30.0.6")}
	rs := Ruleset(v4, v6, []TapGateway{a, b})
	in := inputChain(t, rs)
	wantLink := []string{
		`iifname "osb-00000000001" ip saddr { 0.0.0.0, 172.30.0.6 } ip daddr { 172.30.0.5, 255.255.255.255 } udp sport 68 udp dport 67 accept`,
		`iifname "osb-00000000001" ip6 saddr fe80::/10 ip6 daddr ff02::1:2 udp sport 546 udp dport 547 accept`,
		`iifname "osb-00000000001" ip6 hoplimit 255 ip6 daddr { fe80::/10, ff02::/16 } icmpv6 type { nd-router-solicit, nd-neighbor-solicit, nd-neighbor-advert } accept`,
		`iifname "osb-00000000002" ip saddr { 0.0.0.0, 172.30.0.10 } ip daddr { 172.30.0.9, 255.255.255.255 } udp sport 68 udp dport 67 accept`,
		`iifname "osb-00000000002" ip6 saddr fe80::/10 ip6 daddr ff02::1:2 udp sport 546 udp dport 547 accept`,
		`iifname "osb-00000000002" ip6 hoplimit 255 ip6 daddr { fe80::/10, ff02::/16 } icmpv6 type { nd-router-solicit, nd-neighbor-solicit, nd-neighbor-advert } accept`,
	}
	// Preamble, four DNS rules, then the link rules.
	const dnsRules = 4
	if len(in) < 1+dnsRules+len(wantLink) || !slices.Equal(in[1+dnsRules:1+dnsRules+len(wantLink)], wantLink) {
		t.Fatalf("input chain = %q, want the link rules %q after the DNS rules", in, wantLink)
	}
	for _, l := range in[1 : 1+dnsRules] {
		if !strings.Contains(l, "dport 53 accept") {
			t.Errorf("DNS rules are not first: %q", l)
		}
	}
	if last := in[len(in)-1]; last != `iifname "osb-*" drop` {
		t.Fatalf("last input rule = %q, want the wildcard drop", last)
	}
	// No accept matches only the tap wildcard.
	for _, l := range in {
		if strings.HasSuffix(l, " accept") && strings.Contains(l, `iifname "osb-*"`) {
			t.Errorf("wildcard accept %q", l)
		}
	}
	if rs != Ruleset(v4, v6, []TapGateway{b, a}) {
		t.Error("ruleset depends on the assignment order")
	}
	// Without an assignment there is no DHCP or neighbor discovery accept.
	for _, l := range inputChain(t, Ruleset(v4, v6, nil)) {
		if strings.Contains(l, "sport 68") || strings.Contains(l, "sport 546") || strings.Contains(l, "icmpv6") {
			t.Errorf("link accept without an assignment: %q", l)
		}
	}
	// An invalid assignment renders none either.
	invalid := Ruleset(v4, v6, []TapGateway{{Tap: a.Tap, Gateway: a.Gateway}, {Tap: "", Gateway: a.Gateway, Guest: a.Guest}})
	if strings.Contains(invalid, "sport 68") || strings.Contains(invalid, "icmpv6") {
		t.Errorf("link accept from an invalid assignment:\n%s", invalid)
	}
}
