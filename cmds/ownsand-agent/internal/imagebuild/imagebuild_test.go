// SPDX-License-Identifier: Apache-2.0

package imagebuild

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func TestParseGuestOS(t *testing.T) {
	for in, want := range map[string]ownsandv1.GuestOs{
		"macos":   ownsandv1.GuestOs_GUEST_OS_MACOS,
		"windows": ownsandv1.GuestOs_GUEST_OS_WINDOWS,
	} {
		if got, err := ParseGuestOS(in); err != nil || got != want {
			t.Fatalf("ParseGuestOS(%q) = %v %v", in, got, err)
		}
	}
	if _, err := ParseGuestOS("linux"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("linux: %v", err)
	}
	if _, err := ParseGuestOS("plan9"); err == nil || errors.Is(err, ErrUnsupported) {
		t.Fatalf("unknown guest OS: %v", err)
	}
	if got, err := ParseGuestOS(""); got != DefaultGuestOS() && err == nil {
		t.Fatalf("default guest OS %v", got)
	}
}

func TestBuildChecksTheGuestBinary(t *testing.T) {
	dir := t.TempDir()
	notExe := filepath.Join(dir, "ownsand-guest")
	if err := os.WriteFile(notExe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, guest := range []ownsandv1.GuestOs{ownsandv1.GuestOs_GUEST_OS_MACOS, ownsandv1.GuestOs_GUEST_OS_WINDOWS} {
		err := Build(context.Background(), Options{ImagesDir: dir, GuestOS: guest, ISO: "x.iso", GuestBinary: notExe})
		if err == nil {
			t.Fatalf("%v: a script was accepted as the guest helper", guest)
		}
	}
	if err := Build(context.Background(), Options{ImagesDir: dir, GuestOS: ownsandv1.GuestOs_GUEST_OS_WINDOWS, GuestBinary: notExe}); err == nil {
		t.Fatal("a Windows build without an ISO started")
	}
}
