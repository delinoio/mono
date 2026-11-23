// SPDX-License-Identifier: FSL-1.1-ALv2

package guest

import (
	"strings"
	"testing"
)

func TestExecEnvWindows(t *testing.T) {
	t.Setenv("OwnsandGuestTestVar", "helper")
	t.Setenv("OwnsandGuestOther", "kept")
	env := commandEnv(map[string]string{"OWNSANDGUESTTESTVAR": "request"})
	var sawRequest, sawOther bool
	for _, kv := range env {
		switch {
		case strings.EqualFold(kv, "OwnsandGuestTestVar=request"):
			sawRequest = kv == "OWNSANDGUESTTESTVAR=request"
		case strings.EqualFold(kv, "OwnsandGuestTestVar=helper"):
			t.Fatalf("inherited spelling survived: %q", kv)
		case kv == "OwnsandGuestOther=kept":
			sawOther = true
		}
	}
	if !sawRequest || !sawOther {
		// Report only the two booleans: env is the merged process environment
		// and inherits whatever credentials the runner carries, so it must not
		// reach test logs.
		t.Fatalf("request override kept=%v, unrelated variable kept=%v", sawRequest, sawOther)
	}
}
