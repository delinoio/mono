// SPDX-License-Identifier: FSL-1.1-ALv2

package store

import (
	"os"
	"testing"
)

func pendingIdentity() *Identity {
	return &Identity{PrivateKeySeed: []byte("0123456789abcdef0123456789abcdef")}
}

// The file keeps its format and mode.
func TestSaveIdentityDurableKeepsFormat(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := pendingIdentity()
	if err := s.SaveIdentity(id); err != nil {
		t.Fatal(err)
	}
	plain, err := os.ReadFile(s.Path(IdentityFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveIdentityDurable(id); err != nil {
		t.Fatal(err)
	}
	durable, err := os.ReadFile(s.Path(IdentityFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(plain) != string(durable) {
		t.Fatalf("durable file differs from SaveIdentity output:\n%s\nvs\n%s", durable, plain)
	}
}
