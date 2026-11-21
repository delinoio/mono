// SPDX-License-Identifier: Apache-2.0

package version

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
