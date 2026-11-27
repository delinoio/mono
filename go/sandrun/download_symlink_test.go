// SPDX-License-Identifier: Apache-2.0

package sandrun

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestDownloadArchiveSymlinks(t *testing.T) {
	parent := t.TempDir()
	if err := probeArchiveSymlink(t, parent); err != nil {
		if os.Getenv("SANDRUN_TEST_SYMLINKS") == "required" {
			t.Fatalf("cannot create a directory symlink on the destination volume: %v", err)
		}
		t.Skipf("cannot create a directory symlink on the destination volume: %v", err)
	}
	t.Log("os.Root.Symlink probe succeeded")

	link := &tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "dir"}
	dir := &tar.Header{Name: "dir/", Typeflag: tar.TypeDir, Mode: 0o755}
	file := &tar.Header{Name: "dir/file.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1}
	for _, tc := range []struct {
		name    string
		entries []*tar.Header
	}{
		{"link first", []*tar.Header{link, dir, file}},
		{"target first", []*tar.Header{dir, link, file}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dest, res := downloadSymlinkArchive(t, parent, tc.entries...)
			if res.Files != 2 || res.Bytes != 1 || len(res.Warnings) != 0 {
				t.Fatalf("download = %+v, want 2 files, 1 byte and no warnings", res)
			}
			checkArchiveSymlink(t, dest, "link", "dir")
			checkArchiveContents(t, dest, "dir/file.txt", "x")
			checkArchiveContents(t, dest, "link/file.txt", "x")
		})
	}

	t.Run("chain, file, dangling and replaced links", func(t *testing.T) {
		dest, res := downloadSymlinkArchive(t, parent,
			&tar.Header{Name: "a-link", Typeflag: tar.TypeSymlink, Linkname: "z-link/sub"},
			&tar.Header{Name: "z-link", Typeflag: tar.TypeSymlink, Linkname: "dir"},
			&tar.Header{Name: "file-link", Typeflag: tar.TypeSymlink, Linkname: "dir/sub/file.txt"},
			&tar.Header{Name: "dangling", Typeflag: tar.TypeSymlink, Linkname: "dir/missing"},
			&tar.Header{Name: "replaced", Typeflag: tar.TypeSymlink, Linkname: "dir"},
			&tar.Header{Name: "replaced", Typeflag: tar.TypeReg, Mode: 0o644, Size: 2},
			&tar.Header{Name: "dir/sub/file.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1},
		)
		if res.Files != 7 || res.Bytes != 3 || len(res.Warnings) != 0 {
			t.Fatalf("download = %+v, want 7 writes, 3 bytes and no warnings", res)
		}
		checkArchiveSymlink(t, dest, "a-link", "z-link/sub")
		checkArchiveSymlink(t, dest, "z-link", "dir")
		checkArchiveSymlink(t, dest, "file-link", "dir/sub/file.txt")
		checkArchiveSymlink(t, dest, "dangling", "dir/missing")
		checkArchiveContents(t, dest, "a-link/file.txt", "x")
		checkArchiveContents(t, dest, "z-link/sub/file.txt", "x")
		checkArchiveContents(t, dest, "file-link", "x")
		checkArchiveContents(t, dest, "replaced", "xx")
		if _, err := os.Stat(filepath.Join(dest, "dangling")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("dangling link target = %v, want missing", err)
		}
		if info, err := os.Lstat(filepath.Join(dest, "replaced")); err != nil || !info.Mode().IsRegular() {
			t.Errorf("replacement = %v, %v, want regular file", info, err)
		}
	})
}

func probeArchiveSymlink(t *testing.T, parent string) error {
	t.Helper()
	dir, err := os.MkdirTemp(parent, "link-probe-")
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.Mkdir("target", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile(filepath.Join("target", "file.txt"), []byte("probe"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := root.Symlink("target", "link"); err != nil {
		return err
	}
	checkArchiveContents(t, dir, "link/file.txt", "probe")
	return nil
}

func downloadSymlinkArchive(t *testing.T, parent string, entries ...*tar.Header) (string, *DownloadResult) {
	t.Helper()
	archive, err := io.ReadAll(tarOf(t, entries...))
	if err != nil {
		t.Fatal(err)
	}
	size := int64(len(archive))
	client := newScriptedDownloadClient(t, &scriptedDownloadSession{
		probe: fmt.Sprintf("dir %d", size), chunks: [][]byte{archive}, endSize: size,
	})
	dest, err := os.MkdirTemp(parent, "result-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, "previous"), []byte("previous"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := client.Download(t.Context(), DownloadParams{
		OrganizationID: "org", DevSessionID: "session", GuestPath: "out", LocalPath: dest,
	})
	if err != nil || res == nil || !res.Directory {
		t.Fatalf("directory download = %+v, %v", res, err)
	}
	if _, err := os.Lstat(filepath.Join(dest, "previous")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("previous destination still present: %v", err)
	}
	if matches, err := filepath.Glob(filepath.Join(parent, ".sandrun-download-*")); err != nil || len(matches) != 0 {
		t.Errorf("local staging remains: %v, %v", matches, err)
	}
	return dest, res
}

func checkArchiveSymlink(t *testing.T, dest, name, want string) {
	t.Helper()
	target, err := os.Readlink(filepath.Join(dest, name))
	if err != nil || filepath.ToSlash(target) != want {
		t.Errorf("readlink %s = %q, %v, want %q", name, target, err, want)
	}
}

func checkArchiveContents(t *testing.T, dest, name, want string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dest, name))
	if err != nil || string(data) != want {
		t.Errorf("read %s = %q, %v, want %q", name, data, err, want)
	}
}
