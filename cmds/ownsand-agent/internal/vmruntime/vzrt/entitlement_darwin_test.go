// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build darwin && cgo

package vzrt

import (
	"path/filepath"
	"testing"
)

func TestVirtualizationEntitlementPlist(t *testing.T) {
	const key = "<key>com.apple.security.virtualization</key>"
	for _, tc := range []struct {
		name, body string
		enabled    bool
		malformed  bool
	}{
		{name: "true", body: key + "<true/>", enabled: true},
		{name: "false", body: key + "<false/>"},
		{name: "missing"},
		{name: "string", body: key + "<string>true</string>"},
		{name: "nested", body: "<key>other</key><dict>" + key + "<true/></dict>"},
		{name: "wrong key", body: "<key>com.apple.security.virtualization.other</key><true/>"},
		{name: "malformed", body: key + "<true", malformed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plist := `<?xml version="1.0" encoding="UTF-8"?>` +
				`<plist version="1.0"><dict>` + tc.body + `</dict></plist>`
			enabled, err := virtualizationEntitlementEnabled(t.Context(), []byte(plist))
			if enabled != tc.enabled || (err != nil) != tc.malformed {
				t.Fatalf("enabled = %t, error = %v; want enabled = %t, malformed = %t", enabled, err, tc.enabled, tc.malformed)
			}
		})
	}
}

func TestInspectVirtualizationEntitlementFailed(t *testing.T) {
	enabled, detail := inspectVirtualizationEntitlement(t.Context(), filepath.Join(t.TempDir(), "missing"))
	if enabled || detail != "cannot inspect the signed entitlements of the agent executable" {
		t.Fatalf("enabled = %t, detail = %q; want inspection failure", enabled, detail)
	}
}
