// SPDX-License-Identifier: FSL-1.1-ALv2

package nonce

import (
	"errors"
	"testing"
	"time"
)

func TestNonce(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	dev := "0190a0c0-0000-7000-8000-000000000001"
	other := "0190a0c0-0000-7000-8000-000000000002"
	k := Keys{Current: []byte("0123456789abcdef0123456789abcdef")}
	n, exp, err := k.Issue(dev, now)
	if err != nil || !exp.Equal(now.Add(Lifetime)) {
		t.Fatal(err)
	}
	if _, err := k.Check(n, dev, now.Add(59*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Check(n, dev, now.Add(60*time.Second)); !errors.Is(err, ErrExpired) {
		t.Fatalf("expiry: %v", err)
	}
	if _, err := k.Check(n, other, now); !errors.Is(err, ErrDevice) {
		t.Fatalf("other device: %v", err)
	}
	bad := append([]byte(nil), n...)
	bad[20] ^= 1
	if _, err := k.Check(bad, dev, now); !errors.Is(err, ErrMAC) {
		t.Fatalf("tampered: %v", err)
	}
	if _, err := k.Check(n[:10], dev, now); !errors.Is(err, ErrMalformed) {
		t.Fatalf("short: %v", err)
	}
	rotated := Keys{Current: []byte("fedcba9876543210fedcba9876543210"), Previous: k.Current}
	if _, err := rotated.Check(n, dev, now); err != nil {
		t.Fatalf("previous key: %v", err)
	}
	if _, err := (Keys{Current: rotated.Current}).Check(n, dev, now); !errors.Is(err, ErrMAC) {
		t.Fatalf("after the overlap: %v", err)
	}
	n2, _, _ := k.Issue(dev, now)
	if string(n2) == string(n) {
		t.Fatal("nonces repeat")
	}
}
