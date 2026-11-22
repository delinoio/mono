// SPDX-License-Identifier: FSL-1.1-ALv2

package secrets

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func randomKey(t *testing.T) []byte {
	t.Helper()
	b := make([]byte, KeySize)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSealOpenRoundTrip(t *testing.T) {
	k, err := GenerateKey("k1")
	if err != nil {
		t.Fatal(err)
	}
	ring, err := NewKeyring(k)
	if err != nil {
		t.Fatal(err)
	}
	ad := AssociatedData("ownsand_link", "org-1")
	s, err := ring.Seal([]byte("osk_secret"), ad)
	if err != nil {
		t.Fatal(err)
	}
	if s.KeyID != "k1" || bytes.Contains(s.Ciphertext, []byte("osk_secret")) {
		t.Fatalf("sealed = %+v", s)
	}
	got, err := ring.Open(s, ad)
	if err != nil || string(got) != "osk_secret" {
		t.Fatalf("open = %q, %v", got, err)
	}
	// Other associated data, a changed byte and a truncated value fail.
	if _, err := ring.Open(s, AssociatedData("ownsand_link", "org-2")); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("other associated data: %v", err)
	}
	tampered := Sealed{KeyID: s.KeyID, Ciphertext: append([]byte(nil), s.Ciphertext...)}
	tampered.Ciphertext[len(tampered.Ciphertext)-1] ^= 1
	if _, err := ring.Open(tampered, ad); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("tampered: %v", err)
	}
	if _, err := ring.Open(Sealed{KeyID: "k1", Ciphertext: s.Ciphertext[:5]}, ad); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("truncated: %v", err)
	}
	// Two seals of the same value differ (random nonce).
	s2, _ := ring.Seal([]byte("osk_secret"), ad)
	if bytes.Equal(s.Ciphertext, s2.Ciphertext) {
		t.Fatal("nonce reused")
	}
}

func TestAssociatedDataSeparatesParts(t *testing.T) {
	if bytes.Equal(AssociatedData("a", "bc"), AssociatedData("ab", "c")) {
		t.Fatal("associated data parts are ambiguous")
	}
}

func TestKeyRotation(t *testing.T) {
	old, _ := GenerateKey("2026-01")
	next, _ := GenerateKey("2026-06")
	ad := AssociatedData("agent_integration", "i-1", "environment_key")

	before, _ := NewKeyring(old)
	sealed, err := before.Seal([]byte("value"), ad)
	if err != nil {
		t.Fatal(err)
	}

	// The operator adds the new key in front of the old one.
	during, err := NewKeyring(next, old)
	if err != nil {
		t.Fatal(err)
	}
	if during.PrimaryKeyID() != "2026-06" || strings.Join(during.KeyIDs(), ",") != "2026-06,2026-01" {
		t.Fatalf("keys = %s / %v", during.PrimaryKeyID(), during.KeyIDs())
	}
	if got, err := during.Open(sealed, ad); err != nil || string(got) != "value" {
		t.Fatalf("old value with both keys: %q, %v", got, err)
	}
	if !during.NeedsReseal(sealed) {
		t.Fatal("old value must need a reseal")
	}
	resealed, changed, err := during.Reseal(sealed, ad)
	if err != nil || !changed || resealed.KeyID != "2026-06" {
		t.Fatalf("reseal = %+v, %v, %v", resealed, changed, err)
	}
	again, changed, err := during.Reseal(resealed, ad)
	if err != nil || changed || !bytes.Equal(again.Ciphertext, resealed.Ciphertext) {
		t.Fatalf("second reseal = %v, %v", changed, err)
	}

	// After the old key is removed, only the resealed value opens.
	after, _ := NewKeyring(next)
	if got, err := after.Open(resealed, ad); err != nil || string(got) != "value" {
		t.Fatalf("resealed after removal: %q, %v", got, err)
	}
	if _, err := after.Open(sealed, ad); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("old value after removal: %v", err)
	}
	if _, _, err := after.Reseal(sealed, ad); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("reseal of an unknown key: %v", err)
	}
}

func TestSameKeyIDWithOtherMaterialFails(t *testing.T) {
	a, _ := GenerateKey("k")
	b, _ := GenerateKey("k")
	ra, _ := NewKeyring(a)
	rb, _ := NewKeyring(b)
	s, _ := ra.Seal([]byte("v"), nil)
	if _, err := rb.Open(s, nil); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("open with other material: %v", err)
	}
}

func TestParseKeys(t *testing.T) {
	k1, k2 := randomKey(t), randomKey(t)
	value := " new:" + base64.StdEncoding.EncodeToString(k1) + " , old:" + base64.RawURLEncoding.EncodeToString(k2) + ","
	ring, err := ParseKeys(value)
	if err != nil {
		t.Fatal(err)
	}
	if ring.PrimaryKeyID() != "new" || strings.Join(ring.KeyIDs(), ",") != "new,old" {
		t.Fatalf("keys = %v", ring.KeyIDs())
	}
	// A value sealed by the parsed ring opens with a ring built from the
	// same material.
	key, _ := NewKey("new", k1)
	direct, _ := NewKeyring(key)
	s, _ := ring.Seal([]byte("x"), nil)
	if got, err := direct.Open(s, nil); err != nil || string(got) != "x" {
		t.Fatalf("open = %q, %v", got, err)
	}

	secretText := base64.StdEncoding.EncodeToString(k1)
	for _, bad := range []string{
		"",
		" , ",
		"nokeyid",
		"k:" + base64.StdEncoding.EncodeToString(k1[:16]),
		"k:not base64!",
		"bad id:" + secretText,
		"k:" + secretText + ",k:" + secretText,
		strings.Repeat("x", 33) + ":" + secretText,
	} {
		_, err := ParseKeys(bad)
		if err == nil {
			t.Errorf("ParseKeys(%q) succeeded", bad)
			continue
		}
		if strings.Contains(err.Error(), secretText) {
			t.Errorf("error for %q contains the key", bad)
		}
	}
}

func TestEnvelope(t *testing.T) {
	k, _ := GenerateKey("k1")
	ring, _ := NewKeyring(k)
	s, _ := ring.Seal([]byte("v"), nil)
	b, err := s.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	var back Sealed
	if err := back.UnmarshalBinary(b); err != nil || back.KeyID != "k1" || !bytes.Equal(back.Ciphertext, s.Ciphertext) {
		t.Fatalf("unmarshal = %+v, %v", back, err)
	}
	if got, err := ring.Open(back, nil); err != nil || string(got) != "v" {
		t.Fatalf("open = %q, %v", got, err)
	}
	for _, bad := range [][]byte{nil, {1}, {2, 2, 'k', '1'}, {1, 5, 'k'}, {1, 1, ' '}} {
		var s Sealed
		if err := s.UnmarshalBinary(bad); !errors.Is(err, ErrMalformed) {
			t.Errorf("UnmarshalBinary(%v) = %v", bad, err)
		}
	}
	if _, err := (Sealed{KeyID: ""}).MarshalBinary(); !errors.Is(err, ErrMalformed) {
		t.Fatalf("marshal without key ID: %v", err)
	}
}
