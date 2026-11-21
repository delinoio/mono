// SPDX-License-Identifier: Apache-2.0

package fsutil

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
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

// cancelAfterWriter cancels the context after it accepted limit bytes and
// counts every byte that it accepts.
type cancelAfterWriter struct {
	cancel  context.CancelFunc
	limit   int64
	written int64
}

func (w *cancelAfterWriter) Write(p []byte) (int, error) {
	w.written += int64(len(p))
	if w.written >= w.limit {
		w.cancel()
	}
	return len(p), nil
}

func TestWriteTarContextStopsInsideALargeFile(t *testing.T) {
	src := t.TempDir()
	const size = 64 << 20
	f, err := os.Create(filepath.Join(src, "big"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	f.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &cancelAfterWriter{cancel: cancel, limit: 1 << 20}
	if err := WriteTarContext(ctx, w, src); !errors.Is(err, context.Canceled) {
		t.Fatalf("WriteTarContext error = %v, want context.Canceled", err)
	}
	// The copy stops within a few chunks of the cancellation, far before
	// the end of the file.
	if w.written >= size/4 {
		t.Fatalf("wrote %d bytes after the cancellation at %d", w.written, w.limit)
	}
}

func TestWriteTarContextStopsBeforeTheFirstEntry(t *testing.T) {
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "f"), []byte("x"), 0o600)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var buf bytes.Buffer
	if err := WriteTarContext(ctx, &buf, src); !errors.Is(err, context.Canceled) {
		t.Fatalf("WriteTarContext error = %v, want context.Canceled", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("wrote %d bytes for a cancelled context", buf.Len())
	}
}

// cancelAfterReader cancels the context after it served limit bytes of r.
type cancelAfterReader struct {
	r      io.Reader
	cancel context.CancelFunc
	limit  int64
	read   int64
}

func (c *cancelAfterReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.read += int64(n)
	if c.read >= c.limit {
		c.cancel()
	}
	return n, err
}

func TestReadTarContextStopsInsideALargeFile(t *testing.T) {
	src := t.TempDir()
	const size = 64 << 20
	f, err := os.Create(filepath.Join(src, "big"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	f.Close()
	var archive bytes.Buffer
	if err := WriteTar(&archive, src); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &cancelAfterReader{r: &archive, cancel: cancel, limit: 1 << 20}
	dir := t.TempDir()
	if err := ReadTarContext(ctx, r, dir); !errors.Is(err, context.Canceled) {
		t.Fatalf("ReadTarContext error = %v, want context.Canceled", err)
	}
	if r.read >= size/4 {
		t.Fatalf("read %d bytes after the cancellation at %d", r.read, r.limit)
	}
	if info, err := os.Stat(filepath.Join(dir, "big")); err == nil && info.Size() >= size/4 {
		t.Fatalf("extracted %d bytes after the cancellation", info.Size())
	}
}

func TestReadTarContextStopsBeforeTheFirstEntry(t *testing.T) {
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "f"), []byte("x"), 0o600)
	var archive bytes.Buffer
	if err := WriteTar(&archive, src); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dir := t.TempDir()
	if err := ReadTarContext(ctx, &archive, dir); !errors.Is(err, context.Canceled) {
		t.Fatalf("ReadTarContext error = %v, want context.Canceled", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("extracted %d entries for a cancelled context", len(entries))
	}
}

func TestContextVariantsKeepTheTarFormat(t *testing.T) {
	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, "d"), 0o755)
	os.WriteFile(filepath.Join(src, "d", "f"), []byte("hello"), 0o640)
	os.Symlink("f", filepath.Join(src, "d", "l"))
	var plain, withCtx bytes.Buffer
	if err := WriteTar(&plain, src); err != nil {
		t.Fatal(err)
	}
	if err := WriteTarContext(context.Background(), &withCtx, src); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(plain.Bytes(), withCtx.Bytes()) {
		t.Fatal("WriteTarContext wrote a different stream than WriteTar")
	}
	out := t.TempDir()
	if err := ReadTarContext(context.Background(), &withCtx, out); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(out, "d", "l")); string(b) != "hello" {
		t.Fatalf("extracted %q", b)
	}
}
