// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
)

func TestImageBuildInputPreflight(t *testing.T) {
	clearAgentEnv(t)
	root := t.TempDir()
	write := func(name string, data []byte) string {
		t.Helper()
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	text := write("text-helper", []byte("not an executable"))
	missing := filepath.Join(root, "missing-helper")
	iso := write("placeholder.iso", nil)

	valid := write("valid-windows.exe", windowsImageBuildTestPE(0x8664))
	wrongWindows := write("wrong-windows.exe", windowsImageBuildTestPE(0x14c))

	// Mach-O executable header for x86_64.
	mach := make([]byte, 32)
	binary.LittleEndian.PutUint32(mach[0:], 0xfeedfacf)
	binary.LittleEndian.PutUint32(mach[4:], 0x01000007)
	binary.LittleEndian.PutUint32(mach[12:], 2)
	wrongMac := write("wrong-macos", mach)

	cases := []struct {
		name string
		args []string
		code int
		diag string
	}{
		{"missing ISO", []string{"--os", "windows", "--guest-binary", missing}, exitUsage, "needs --iso"},
		{"Windows format", []string{"--os", "windows", "--iso", iso, "--guest-binary", text}, exitUsage, "is not a Windows executable"},
		{"macOS format", []string{"--os", "macos", "--guest-binary", text}, exitUsage, "is not a macOS executable"},
		{"Windows architecture", []string{"--os", "windows", "--iso", iso, "--guest-binary", wrongWindows}, exitUsage, "Windows guests need GOOS=windows GOARCH=amd64"},
		{"macOS architecture", []string{"--os", "macos", "--guest-binary", wrongMac}, exitUsage, "macOS guests need arm64"},
		{"missing Windows helper", []string{"--os", "windows", "--iso", iso, "--guest-binary", missing}, exitError, "open " + missing},
		{"missing macOS helper", []string{"--os", "macos", "--guest-binary", missing}, exitError, "open " + missing},
		{"unreadable Windows helper", []string{"--os", "windows", "--iso", iso, "--guest-binary", root}, exitError, "is not a Windows executable"},
		{"unreadable macOS helper", []string{"--os", "macos", "--guest-binary", root}, exitError, "is not a macOS executable"},
	}
	invoke := func(t *testing.T, state string, args []string, code int, diagnostic string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		args = append([]string{"image", "build", "--state-dir", state}, args...)
		if got := run(args, nil, &stdout, &stderr); got != code || !strings.Contains(stderr.String(), diagnostic) {
			t.Fatalf("%v: exit %d, stderr %q; want exit %d and diagnostic %q", args, got, stderr.String(), code, diagnostic)
		}
	}

	absent := filepath.Join(root, "absent")
	existing := filepath.Join(root, "existing")
	if err := os.Mkdir(existing, 0o755); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(existing, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	sentinel := filepath.Join(existing, "sentinel")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	file := write("state-file", []byte("keep"))
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			invoke(t, absent, tc.args, tc.code, tc.diag)
			if _, err := os.Lstat(absent); !os.IsNotExist(err) {
				t.Fatalf("absent state path changed: %v", err)
			}

			invoke(t, existing, tc.args, tc.code, tc.diag)
			entries, err := os.ReadDir(existing)
			if err != nil || len(entries) != 1 || entries[0].Name() != "sentinel" {
				t.Fatalf("state entries changed: %v, %v", entries, err)
			}
			if data, err := os.ReadFile(sentinel); err != nil || string(data) != "keep" {
				t.Fatalf("state sentinel changed: %q, %v", data, err)
			}
			if runtime.GOOS != "windows" {
				info, err := os.Stat(existing)
				if err != nil || info.Mode().Perm() != 0o755 {
					t.Fatalf("state permissions changed: %v, %v", info, err)
				}
			}

			invoke(t, file, tc.args, tc.code, tc.diag)
			if data, err := os.ReadFile(file); err != nil || string(data) != "keep" {
				t.Fatalf("state file changed: %q, %v", data, err)
			}
		})
	}
	t.Run("valid preflight opens state", func(t *testing.T) {
		invoke(t, file, []string{"--os", "windows", "--iso", iso, "--guest-binary", valid}, exitError, "store: prepare state directory for image build")
	})
}

func windowsImageBuildTestPE(machine uint16) []byte {
	// A PE32+ executable header with no sections suffices for the preflight.
	pe := make([]byte, 0x58+240)
	copy(pe, "MZ")
	pe[0x3c] = 0x40
	copy(pe[0x40:], "PE\x00\x00")
	binary.LittleEndian.PutUint16(pe[0x44:], machine)
	binary.LittleEndian.PutUint16(pe[0x54:], 240)
	pe[0x56] = 2
	binary.LittleEndian.PutUint16(pe[0x58:], 0x20b)
	pe[0x58+108] = 16
	return pe
}

func TestImageBuildRefusedBeforeOpeningStore(t *testing.T) {
	for _, held := range []bool{false, true} {
		name, want := "pending", store.ErrRemovalPending
		if held {
			name, want = "exclusive", store.ErrEnrollmentBusy
		}
		t.Run(name, func(t *testing.T) {
			clearAgentEnv(t)
			root := t.TempDir()
			helper := filepath.Join(root, "ownsand-guest.exe")
			if err := os.WriteFile(helper, windowsImageBuildTestPE(0x8664), 0o600); err != nil {
				t.Fatal(err)
			}
			st, err := store.OpenForRead(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if held {
				unlock, err := st.LockEnrollment(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer unlock()
			} else if _, err := st.UpdateAgent(func(s *store.AgentState) error {
				s.Removal.NoticeReceived = true
				return nil
			}); err != nil {
				t.Fatal(err)
			}

			var stdout, stderr bytes.Buffer
			code := run([]string{"image", "build", "--os", "windows", "--iso", "placeholder.iso", "--guest-binary", helper, "--state-dir", st.Dir(), "--no-color"}, nil, &stdout, &stderr)
			if code != exitError || !strings.Contains(stderr.String(), want.Error()) {
				t.Fatalf("image build = %d, %q, want %d and %v", code, stderr.String(), exitError, want)
			}
			for _, sub := range []string{store.ImagesDir, store.SandboxesDir, store.SnapshotsDir, store.VMsDir, store.UpdateDir} {
				if _, err := os.Stat(st.Path(sub)); !os.IsNotExist(err) {
					t.Errorf("refused build changed %s: %v", sub, err)
				}
			}
		})
	}
}
