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
		t.Fatalf("request=%v other=%v env=%q", sawRequest, sawOther, env)
	}
}
