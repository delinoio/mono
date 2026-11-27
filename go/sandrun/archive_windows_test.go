// SPDX-License-Identifier: Apache-2.0

package sandrun

import (
	"archive/tar"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestExtractArchiveDirectorySymlinkRepairFailure(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.Symlink("target", "link"); err != nil {
		if os.Getenv("SANDRUN_TEST_SYMLINKS") == "required" {
			t.Fatalf("cannot create a forward link: %v", err)
		}
		t.Skipf("cannot create a forward link: %v", err)
	}
	if err := root.Mkdir("target", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile(filepath.Join("target", "file.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	st := &extractStats{files: 2, bytes: 1}
	deny := func(_ *os.Root, target, name string) error {
		if target != "target" || name != "link" {
			t.Fatalf("repair = %s -> %s, want link -> target", name, target)
		}
		return os.ErrPermission
	}
	if err := repairArchiveDirectoryLinks(t.Context(), root, st, deny); err != nil {
		t.Fatal(err)
	}
	if st.files != 1 || st.bytes != 1 || !slices.Equal(st.warnings, []string{"link: the symbolic link could not be created"}) {
		t.Fatalf("extraction = %+v, want omitted link and a warning", st)
	}
	if _, err := root.Lstat("link"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("omitted link = %v, want missing", err)
	}
	checkArchiveContents(t, dir, "target/file.txt", "x")
}

func TestDownloadArchiveSymlinkDenied(t *testing.T) {
	if os.Getenv("SANDRUN_TEST_SYMLINKS") != "denied" {
		t.Skip("requires a Windows test account or volume that denies os.Root.Symlink")
	}
	parent := t.TempDir()
	if err := probeArchiveSymlink(t, parent); err == nil {
		t.Fatal("os.Root.Symlink succeeded on the destination volume")
	} else {
		t.Logf("os.Root.Symlink probe denied: %v", err)
	}
	dest, res := downloadSymlinkArchive(t, parent,
		&tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "dir"},
		&tar.Header{Name: "dir/file.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1},
	)
	if res.Files != 1 || res.Bytes != 1 || !slices.Equal(res.Warnings, []string{"link: the symbolic link could not be created"}) {
		t.Fatalf("download = %+v, want omitted link and a warning", res)
	}
	if _, err := os.Lstat(filepath.Join(dest, "link")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("omitted link = %v, want missing", err)
	}
	checkArchiveContents(t, dest, "dir/file.txt", "x")
}
