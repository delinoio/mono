// SPDX-License-Identifier: FSL-1.1-ALv2

package imagebuild

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

	// The empty input resolves per host: the host guest OS and no error where
	// the host can build images, and an unspecified guest OS with
	// ErrUnsupported elsewhere. Both values are checked independently, so a
	// wrong value fails even when the other one is right.
	var (
		wantOS          ownsandv1.GuestOs
		wantUnsupported bool
	)
	switch runtime.GOOS {
	case "darwin":
		wantOS = ownsandv1.GuestOs_GUEST_OS_MACOS
	case "windows":
		wantOS = ownsandv1.GuestOs_GUEST_OS_WINDOWS
	default:
		wantOS = ownsandv1.GuestOs_GUEST_OS_UNSPECIFIED
		wantUnsupported = true
	}
	got, err := ParseGuestOS("")
	gotUnsupported := errors.Is(err, ErrUnsupported)
	if got != wantOS || gotUnsupported != wantUnsupported || (err != nil) != wantUnsupported {
		t.Fatalf("ParseGuestOS(\"\") on %s = (%v, unsupported=%t, err=nil:%t); want (%v, unsupported=%t, err=nil:%t)",
			runtime.GOOS, got, gotUnsupported, err == nil, wantOS, wantUnsupported, !wantUnsupported)
	}
}

// minimalWindowsAMD64PE is the smallest image debug/pe accepts as an amd64
// executable: an MZ header whose e_lfanew (offset 0x3c) points at 0x40, the
// "PE\0\0" signature, and a COFF file header with machine 0x8664 and no
// sections or optional header.
func minimalWindowsAMD64PE() []byte {
	b := make([]byte, 96)
	b[0], b[1] = 'M', 'Z'
	b[0x3c] = 0x40
	copy(b[0x40:], "PE\x00\x00")
	b[0x44], b[0x45] = 0x64, 0x86 // IMAGE_FILE_MACHINE_AMD64, little endian
	return b
}

// TestBuildChecksTheGuestBinary runs on every host without an ISO, an IPSW or
// a hypervisor: each case fails inside Build before any image work starts,
// and each asserts the exact diagnostic so that a skipped check cannot pass
// on the error that the unsupported-host stubs return.
func TestBuildChecksTheGuestBinary(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "ownsand-guest")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	pePath := filepath.Join(dir, "ownsand-guest.exe")
	if err := os.WriteFile(pePath, minimalWindowsAMD64PE(), 0o755); err != nil {
		t.Fatal(err)
	}
	// The missing-ISO case is only meaningful when the helper itself is valid.
	if err := checkWindowsAMD64(pePath); err != nil {
		t.Fatalf("the PE fixture is not accepted as a Windows amd64 executable: %v", err)
	}
	if err := checkWindowsAMD64(script); err == nil {
		t.Fatal("a script was accepted as a Windows executable")
	}
	if err := checkDarwinARM64(script); err == nil {
		t.Fatal("a script was accepted as a macOS executable")
	}

	tests := []struct {
		name       string
		opts       Options
		wantPrefix string
		wantExact  string
	}{
		{
			name:       "macOS rejects a script as the guest helper",
			opts:       Options{GuestOS: ownsandv1.GuestOs_GUEST_OS_MACOS, IPSW: filepath.Join(dir, "missing.ipsw"), GuestBinary: script},
			wantPrefix: "imagebuild: the guest helper " + script + " is not a macOS executable: ",
		},
		{
			name:       "Windows rejects a script as the guest helper",
			opts:       Options{GuestOS: ownsandv1.GuestOs_GUEST_OS_WINDOWS, ISO: filepath.Join(dir, "missing.iso"), GuestBinary: script},
			wantPrefix: "imagebuild: the guest helper " + script + " is not a Windows executable: ",
		},
		{
			name:      "Windows without an ISO is rejected with a valid helper",
			opts:      Options{GuestOS: ownsandv1.GuestOs_GUEST_OS_WINDOWS, GuestBinary: pePath},
			wantExact: "imagebuild: a Windows image build needs --iso <path to a Windows ISO>",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.opts.ImagesDir = t.TempDir()
			err := Build(context.Background(), tt.opts)
			if err == nil {
				t.Fatal("Build succeeded")
			}
			if tt.wantExact != "" && err.Error() != tt.wantExact {
				t.Fatalf("Build error = %q; want exactly %q", err.Error(), tt.wantExact)
			}
			if !strings.HasPrefix(err.Error(), tt.wantPrefix) {
				t.Fatalf("Build error = %q; want prefix %q", err.Error(), tt.wantPrefix)
			}
		})
	}
}
