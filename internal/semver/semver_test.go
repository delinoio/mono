// SPDX-License-Identifier: Apache-2.0

package semver

import "testing"

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.2.3", "1.2.3", 0},
		{"v1.2.3", "1.2.4", -1},
		{"1.10.0", "1.9.9", 1},
		{"1.0.0-rc.1", "1.0.0", -1},
		{"0.0.0-dev", "0.1.0", -1},
		{"1.10.0", "1.9.0", 1},
		{"1.9.0", "1.10.0", -1},
		{"v1.9.0", "1.9.0", 0},
		{"1.9.0-rc.1", "1.9.0", -1},
	}
	for _, c := range cases {
		got, err := CompareStrings(c.a, c.b)
		if err != nil || got != c.want {
			t.Errorf("Compare(%s, %s) = %d, %v", c.a, c.b, got, err)
		}
	}
	if _, err := Parse("1.2"); err == nil {
		t.Fatal("short version accepted")
	}
}

func TestParseRejectsInvalidVersions(t *testing.T) {
	for _, s := range []string{"", "dev", "1.2", "1.2.3.4", "1.x.3", "1.-2.3", "latest",
		"+1.2.3", "1.+2.3", "1.2.+3", "v+1.2.3", "01.2.3", "1.02.3", "1.2.03", "v01.2.3",
		"1.2.3-", "v1.2.3-", "vv1.2.3", "1..3", ".1.2", "1.2.", "1.2.3 ", "1.2.٣",
		"99999999999999999999.0.0"} {
		if _, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) accepted", s)
		}
		if _, err := CompareStrings(s, "1.0.0"); err == nil {
			t.Errorf("CompareStrings(%q, 1.0.0) accepted", s)
		}
	}
}

func TestParsePreservesValidVersions(t *testing.T) {
	cases := []struct {
		in   string
		want Semver
	}{
		{"1.2.3", Semver{1, 2, 3, ""}},
		{"v1.2.3", Semver{1, 2, 3, ""}},
		{"0.0.0", Semver{0, 0, 0, ""}},
		{"0.0.0-dev", Semver{0, 0, 0, "dev"}},
		{"1.2.3-rc.1", Semver{1, 2, 3, "rc.1"}},
		{"1.10.0", Semver{1, 10, 0, ""}},
		{"1.9.0-rc.1", Semver{1, 9, 0, "rc.1"}},
		{"10.20.30", Semver{10, 20, 30, ""}},
	}
	for _, c := range cases {
		got, err := Parse(c.in)
		if err != nil || got != c.want {
			t.Errorf("Parse(%q) = %+v, %v; want %+v", c.in, got, err, c.want)
		}
	}
}
