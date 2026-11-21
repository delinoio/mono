// SPDX-License-Identifier: Apache-2.0

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
// It skips when nft or unprivileged user namespaces are unavailable.
func TestRulesetParsesWithNft(t *testing.T) {
	if _, err := exec.LookPath("nft"); err != nil {
		t.Skip("nft not installed")
	}
	if _, err := exec.LookPath("unshare"); err != nil {
		t.Skip("unshare not installed")
	}
	routes, onLink, err := HostRoutes()
	if err != nil {
		t.Fatal(err)
	}
	v4, v6 := Blocked(routes, onLink)
	cmd := exec.Command("unshare", "-rn", "nft", "-c", "-f", "-")
	cmd.Stdin = strings.NewReader(Ruleset(v4, v6, []TapGateway{{Tap: TapName(1), Gateway: netip.MustParseAddr("172.30.0.5")}, {Tap: TapName(2), Gateway: netip.MustParseAddr("172.30.0.9")}}))
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		if strings.Contains(out.String(), "Operation not permitted") || strings.Contains(out.String(), "unshare failed") {
			t.Skipf("no unprivileged namespaces: %s", out.String())
		}
		t.Fatalf("nft rejected the ruleset: %v\n%s", err, out.String())
	}
}
