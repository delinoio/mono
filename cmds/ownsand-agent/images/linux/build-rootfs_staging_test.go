// SPDX-License-Identifier: FSL-1.1-ALv2

package linux

import (
	"bytes"
	"os"
	"os/exec"
	"strconv"
	"testing"
)

const stubApplets = "STUB_APPLETS=sh\nls\nbusybox\ninit"

// runSized runs the script with an explicit image size argument.
func (b *rootfsBuild) runSized(size string) (string, error) {
	cmd := exec.Command("bash", b.script, b.out, size)
	cmd.Env = append(append(os.Environ(), b.env...), stubApplets)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// requireMkfs skips the test when no mkfs.ext4 is installed.
func requireMkfs(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("mkfs.ext4"); err != nil {
		if _, statErr := os.Stat("/usr/sbin/mkfs.ext4"); statErr != nil {
			t.Skip("mkfs.ext4 is not available")
		}
	}
}

// requireOutputDirEntries checks that the output directory holds exactly
// the wanted entries, so a staged file left beside the output fails the test.
func (b *rootfsBuild) requireOutputDirEntries(want ...string) {
	b.t.Helper()
	entries, err := os.ReadDir(b.dir)
	if err != nil {
		b.t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range entries {
		got[e.Name()] = true
	}
	// The harness directory always holds its stubs, guest and scratch.
	for _, name := range []string{"bin", "guest", "tmp"} {
		delete(got, name)
	}
	if len(got) != len(want) {
		b.t.Fatalf("output directory entries = %v, want %v", got, want)
	}
	for _, name := range want {
		if !got[name] {
			b.t.Fatalf("output directory entries = %v, want %v", got, want)
		}
	}
}

// Sizes that make truncate (not a number) or mkfs.ext4 (a zero-byte file)
// fail after the root tree is ready, which is where the script used to have
// already replaced the output.
var failingSizes = []string{"invalid", "0"}

func TestAllocationOrFormatFailureKeepsAnExistingImage(t *testing.T) {
	requireMkfs(t)
	for _, size := range failingSizes {
		t.Run(size, func(t *testing.T) {
			b := newRootfsBuild(t)
			if out, err := b.runSized("8"); err != nil {
				t.Fatalf("the first build failed: %v\n%s", err, out)
			}
			before, err := os.ReadFile(b.out)
			if err != nil {
				t.Fatal(err)
			}
			out, runErr := b.runSized(size)
			requireFailure(t, out, runErr)
			after, err := os.ReadFile(b.out)
			if err != nil {
				t.Fatalf("the earlier image is gone: %v", err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("the earlier image changed")
			}
			b.requireOutputDirEntries("rootfs.ext4")
			b.requireNoScratch()
		})
	}
}

func TestAllocationOrFormatFailureCreatesNoImage(t *testing.T) {
	requireMkfs(t)
	for _, size := range failingSizes {
		t.Run(size, func(t *testing.T) {
			b := newRootfsBuild(t)
			out, err := b.runSized(size)
			requireFailure(t, out, err)
			if _, statErr := os.Stat(b.out); !os.IsNotExist(statErr) {
				t.Fatalf("the failed build left an output file: %v", statErr)
			}
			b.requireOutputDirEntries()
			b.requireNoScratch()
		})
	}
}

func TestSuccessfulBuildReplacesAnEarlierImage(t *testing.T) {
	requireMkfs(t)
	b := newRootfsBuild(t)
	if err := os.WriteFile(b.out, []byte("an earlier image"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := b.runSized("12")
	if err != nil {
		t.Fatalf("the build failed: %v\n%s", err, out)
	}
	info, statErr := os.Stat(b.out)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if want := int64(12 << 20); info.Size() != want {
		t.Fatalf("image size = %d, want %d", info.Size(), want)
	}
	// The ext4 superblock magic 0xEF53 sits at byte offset 1080.
	data, err := os.ReadFile(b.out)
	if err != nil {
		t.Fatal(err)
	}
	if data[1080] != 0x53 || data[1081] != 0xEF {
		t.Fatalf("no ext4 superblock magic at offset 1080: % x", data[1080:1082])
	}
	if info.Mode().Perm()&0o600 != 0o600 {
		t.Fatalf("image mode = %v, want owner read and write", info.Mode().Perm())
	}
	if want := "wrote " + b.out + "\n"; !bytes.HasSuffix([]byte(out), []byte(want)) {
		t.Fatalf("output = %q, want suffix %q", out, want)
	}
	b.requireOutputDirEntries("rootfs.ext4")
	b.requireNoScratch()
}

func TestDirectoryAtTheOutputPathFailsTheBuild(t *testing.T) {
	b := newRootfsBuild(t)
	if err := os.Mkdir(b.out, 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := b.runSized(strconv.Itoa(8))
	requireFailure(t, out, err)
	entries, readErr := os.ReadDir(b.out)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("the build wrote %d entries into the directory", len(entries))
	}
}
