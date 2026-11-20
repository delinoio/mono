// SPDX-License-Identifier: FSL-1.1-ALv2

package agent

import (
	"testing"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/store"
)

func TestTargetVersion(t *testing.T) {
	d := store.Device{AgentVersion: "1.0.0", Capabilities: &ownsandv1.DeviceCapabilities{HostOs: ownsandv1.HostOs_HOST_OS_LINUX, Arch: ownsandv1.Arch_ARCH_AMD64}}
	defaults := map[string]string{"linux/amd64": "1.2.0"}
	pol := func(mode ownsandv1.AgentVersionPolicyMode, pin string) store.AgentVersionPolicy {
		return store.AgentVersionPolicy{Mode: mode, PinnedVersion: pin, DefaultVersions: defaults}
	}
	cases := []struct {
		name     string
		p        store.AgentVersionPolicy
		revoked  map[string]bool
		want     string
		recovery bool
	}{
		{"follow", pol(ownsandv1.AgentVersionPolicyMode_AGENT_VERSION_POLICY_MODE_FOLLOW_DEFAULT, ""), nil, "1.2.0", false},
		{"pin", pol(ownsandv1.AgentVersionPolicyMode_AGENT_VERSION_POLICY_MODE_PIN, "1.1.0"), nil, "1.1.0", false},
		{"hold", pol(ownsandv1.AgentVersionPolicyMode_AGENT_VERSION_POLICY_MODE_HOLD, ""), nil, "1.0.0", false},
		{"running revoked overrides hold", pol(ownsandv1.AgentVersionPolicyMode_AGENT_VERSION_POLICY_MODE_HOLD, ""), map[string]bool{"1.0.0": true}, "1.2.0", true},
		{"pinned revoked", pol(ownsandv1.AgentVersionPolicyMode_AGENT_VERSION_POLICY_MODE_PIN, "1.1.0"), map[string]bool{"1.1.0": true}, "1.2.0", true},
	}
	for _, c := range cases {
		got, rec := TargetVersion(c.p, d, c.revoked)
		if got != c.want || rec != c.recovery {
			t.Errorf("%s: got %s %v", c.name, got, rec)
		}
	}
	if PlatformKey(ownsandv1.HostOs_HOST_OS_MACOS, ownsandv1.Arch_ARCH_ARM64) != "macos/arm64" {
		t.Fatal("platform key")
	}
}
