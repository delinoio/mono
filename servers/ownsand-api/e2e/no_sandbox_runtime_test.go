// SPDX-License-Identifier: FSL-1.1-ALv2

package e2e

import (
	"strings"
	"testing"
)

// TestNoSandboxProcessRuntimeSkip checks that a stack that creates no sandbox
// is skipped only on hosts without the process runtime, so Linux and macOS
// hosts still run it.
func TestNoSandboxProcessRuntimeSkip(t *testing.T) {
	for _, tc := range []struct {
		goos   string
		reason string
	}{
		{goos: "linux"},
		{goos: "darwin"},
		{goos: "windows", reason: "windows host"},
	} {
		got := noSandboxProcessRuntimeSkip(tc.goos)
		if (tc.reason == "") != (got == "") || !strings.Contains(got, tc.reason) {
			t.Errorf("noSandboxProcessRuntimeSkip(%q) = %q, want a reason containing %q", tc.goos, got, tc.reason)
		}
	}
}
