// SPDX-License-Identifier: FSL-1.1-ALv2

package auth

import (
	"strings"
	"testing"
)

func TestSecrets(t *testing.T) {
	for _, kind := range []SecretKind{SecretAPIKey, SecretInvitation} {
		s, err := NewSecret(kind)
		if err != nil {
			t.Fatal(err)
		}
		prefix := kind.Prefix()
		if !strings.HasPrefix(s.Value, prefix) || len(s.Value) != len(prefix)+43 {
			t.Fatalf("%v: shape of %d bytes", kind, len(s.Value))
		}
		if strings.ContainsAny(s.Value[len(prefix):], "+/=") {
			t.Fatalf("%v: not unpadded base64url", kind)
		}
		if ClassifySecret(s.Value) != kind || !WellFormed(kind, s.Value) {
			t.Fatalf("%v: not recognized", kind)
		}
		if !HashEqual(s.Hash, HashSecret(s.Value)) || len(s.Hash) != 32 {
			t.Fatalf("%v: hash mismatch", kind)
		}
		if s.DisplayPrefix != s.Value[:len(prefix)+4] || DisplayPrefix(kind, s.Value) != s.DisplayPrefix {
			t.Fatalf("%v: display prefix %q", kind, s.DisplayPrefix)
		}
		// The two secrets of one kind differ.
		other, _ := NewSecret(kind)
		if other.Value == s.Value || HashEqual(other.Hash, s.Hash) {
			t.Fatalf("%v: repeated secret", kind)
		}
	}
	if ClassifySecret("eyJhbGciOi") != SecretUnknown {
		t.Fatal("a JWT is not a tide secret")
	}
	if ClassifySecret("tide_bad") != SecretAPIKey || ClassifySecret("tdi_bad") != SecretInvitation {
		t.Fatal("a malformed value is classified by its prefix")
	}
	good, _ := NewSecret(SecretAPIKey)
	for _, bad := range []string{
		"tide_short",
		"tide_" + strings.Repeat("=", 43),
		"tide_" + strings.Repeat("A", 44),
		// The last character of 32 bytes in base64url carries 4 zero bits;
		// a value with other bits there is not a canonical encoding.
		"tide_" + strings.Repeat("A", 42) + "B",
		good.Value[:len(good.Value)-1] + "+",
		"tdi_" + good.Value[5:],
		"TIDE_" + good.Value[5:],
	} {
		if WellFormed(SecretAPIKey, bad) {
			t.Errorf("WellFormed(%q) = true", bad)
		}
	}
	if WellFormed(SecretUnknown, good.Value) {
		t.Fatal("unknown kind accepted")
	}
	if HashEqual([]byte{1}, []byte{1, 2}) {
		t.Fatal("hashes of different lengths are equal")
	}
}
