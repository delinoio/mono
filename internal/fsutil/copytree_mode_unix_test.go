// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package fsutil

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
)

// setUmask sets the process umask for the test and restores it on cleanup.
// The tests of this package do not run in parallel, so the process-wide
// change does not reach another test.
func setUmask(t *testing.T, mask int) {
	t.Helper()
	old := syscall.Umask(mask)
	t.Cleanup(func() { syscall.Umask(old) })
}

func requireMode(t *testing.T, path string, want fs.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("%s: mode %o, want %o", path, got, want)
	}
}

// buildModeTree creates a source tree with read-only and group-writable
// directories and files, whose modes the process umask would clear. The
// modes are set with Chmod, which the umask does not filter. The cleanup
// makes the tree writable again so that t.TempDir can remove it.
func buildModeTree(t *testing.T) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), "src")
	for _, d := range []string{"ro/deep", "rw"} {
		if err := os.MkdirAll(filepath.Join(src, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]fs.FileMode{
		"top":            0o660,
		"ro/f":           0o770,
		"ro/deep/g":      0o444,
		"ro/deep/shared": 0o664,
		"rw/private":     0o600,
	}
	for name, mode := range files {
		path := filepath.Join(src, name)
		if err := os.WriteFile(path, []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("../top", filepath.Join(src, "ro", "link")); err != nil {
		t.Fatal(err)
	}
	dirs := map[string]fs.FileMode{
		"rw":      0o775,
		"ro/deep": 0o555,
		"ro":      0o555,
		".":       0o775,
	}
	for name, mode := range dirs {
		if err := os.Chmod(filepath.Join(src, name), mode); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { makeDirsWritable(src) })
	return src
}

func requireModeTree(t *testing.T, dst string, rootMode fs.FileMode) {
	t.Helper()
	for name, mode := range map[string]fs.FileMode{
		".":              rootMode,
		"rw":             0o775,
		"ro":             0o555,
		"ro/deep":        0o555,
		"top":            0o660,
		"ro/f":           0o770,
		"ro/deep/g":      0o444,
		"ro/deep/shared": 0o664,
		"rw/private":     0o600,
	} {
		requireMode(t, filepath.Join(dst, name), mode)
	}
}

func TestCopyTreePreservesModesUnderUmask(t *testing.T) {
	for _, mask := range []int{0o022, 0o077, 0o277, 0o177} {
		t.Run(fmt.Sprintf("umask%04o", mask), func(t *testing.T) {
			src := buildModeTree(t)
			base := t.TempDir()
			if err := os.Chmod(base, 0o711); err != nil {
				t.Fatal(err)
			}
			for _, test := range []struct {
				name string
				dir  string
			}{
				{name: "existing-parent", dir: base},
				{name: "missing-parents", dir: filepath.Join(base, "parent", "nested")},
			} {
				t.Run(test.name, func(t *testing.T) {
					dst := filepath.Join(test.dir, "dst")
					t.Cleanup(func() { makeDirsWritable(dst) })
					setUmask(t, mask)
					if err := CopyTree(src, dst); err != nil {
						t.Fatalf("CopyTree: %v", err)
					}
					requireModeTree(t, dst, 0o775)
					requireMode(t, base, 0o711)
					if test.dir != base {
						parentMode := 0o775&^fs.FileMode(mask) | 0o700
						requireMode(t, filepath.Dir(test.dir), parentMode)
						requireMode(t, test.dir, parentMode)
					}
					for _, name := range []string{"top", "ro/deep/g"} {
						if b, err := os.ReadFile(filepath.Join(dst, name)); err != nil || string(b) != name {
							t.Fatalf("content of %s = %q, %v", name, b, err)
						}
					}
					link, err := os.Readlink(filepath.Join(dst, "ro", "link"))
					if err != nil || link != "../top" {
						t.Fatalf("symbolic link = %q, %v", link, err)
					}
				})
			}
		})
	}
}

func TestReadTarPreservesModesUnderUmask(t *testing.T) {
	const env = "FSUTIL_TEST_TAR_UMASK"
	if value := os.Getenv(env); value != "" {
		mask, err := strconv.ParseUint(value, 8, 32)
		if err != nil {
			t.Fatal(err)
		}
		setUmask(t, int(mask))
		src := buildModeTree(t)
		var archive bytes.Buffer
		if err := WriteTar(&archive, src); err != nil {
			t.Fatal(err)
		}
		dst := t.TempDir()
		if err := os.Chmod(dst, 0o700); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { makeDirsWritable(dst) })
		if err := ReadTar(&archive, dst); err != nil {
			t.Fatal(err)
		}
		requireModeTree(t, dst, 0o700)
		for name, want := range map[string]string{"ro/deep/shared": "ro/deep/shared", "ro/link": "top"} {
			if b, err := os.ReadFile(filepath.Join(dst, name)); err != nil || string(b) != want {
				t.Errorf("read %s: %q, %v; want %q", name, b, err, want)
			}
		}
		return
	}
	for _, mask := range []string{"0022", "0077"} {
		t.Run("umask"+mask, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestReadTarPreservesModesUnderUmask$")
			cmd.Env = append(os.Environ(), env+"="+mask)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("tar round trip: %v\n%s", err, out)
			}
		})
	}
}

func TestReadTarAppliesExplicitDirectoryModesAfterExtraction(t *testing.T) {
	dst := t.TempDir()
	t.Cleanup(func() { makeDirsWritable(dst) })
	archive := buildTar(t, []tarEntry{
		{hdr: tar.Header{Name: "parent", Typeflag: tar.TypeDir, Mode: 0}},
		{hdr: tar.Header{Name: "parent/deep", Typeflag: tar.TypeDir, Mode: 0o550}},
		{hdr: tar.Header{Name: "parent/implicit/file", Typeflag: tar.TypeReg, Mode: 0o600}, data: "contents"},
		{hdr: tar.Header{Name: "parent/", Typeflag: tar.TypeDir, Mode: 0o400}},
	})
	if err := ReadTar(archive, dst); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(dst, "parent")
	requireMode(t, parent, 0o400)
	// Reopen the parent to check what extraction wrote below it.
	if err := os.Chmod(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	requireMode(t, filepath.Join(parent, "deep"), 0o550)
	requireMode(t, filepath.Join(parent, "implicit"), 0o700)
	if got, err := os.ReadFile(filepath.Join(parent, "implicit", "file")); err != nil || string(got) != "contents" {
		t.Fatalf("extracted file = %q, %v", got, err)
	}
}

func TestCopyTreeCopiesReadOnlySourceRoot(t *testing.T) {
	setUmask(t, 0o022)
	src := filepath.Join(t.TempDir(), "src")
	if err := os.Mkdir(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "f"), []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(src, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(src, 0o755) })
	dst := filepath.Join(t.TempDir(), "dst")
	t.Cleanup(func() { os.Chmod(dst, 0o755) })
	if err := CopyTree(src, dst); err != nil {
		t.Fatalf("CopyTree: %v", err)
	}
	requireMode(t, dst, 0o500)
	requireMode(t, filepath.Join(dst, "f"), 0o640)
}

func TestCopyFilePreservesModeUnderUmask(t *testing.T) {
	setUmask(t, 0o077)
	src := filepath.Join(t.TempDir(), "src")
	if err := os.WriteFile(src, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "dst")
	if err := CopyFile(src, dst, 0o664); err != nil {
		t.Fatal(err)
	}
	requireMode(t, dst, 0o664)
}

// A failed copy still reports its error, and it leaves the directories that
// it created writable, so removing the partial copy does not fail.
func TestCopyTreeErrorIsReportedAndLeavesPartialCopyRemovable(t *testing.T) {
	setUmask(t, 0o022)
	src := buildModeTree(t)
	dst := filepath.Join(t.TempDir(), "dst")
	t.Cleanup(func() { makeDirsWritable(dst) })
	// A file at the place of a copied file makes the exclusive create fail
	// after the read-only directory ro was created.
	if err := os.MkdirAll(filepath.Join(dst, "ro"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "ro", "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := CopyTree(src, dst)
	if !errors.Is(err, fs.ErrExist) {
		t.Fatalf("CopyTree error = %v, want an already-exists error", err)
	}
	if err := os.RemoveAll(dst); err != nil {
		t.Fatalf("os.RemoveAll of the partial copy: %v", err)
	}
}

func TestCopyTreeContextCanceledLeavesPartialCopyRemovable(t *testing.T) {
	for _, mask := range []int{0o022, 0o277, 0o177} {
		t.Run(fmt.Sprintf("umask%04o", mask), func(t *testing.T) {
			src := buildModeTree(t)
			parent := filepath.Join(t.TempDir(), "parent")
			dst := filepath.Join(parent, "dst")
			t.Cleanup(func() { makeDirsWritable(parent) })
			setUmask(t, mask)
			// The context ends at the fourth entry, after ro and ro/deep were created.
			ctx := &errAfterContext{Context: context.Background(), left: 3}
			if err := CopyTreeContext(ctx, src, dst); !errors.Is(err, context.Canceled) {
				t.Fatalf("CopyTreeContext error = %v, want context.Canceled", err)
			}
			requireMode(t, filepath.Join(dst, "ro", "deep"), 0o755&^fs.FileMode(mask)|0o700)
			if err := os.RemoveAll(parent); err != nil {
				t.Fatalf("os.RemoveAll of the partial copy: %v", err)
			}
		})
	}
}
