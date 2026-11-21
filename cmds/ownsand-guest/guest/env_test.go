// SPDX-License-Identifier: FSL-1.1-ALv2

package guest

import (
	"strings"
	"testing"
)

func TestMergeEnvCaseInsensitiveFold(t *testing.T) {
	inherited := []string{"OwnsandGuestTestVar=helper", "KEEP=1", "=C:=C:\\work"}
	got := mergeEnv(inherited, map[string]string{"OWNSANDGUESTTESTVAR": "request"}, strings.ToLower)
	want := []string{"=C:=C:\\work", "KEEP=1", "OWNSANDGUESTTESTVAR=request"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestMergeEnvCaseSensitiveIdentity(t *testing.T) {
	identity := func(k string) string { return k }
	inherited := []string{"OwnsandGuestTestVar=helper", "KEEP=1"}
	got := mergeEnv(inherited, map[string]string{"OWNSANDGUESTTESTVAR": "request"}, identity)
	want := []string{"KEEP=1", "OWNSANDGUESTTESTVAR=request", "OwnsandGuestTestVar=helper"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("different spellings: got %q, want %q", got, want)
	}
	got = mergeEnv(inherited, map[string]string{"OwnsandGuestTestVar": "request"}, identity)
	want = []string{"KEEP=1", "OwnsandGuestTestVar=request"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("exact override: got %q, want %q", got, want)
	}
}
