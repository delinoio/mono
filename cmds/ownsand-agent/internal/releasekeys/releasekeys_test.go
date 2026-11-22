// SPDX-License-Identifier: FSL-1.1-ALv2

package releasekeys

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
)

func TestBuiltinKeysDecode(t *testing.T) {
	keys, err := PublicKeys()
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != len(encoded) {
		t.Fatalf("decoded %d keys from %d entries", len(keys), len(encoded))
	}
	seen := map[string]bool{}
	for _, value := range encoded {
		if seen[value] {
			t.Fatalf("key %s is listed twice", value)
		}
		seen[value] = true
	}
}

func TestDecodeRoundTrip(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := Decode([]string{Encode(pub)})
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || !keys[0].Equal(pub) {
		t.Fatalf("Decode(Encode(key)) = %v", keys)
	}
}

func TestDecodeRejectsInvalidKeys(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{
		"",
		"not base64",
		Encode(pub[:31]),
		Encode(append(append([]byte{}, pub...), 0)),
		// The list uses the padded standard alphabet only.
		Encode(pub)[:43],
	} {
		if _, err := Decode([]string{value}); err == nil {
			t.Errorf("Decode(%q) accepted an invalid key", value)
		}
	}
}
