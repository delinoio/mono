// SPDX-License-Identifier: FSL-1.1-ALv2

package linux

import (
	"os"
	"strings"
	"testing"
)

// TestMain removes every STUB_* variable from the test process environment.
// The helpers in this package start bash scripts against stub commands that
// read their scenario from STUB_* variables, and they build the script
// environment from os.Environ(). A STUB_* variable exported in the caller's
// shell would otherwise leak into every scenario and change its outcome. Each
// test selects its scenario by passing its own STUB_* values to the helper, so
// clearing the inherited ones here leaves the explicit scenarios unchanged.
// Doing it once for the package also covers a helper that a later change adds.
func TestMain(m *testing.M) {
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "STUB_") {
			if err := os.Unsetenv(name); err != nil {
				panic("unset inherited stub control: " + name)
			}
		}
	}
	os.Exit(m.Run())
}
