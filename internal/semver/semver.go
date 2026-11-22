// SPDX-License-Identifier: Apache-2.0

// Package semver parses and compares MAJOR.MINOR.PATCH release versions
// with an optional pre-release suffix.
package semver

import (
	"fmt"
	"strconv"
	"strings"
)

// Semver is a parsed MAJOR.MINOR.PATCH version with an optional
// pre-release suffix.
type Semver struct {
	Major, Minor, Patch int
	Pre                 string
}

// Parse parses "1.2.3", "v1.2.3" or "1.2.3-rc.1". Each numeric component
// must be a non-empty run of ASCII digits without a leading zero (a lone
// "0" is valid) that fits in an int, and a "-" must be followed by a
// non-empty pre-release suffix. A sign, a zero-padded component or an
// empty suffix is rejected, so that distinct strings do not compare equal.
func Parse(s string) (Semver, error) {
	s = strings.TrimPrefix(s, "v")
	core, pre, hasPre := strings.Cut(s, "-")
	if hasPre && pre == "" {
		return Semver{}, fmt.Errorf("semver: %q is not MAJOR.MINOR.PATCH", s)
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return Semver{}, fmt.Errorf("semver: %q is not MAJOR.MINOR.PATCH", s)
	}
	var nums [3]int
	for i, p := range parts {
		n, ok := parseComponent(p)
		if !ok {
			return Semver{}, fmt.Errorf("semver: %q is not MAJOR.MINOR.PATCH", s)
		}
		nums[i] = n
	}
	return Semver{Major: nums[0], Minor: nums[1], Patch: nums[2], Pre: pre}, nil
}

// parseComponent parses one numeric version component: ASCII digits only,
// no leading zero unless the component is "0", and a value that fits in an
// int. strconv.Atoi alone would accept a sign and leading zeros.
func parseComponent(p string) (int, bool) {
	if p == "" || (len(p) > 1 && p[0] == '0') {
		return 0, false
	}
	for i := 0; i < len(p); i++ {
		if p[i] < '0' || p[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(p)
	if err != nil {
		return 0, false
	}
	return n, true
}

// Compare returns -1, 0 or 1. A pre-release sorts before its release;
// pre-release suffixes compare as strings.
func Compare(a, b Semver) int {
	for _, d := range [3][2]int{{a.Major, b.Major}, {a.Minor, b.Minor}, {a.Patch, b.Patch}} {
		if d[0] != d[1] {
			if d[0] < d[1] {
				return -1
			}
			return 1
		}
	}
	switch {
	case a.Pre == b.Pre:
		return 0
	case a.Pre == "":
		return 1
	case b.Pre == "":
		return -1
	case a.Pre < b.Pre:
		return -1
	}
	return 1
}

// CompareStrings compares two version strings.
func CompareStrings(a, b string) (int, error) {
	va, err := Parse(a)
	if err != nil {
		return 0, err
	}
	vb, err := Parse(b)
	if err != nil {
		return 0, err
	}
	return Compare(va, vb), nil
}
