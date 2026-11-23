// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The tool refuses a malformed pre-release version, and a manifest whose
// revoked_versions or releases hold one, so that such an entry cannot sit in
// a signed manifest and be compared equal to a valid version.
func TestMalformedPreReleaseVersionsAreRefused(t *testing.T) {
	sha := strings.Repeat("a", 64)
	for _, v := range []string{"1.2.3-rc.", "1.2.3-.rc", "1.2.3-rc.01", "1.2.3-rc_1"} {
		revoked := `{"schema":1,"releases":[],"revoked_versions":["` + v + `"]}`
		if _, err := decodeManifest([]byte(revoked)); err == nil {
			t.Errorf("decodeManifest accepted revoked version %q", v)
		}
		listed := `{"schema":1,"releases":[{"version":"` + v + `","default":true,"artifacts":[{"platform":"linux-amd64","url":"https://example.test/a","sha256":"` + sha + `"}]}],"revoked_versions":[]}`
		if _, err := decodeManifest([]byte(listed)); err == nil {
			t.Errorf("decodeManifest accepted release version %q", v)
		}
		if err := checkReleaseVersion(v); err == nil {
			t.Errorf("checkReleaseVersion(%q) accepted", v)
		}

		f := newFixture(t)
		art := f.artifacts(t, "a", "x", PlatformLinuxAmd64)
		out := filepath.Join(f.dir, "out.json")
		if err := f.addRelease(t, "", v, art, out, "linux-amd64"); err == nil {
			t.Errorf("add-release accepted %q", v)
		}
		if _, err := os.Stat(out); !os.IsNotExist(err) {
			t.Errorf("a rejected release %q wrote %s", v, out)
		}
	}
}
