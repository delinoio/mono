// SPDX-License-Identifier: FSL-1.1-ALv2

package netpolicy

import (
	"net/netip"
	"slices"
	"strings"
	"testing"
)

// The forward chain drops all IPv6 packets from the sandbox taps exactly
// once, with and without assignments, after the per-tap IPv4 source guards
// and before the blocked4 drop. The blocked6 drop and set stay.
func TestRulesetForwardDropsIPv6FromTap(t *testing.T) {
	const ipv6Drop = `iifname "osb-*" meta nfproto ipv6 drop`
	const blocked4Drop = `iifname "osb-*" ip daddr @blocked4 drop`
	v4, v6 := Blocked(nil, nil)
	assignments := []TapGateway{
		{Tap: TapName(1), Gateway: netip.MustParseAddr("172.30.0.5"), Guest: netip.MustParseAddr("172.30.0.6")},
		{Tap: TapName(2), Gateway: netip.MustParseAddr("172.30.0.9"), Guest: netip.MustParseAddr("172.30.0.10")},
	}
	for name, as := range map[string][]TapGateway{"assignments": assignments, "none": nil} {
		rs := Ruleset(v4, v6, as)
		fwd := forwardChain(t, rs)
		if n := strings.Count(rs, ipv6Drop); n != 1 {
			t.Fatalf("%s: IPv6 drop rendered %d times, want 1", name, n)
		}
		at, before := slices.Index(fwd, ipv6Drop), slices.Index(fwd, blocked4Drop)
		if at < 0 || before != at+1 {
			t.Fatalf("%s: IPv6 drop at %d, blocked4 drop at %d in %q", name, at, before, fwd)
		}
		for i, l := range fwd {
			if strings.Contains(l, "ip saddr !=") && i > at {
				t.Errorf("%s: source guard %q after the IPv6 drop", name, l)
			}
		}
		if !slices.Contains(fwd, `iifname "osb-*" ip6 daddr @blocked6 drop`) || !strings.Contains(rs, "set blocked6 {") {
			t.Errorf("%s: blocked6 drop or set missing", name)
		}
		// The IPv6 drop belongs to the forward chain only.
		if slices.Contains(inputChain(t, rs), ipv6Drop) {
			t.Errorf("%s: IPv6 drop in the input chain", name)
		}
	}
}
