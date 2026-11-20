// SPDX-License-Identifier: FSL-1.1-ALv2

package secret

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestFormats(t *testing.T) {
	for _, kind := range []Kind{KindAPIKey, KindEnrollmentToken, KindInvitation, KindDeviceSession} {
		v := New(kind)
		if !strings.HasPrefix(v, kind.Prefix()) {
			t.Fatalf("%q lacks prefix %q", v, kind.Prefix())
		}
		raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(v, kind.Prefix()))
		if err != nil || len(raw) != RandomBytes {
			t.Fatalf("%q: %d bytes, %v", v, len(raw), err)
		}
		if Classify(v) != kind {
			t.Fatalf("classify %q", v)
		}
		if New(kind) == v {
			t.Fatal("secrets repeat")
		}
		if len(Hash(v)) != 32 || !Equal(Hash(v), Hash(v)) || Equal(Hash(v), Hash(v+"x")) {
			t.Fatal("hash")
		}
		if p := DisplayPrefix(v); len(p) != DisplayPrefixLen || !strings.HasPrefix(v, p) {
			t.Fatalf("display prefix %q", p)
		}
	}
	for _, bad := range []string{"", "osk_", "osk_short", "xyz_" + strings.Repeat("A", 43), "osk_" + strings.Repeat("A", 42) + "=", "osk_" + strings.Repeat("+", 43)} {
		if Classify(bad) != KindUnknown {
			t.Errorf("%q classified", bad)
		}
	}
}
