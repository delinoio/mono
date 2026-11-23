// SPDX-License-Identifier: FSL-1.1-ALv2

package netpolicy

import (
	"net/netip"
	"slices"
	"strings"
	"testing"
)

// The input chain drops packets to the guest subnet that arrive on any
// interface except a sandbox tap or the loopback interface, exactly once,
// with and without assignments: after the per-tap accepts and before the
// final tap drop, which stays last. The DNS accepts stay first.
func TestRulesetInputDropsGuestSubnetFromOtherInterfaces(t *testing.T) {
	want := `iifname != "lo" iifname != "osb-*" ip daddr ` + GuestSubnet.String() + ` drop`
	const finalDrop = `iifname "osb-*" drop`
	v4, v6 := Blocked(nil, nil)
	assignments := []TapGateway{
		{Tap: TapName(2), Gateway: netip.MustParseAddr("172.30.0.9"), Guest: netip.MustParseAddr("172.30.0.10")},
		{Tap: TapName(1), Gateway: netip.MustParseAddr("172.30.0.5"), Guest: netip.MustParseAddr("172.30.0.6")},
	}
	for name, as := range map[string][]TapGateway{"assignments": assignments, "none": nil} {
		rs := Ruleset(v4, v6, as)
		in := inputChain(t, rs)
		if n := strings.Count(rs, want); n != 1 {
			t.Fatalf("%s: subnet drop rendered %d times, want 1", name, n)
		}
		at := slices.Index(in, want)
		if at < 0 || in[len(in)-1] != finalDrop || at != len(in)-2 {
			t.Fatalf("%s: subnet drop at %d in %q, want it right before the final drop", name, at, in)
		}
		// Every accept comes before the drop, so no accepted traffic is cut off.
		for i, l := range in {
			if strings.HasSuffix(l, " accept") && i > at {
				t.Errorf("%s: accept %q after the subnet drop", name, l)
			}
			if i >= 1 && i <= len(as)*2 && !strings.Contains(l, "dport 53 accept") {
				t.Errorf("%s: rule %d = %q, want a DNS accept first", name, i, l)
			}
		}
		// The forward chain does not carry the rule.
		if slices.Contains(forwardChain(t, rs), want) {
			t.Errorf("%s: subnet drop in the forward chain", name)
		}
	}
}
