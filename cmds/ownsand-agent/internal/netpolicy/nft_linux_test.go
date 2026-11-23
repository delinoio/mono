// SPDX-License-Identifier: FSL-1.1-ALv2

package netpolicy

import (
	"bytes"
	"net/netip"
	"os/exec"
	"strings"
	"testing"
)

// TestRulesetParsesWithNft checks the generated ruleset with `nft -c` in a
// new user and network namespace, which gives CAP_NET_ADMIN without root.
// It skips when nft, unshare or unprivileged user namespaces are unavailable.
// The ruleset comes from fixture routes, not HostRoutes, so the test needs
// neither the ip tool nor the host routing table.
func TestRulesetParsesWithNft(t *testing.T) {
	if _, err := exec.LookPath("nft"); err != nil {
		t.Skip("nft not installed")
	}
	if _, err := exec.LookPath("unshare"); err != nil {
		t.Skip("unshare not installed")
	}
	routes := []Route{
		{Prefix: netip.MustParsePrefix("0.0.0.0/0"), Interface: "eth0"},
		{Prefix: netip.MustParsePrefix("203.0.113.0/24"), Interface: "eth0"},
		{Prefix: netip.MustParsePrefix("2001:db8:1::/64"), Interface: "eth0"},
		{Prefix: netip.MustParsePrefix("::/0"), Interface: "eth0"},
	}
	onLink := []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}
	v4, v6 := Blocked(routes, onLink)
	taps := []TapGateway{
		{Tap: TapName(1), Gateway: netip.MustParseAddr("172.30.0.5"), Guest: netip.MustParseAddr("172.30.0.6")},
		{Tap: TapName(2), Gateway: netip.MustParseAddr("172.30.0.9"), Guest: netip.MustParseAddr("172.30.0.10")},
	}
	ruleset := Ruleset(v4, v6, taps)
	// Fail even when nft is skipped below: the script must carry the
	// fixture-derived IPv4 and IPv6 elements and both tap entries.
	for _, want := range []string{"203.0.113.0/24", "192.0.2.0/24", "2001:db8:1::/64", taps[0].Tap, taps[1].Tap} {
		if !strings.Contains(ruleset, want) {
			t.Fatalf("ruleset lacks %q:\n%s", want, ruleset)
		}
	}
	cmd := exec.Command("unshare", "-rn", "nft", "-c", "-f", "-")
	cmd.Stdin = strings.NewReader(ruleset)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		if strings.Contains(out.String(), "Operation not permitted") || strings.Contains(out.String(), "unshare failed") {
			t.Skipf("no unprivileged namespaces: %s", out.String())
		}
		t.Fatalf("nft rejected the ruleset: %v\n%s", err, out.String())
	}
}
