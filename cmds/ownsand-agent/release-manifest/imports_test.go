// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// The signing step runs this tool next to the release signing key, so it
// must not link a third-party module (cmds/AGENTS.md).
func TestImportsOnlyTheStandardLibrary(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go list")
	}
	out, err := exec.Command("go", "list", "-buildvcs=false", "-deps", "-f", "{{if not .Standard}}{{.ImportPath}}{{end}}", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	allowed := []string{
		"github.com/delinoio/mono/cmds/ownsand-agent/internal/releasekeys",
		"github.com/delinoio/mono/cmds/ownsand-agent/release-manifest",
		"github.com/delinoio/mono/internal/semver",
	}
	for _, path := range strings.Fields(string(out)) {
		if !slices.Contains(allowed, path) {
			t.Errorf("release-manifest links %s, which is not the standard library or an allowed repository package", path)
		}
	}
}
