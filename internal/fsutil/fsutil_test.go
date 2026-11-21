// SPDX-License-Identifier: Apache-2.0

package fsutil

import (
	"archive/tar"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCopyTreeAndTarRoundTrip(t *testing.T) {
	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, "a", "b"), 0o755)
	os.WriteFile(filepath.Join(src, "a", "b", "f"), []byte("hello"), 0o640)
	os.Symlink("b/f", filepath.Join(src, "a", "link"))
	dst := filepath.Join(t.TempDir(), "copy")
	if err := CopyTree(src, dst); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "a", "link")); string(b) != "hello" {
		t.Fatalf("copy: %q", b)
	}
	if n, _ := TreeSize(dst); n != 5 {
		t.Fatalf("size %d", n)
	}
	var buf bytes.Buffer
	if err := WriteTar(&buf, src); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if err := ReadTar(&buf, out); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(out, "a", "b", "f")); string(b) != "hello" {
		t.Fatalf("tar: %q", b)
	}
}

func TestReadTarRejectsEscapes(t *testing.T) {
	for _, hdr := range []*tar.Header{
		{Name: "../evil", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"},
		{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "../../x"},
	} {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		tw.WriteHeader(hdr)
		tw.Close()
		if err := ReadTar(&buf, t.TempDir()); err == nil {
			t.Errorf("accepted %+v", hdr)
		}
	}
}

// tarEntry is one entry of a hand-built test archive.
type tarEntry struct {
	hdr  tar.Header
	data string
}

func buildTar(t *testing.T, entries []tarEntry) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := e.hdr
		hdr.Size = int64(len(e.data))
		if err := tw.WriteHeader(&hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf
}

// requireSymlinks skips the test when the platform or the account cannot
// create symbolic links, for example Windows without the symbolic link
// privilege.
func requireSymlinks(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Symlink("target", filepath.Join(dir, "probe")); err != nil {
		t.Skipf("symbolic links are not available: %v", err)
	}
}

func TestReadTarRejectsChainedSymlinkEscape(t *testing.T) {
	requireSymlinks(t)
	parent := t.TempDir()
	dir := filepath.Join(parent, "restore")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Each link target passes the lexical check on its own, but together
	// the links resolve to the parent of the restore root.
	buf := buildTar(t, []tarEntry{
		{hdr: tar.Header{Name: "a", Typeflag: tar.TypeDir, Mode: 0o755}},
		{hdr: tar.Header{Name: "a/b", Typeflag: tar.TypeSymlink, Linkname: ".."}},
		{hdr: tar.Header{Name: "l2", Typeflag: tar.TypeSymlink, Linkname: "a/b/.."}},
		{hdr: tar.Header{Name: "l2/evil", Typeflag: tar.TypeReg, Mode: 0o644}, data: "pwned"},
	})
	if err := ReadTar(buf, dir); err == nil {
		t.Fatal("accepted a chained symbolic link escape")
	}
	if _, err := os.Lstat(filepath.Join(parent, "evil")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file written outside the restore root: %v", err)
	}
}

func TestReadTarRejectsDirectoryBelowEscapingLink(t *testing.T) {
	requireSymlinks(t)
	parent := t.TempDir()
	dir := filepath.Join(parent, "restore")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	buf := buildTar(t, []tarEntry{
		{hdr: tar.Header{Name: "a", Typeflag: tar.TypeDir, Mode: 0o755}},
		{hdr: tar.Header{Name: "a/b", Typeflag: tar.TypeSymlink, Linkname: ".."}},
		{hdr: tar.Header{Name: "l2", Typeflag: tar.TypeSymlink, Linkname: "a/b/.."}},
		{hdr: tar.Header{Name: "l2/created", Typeflag: tar.TypeDir, Mode: 0o755}},
	})
	if err := ReadTar(buf, dir); err == nil {
		t.Fatal("accepted a directory below an escaping symbolic link")
	}
	if _, err := os.Lstat(filepath.Join(parent, "created")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("directory created outside the restore root: %v", err)
	}
}

func TestReadTarAcceptsConfinedLinks(t *testing.T) {
	requireSymlinks(t)
	dir := t.TempDir()
	buf := buildTar(t, []tarEntry{
		{hdr: tar.Header{Name: "real", Typeflag: tar.TypeDir, Mode: 0o755}},
		{hdr: tar.Header{Name: "real/f", Typeflag: tar.TypeReg, Mode: 0o640}, data: "hello"},
		// A link to an in-root directory with no entry below it.
		{hdr: tar.Header{Name: "dirlink", Typeflag: tar.TypeSymlink, Linkname: "real"}},
		// A link that climbs and descends but stays inside the root.
		{hdr: tar.Header{Name: "real/up", Typeflag: tar.TypeSymlink, Linkname: "../real/f"}},
		// A dangling in-root link is allowed.
		{hdr: tar.Header{Name: "dangling", Typeflag: tar.TypeSymlink, Linkname: "missing"}},
	})
	if err := ReadTar(buf, dir); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "dirlink", "f")); string(b) != "hello" {
		t.Fatalf("dirlink: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "real", "up")); string(b) != "hello" {
		t.Fatalf("up: %q", b)
	}
	if got, err := os.Readlink(filepath.Join(dir, "dangling")); err != nil || got != "missing" {
		t.Fatalf("dangling: %q, %v", got, err)
	}
}

func TestReadTarRejectsAbsoluteLookingLink(t *testing.T) {
	for _, link := range []string{"/etc", "a/../../x"} {
		buf := buildTar(t, []tarEntry{
			{hdr: tar.Header{Name: "l", Typeflag: tar.TypeSymlink, Linkname: link}},
		})
		if err := ReadTar(buf, t.TempDir()); err == nil {
			t.Errorf("accepted link %q", link)
		}
	}
}
