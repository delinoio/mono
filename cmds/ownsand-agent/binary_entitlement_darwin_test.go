// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build darwin && cgo

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/testcp"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func TestBinaryVirtualizationEntitlement(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and signs the macOS agent")
	}
	path := filepath.Join(t.TempDir(), "ownsand-agent")
	if out, err := exec.CommandContext(t.Context(), "go", "build", "-o", path, ".").CombinedOutput(); err != nil {
		t.Fatalf("build agent: %v\n%s", err, out)
	}
	binary, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, value string
		wantSupport bool
	}{
		{name: "disabled", value: "<false/>"},
		{name: "enabled", value: "<true/>", wantSupport: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cp := testcp.New(t)
			bin := t.TempDir()
			path := filepath.Join(bin, "ownsand-agent")
			if err := os.WriteFile(path, binary, 0o700); err != nil {
				t.Fatal(err)
			}
			entitlements := filepath.Join(bin, "entitlements.plist")
			plist := `<?xml version="1.0" encoding="UTF-8"?>` +
				`<plist version="1.0"><dict><key>com.apple.security.virtualization</key>` + tc.value + `</dict></plist>`
			if err := os.WriteFile(entitlements, []byte(plist), 0o600); err != nil {
				t.Fatal(err)
			}
			if out, err := exec.CommandContext(t.Context(), "/usr/bin/codesign", "--force", "--sign", "-", "--entitlements", entitlements, path).CombinedOutput(); err != nil {
				t.Fatalf("sign agent: %v\n%s", err, out)
			}
			kernel, rootfs := filepath.Join(bin, "vmlinux"), filepath.Join(bin, "rootfs.ext4")
			// The capability report checks for the files. This test never boots a VM.
			for _, image := range []string{kernel, rootfs} {
				if err := os.WriteFile(image, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			env := append(withoutAgentEnv(os.Environ()),
				"OWNSAND_AGENT_STATE_DIR="+t.TempDir(),
				"OWNSAND_AGENT_RUNTIME=virtualization",
				"OWNSAND_AGENT_RELEASE_MANIFEST_URL="+cp.URL+"/manifest.json",
				"OWNSAND_AGENT_LINUX_KERNEL="+kernel,
				"OWNSAND_AGENT_LINUX_ROOTFS="+rootfs,
			)
			enrollProcessRuntimeAgent(t, bin, cp, env)
			agent := startProcessRuntimeAgent(t, bin, env)
			report, _ := cp.WaitFor(binaryAgentTimeout, 0, func(e *ownsandv1.AgentEnvelope) bool {
				return e.GetCapabilityReport() != nil
			})
			if report == nil {
				t.Fatalf("no capability report\n%s", agent.logs.String())
			}
			caps := report.GetCapabilityReport().GetCapabilities()
			checks := caps.GetHypervisorChecks()
			passed := func(id string) bool {
				i := slices.IndexFunc(checks, func(check *ownsandv1.HypervisorCheck) bool {
					return check.GetCheckId() == id
				})
				if i < 0 {
					t.Fatalf("missing check %s in %v", id, checks)
				}
				return checks[i].GetPassed()
			}
			if !passed("virtualization-framework") {
				t.Skip("Virtualization.framework is unavailable on this host")
			}
			if !passed("linux-base-image") || passed("virtualization-entitlement") != tc.wantSupport {
				t.Fatalf("unexpected checks: %v", checks)
			}
			guests, images := caps.GetGuestOses(), caps.GetBuiltBaseImages()
			if tc.wantSupport {
				if !slices.Contains(guests, ownsandv1.GuestOs_GUEST_OS_LINUX) || !slices.Contains(images, ownsandv1.BaseImage_BASE_IMAGE_LINUX) {
					t.Fatalf("missing Linux guest or image support: guests = %v, images = %v", guests, images)
				}
			} else if len(guests) != 0 || len(images) != 0 || caps.GetMacosGuestsSupported() {
				t.Fatalf("reported support with a disabled entitlement: %v", caps)
			}
			t.Logf("entitlement check passed = %t, guests = %v, images = %v", tc.wantSupport, guests, images)
			stopBinaryAgent(t, agent)
		})
	}
}
