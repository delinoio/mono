// SPDX-License-Identifier: FSL-1.1-ALv2

package identity

import (
	"crypto/ed25519"
	"testing"
)

func TestSignVerifies(t *testing.T) {
	k, seed, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	k2, err := FromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	nonce := []byte{1, 2, 3}
	sig := k2.Sign(PurposeAuthenticate, nonce)
	if !ed25519.Verify(k.PublicKey(), append([]byte("ownsand-authenticate"), nonce...), sig) {
		t.Fatal("signature does not verify over label+nonce")
	}
	if ed25519.Verify(k.PublicKey(), Message(PurposeConfirmRemoval, nonce), sig) {
		t.Fatal("signature verifies for another purpose")
	}
	if _, err := FromSeed([]byte{1}); err == nil {
		t.Fatal("short seed accepted")
	}
}
