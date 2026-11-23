// SPDX-License-Identifier: Apache-2.0

package semver

import "testing"

func TestParseRejectsInvalidPreReleaseIdentifiers(t *testing.T) {
	for _, s := range []string{
		"1.2.3-rc.", "1.2.3-.rc", "1.2.3-rc..1", "1.2.3-.", "v1.2.3-rc.",
		"1.2.3-rc_1", "1.2.3-rc+1", "1.2.3-rc 1", "1.2.3-ré", "1.2.3-rc.٣",
		"1.2.3-rc.01", "1.2.3-01", "1.2.3-rc.1.00", "1.2.3-00",
	} {
		if _, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) accepted", s)
		}
		if _, err := CompareStrings(s, "1.2.3-rc"); err == nil {
			t.Errorf("CompareStrings(%q, 1.2.3-rc) accepted", s)
		}
	}
}

func TestParseAcceptsValidPreReleaseIdentifiers(t *testing.T) {
	for _, c := range []struct{ in, pre string }{
		{"1.2.3-0", "0"},
		{"1.2.3-rc.0", "rc.0"},
		{"1.2.3-rc.1-x", "rc.1-x"},
		{"1.2.3-alpha-1", "alpha-1"},
		{"1.2.3-0a", "0a"},
		{"1.2.3--", "-"},
		{"1.2.3-rc.10.A", "rc.10.A"},
		{"v1.2.3-x.0.y", "x.0.y"},
	} {
		got, err := Parse(c.in)
		if err != nil {
			t.Errorf("Parse(%q) rejected", c.in)
			continue
		}
		if got.Pre != c.pre {
			t.Errorf("Parse(%q).Pre = %q, want %q", c.in, got.Pre, c.pre)
		}
	}
}
