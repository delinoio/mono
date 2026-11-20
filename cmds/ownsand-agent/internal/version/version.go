// SPDX-License-Identifier: Apache-2.0

// Package version holds the agent version and compares release versions.
package version

import (
	"fmt"
	"strconv"
	"strings"
)

// Version is the running agent version. Release builds set it with
// -ldflags "-X github.com/delinoio/mono/cmds/ownsand-agent/internal/version.Version=<version>".
var Version = "0.0.0-dev"

// Semver is a parsed MAJOR.MINOR.PATCH version with an optional
// pre-release suffix.
type Semver struct {
	Major, Minor, Patch int
	Pre                 string
}

// Parse parses "1.2.3", "v1.2.3" or "1.2.3-rc.1".
func Parse(s string) (Semver, error) {
	s = strings.TrimPrefix(s, "v")
	core, pre, _ := strings.Cut(s, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return Semver{}, fmt.Errorf("version: %q is not MAJOR.MINOR.PATCH", s)
	}
	var nums [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return Semver{}, fmt.Errorf("version: %q is not MAJOR.MINOR.PATCH", s)
		}
		nums[i] = n
	}
	return Semver{Major: nums[0], Minor: nums[1], Patch: nums[2], Pre: pre}, nil
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
