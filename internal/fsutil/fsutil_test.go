// SPDX-License-Identifier: Apache-2.0

package fsutil

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
		{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "//server/share"},
		{Name: "link", Typeflag: tar.TypeSymlink, Linkname: ""},
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
	err := ReadTar(buf, dir)
	if runtime.GOOS == "windows" {
		// Windows removes ".." elements lexically before it follows a link,
		// and os.Root does the same there, so "a/b/.." is "a" and the file
		// is "a/evil" inside the root. ReadTar may accept the archive, but
		// then the file must be inside the root.
		if err == nil {
			if b, err := os.ReadFile(filepath.Join(dir, "a", "evil")); err != nil || string(b) != "pwned" {
				t.Fatalf("accepted the archive, but a/evil is not the file in the root: %q, %v", b, err)
			}
		}
	} else if err == nil {
		// On other systems the link "a/b" is resolved first, so "a/b/.."
		// is the parent of the root.
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
	err := ReadTar(buf, dir)
	if runtime.GOOS == "windows" {
		// Windows removes ".." elements lexically before it follows a link,
		// and os.Root does the same there, so "a/b/.." is "a" and the
		// directory is "a/created" inside the root. ReadTar may accept the
		// archive, but then the directory must be inside the root.
		if err == nil {
			if info, err := os.Lstat(filepath.Join(dir, "a", "created")); err != nil || !info.IsDir() {
				t.Fatalf("accepted the archive, but a/created is not a directory in the root: %v", err)
			}
		}
	} else if err == nil {
		// On other systems the link "a/b" is resolved first, so "a/b/.."
		// is the parent of the root.
		t.Fatal("accepted a directory below an escaping symbolic link")
	}
	if _, err := os.Lstat(filepath.Join(parent, "created")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("directory created outside the restore root: %v", err)
	}
}

// restoreBelowSecret returns a restore directory below a parent that holds
// secret.txt, so a link that escapes the restore root has something to reach.
func restoreBelowSecret(t *testing.T) (parent, dir string) {
	t.Helper()
	parent = t.TempDir()
	if err := os.WriteFile(filepath.Join(parent, "secret.txt"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir = filepath.Join(parent, "restore")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return parent, dir
}

// requireNothingOutside fails when the parent directory holds anything but
// secret.txt and the restore directory.
func requireNothingOutside(t *testing.T, parent string) {
	t.Helper()
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "secret.txt" && e.Name() != "restore" {
			t.Errorf("entry created outside the restore root: %s", e.Name())
		}
	}
}

// requireLinkInside fails when the symbolic link name below dir resolves to
// the parent of dir or to secret.txt in it. It asks the operating system
// through os.Stat and compares file identities, because filepath.EvalSymlinks
// follows ".." after a link as the parent of the link destination, which
// Windows does not do for the links of these tests. A link that resolves to
// nothing passes.
func requireLinkInside(t *testing.T, dir, name string) {
	t.Helper()
	info, err := os.Stat(filepath.Join(dir, name))
	if err != nil {
		// The operating system could not resolve the link, so it does not
		// reach anything outside the root.
		return
	}
	parent := filepath.Dir(dir)
	for _, outside := range []string{parent, filepath.Join(parent, "secret.txt")} {
		outsideInfo, err := os.Stat(outside)
		if err != nil {
			t.Fatal(err)
		}
		if os.SameFile(info, outsideInfo) {
			t.Fatalf("link %s resolves outside the root: it is %s", name, outside)
		}
	}
}

func TestRecoverStatReturnsPanicAsError(t *testing.T) {
	err := recoverStat(func() (fs.FileInfo, error) {
		var parts []string
		_ = parts[1] // index out of range, like os.doInRoot on Windows
		return nil, nil
	})
	if !errors.Is(err, errRootStatPanic) {
		t.Fatalf("panic not returned as errRootStatPanic: %v", err)
	}
	if !strings.Contains(err.Error(), "index out of range") {
		t.Fatalf("error lost the panic value: %v", err)
	}
}

func TestRecoverStatKeepsStatResult(t *testing.T) {
	want := fs.ErrNotExist
	if err := recoverStat(func() (fs.FileInfo, error) { return nil, want }); !errors.Is(err, want) {
		t.Fatalf("error changed: %v", err)
	}
	if err := recoverStat(func() (fs.FileInfo, error) { return nil, nil }); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestReadTarRejectsChainedLinkWithNothingWrittenThroughIt(t *testing.T) {
	requireSymlinks(t)
	parent, dir := restoreBelowSecret(t)
	// "b" passes the lexical check, because "a/.." cleans to ".", but "a"
	// is a link to the root, so the file system resolves "a/.." to the
	// parent of the root.
	buf := buildTar(t, []tarEntry{
		{hdr: tar.Header{Name: "a", Typeflag: tar.TypeSymlink, Linkname: "."}},
		{hdr: tar.Header{Name: "b", Typeflag: tar.TypeSymlink, Linkname: "a/.."}},
	})
	err := ReadTar(buf, dir)
	if runtime.GOOS == "windows" {
		// Windows removes ".." elements lexically before it follows a
		// link, so "a/.." is the root. ReadTar may accept the archive, but
		// then "b" must resolve inside the root.
		if err == nil {
			requireLinkInside(t, dir, "b")
		}
	} else if err == nil {
		t.Fatal("accepted a symbolic link that resolves outside the root through an earlier link")
	}
	requireNothingOutside(t, parent)
}

func TestReadTarRejectsChainedLinkVariants(t *testing.T) {
	deep := strings.Repeat("a/", 25) + strings.Repeat("../", 25)
	tests := []struct {
		name    string
		entries []tarEntry
		links   []string
	}{
		{
			name: "link to a directory with a dot-dot suffix",
			entries: []tarEntry{
				{hdr: tar.Header{Name: "sub", Typeflag: tar.TypeDir, Mode: 0o755}},
				{hdr: tar.Header{Name: "a", Typeflag: tar.TypeSymlink, Linkname: "sub/.."}},
				{hdr: tar.Header{Name: "b", Typeflag: tar.TypeSymlink, Linkname: "a/.."}},
			},
			links: []string{"a", "b"},
		},
		{
			name: "link below a directory that climbs",
			entries: []tarEntry{
				{hdr: tar.Header{Name: "d", Typeflag: tar.TypeDir, Mode: 0o755}},
				{hdr: tar.Header{Name: "d/up", Typeflag: tar.TypeSymlink, Linkname: ".."}},
				{hdr: tar.Header{Name: "d/x", Typeflag: tar.TypeSymlink, Linkname: "up/.."}},
			},
			links: []string{"d/up", "d/x"},
		},
		{
			name: "dot-dot after a repeated link",
			entries: []tarEntry{
				{hdr: tar.Header{Name: "a", Typeflag: tar.TypeSymlink, Linkname: "."}},
				{hdr: tar.Header{Name: "b", Typeflag: tar.TypeSymlink, Linkname: "a/a/../.."}},
			},
			links: []string{"a", "b"},
		},
		{
			name: "deep chain that reaches the parent",
			entries: []tarEntry{
				{hdr: tar.Header{Name: "a", Typeflag: tar.TypeSymlink, Linkname: "."}},
				{hdr: tar.Header{Name: "disk.ext4", Typeflag: tar.TypeSymlink, Linkname: deep + "secret.txt"}},
			},
			links: []string{"a", "disk.ext4"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requireSymlinks(t)
			parent, dir := restoreBelowSecret(t)
			err := ReadTar(buildTar(t, tt.entries), dir)
			if runtime.GOOS == "windows" {
				// Windows cleans ".." lexically, so these links stay in
				// the root. ReadTar may accept the archive, but then no
				// link may resolve outside the root.
				if err == nil {
					for _, name := range tt.links {
						requireLinkInside(t, dir, filepath.FromSlash(name))
					}
				}
			} else if err == nil {
				t.Fatal("accepted a symbolic link that resolves outside the root")
			}
			requireNothingOutside(t, parent)
		})
	}
}

func TestReadTarRejectsLinkThatLaterBecomesChained(t *testing.T) {
	requireSymlinks(t)
	parent, dir := restoreBelowSecret(t)
	// "missing" does not exist when "b" is created, so a check at creation
	// time accepts "b". The next entry turns "missing" into a link to the
	// root, and then "b" resolves to the parent of the root.
	buf := buildTar(t, []tarEntry{
		{hdr: tar.Header{Name: "b", Typeflag: tar.TypeSymlink, Linkname: "missing/.."}},
		{hdr: tar.Header{Name: "missing", Typeflag: tar.TypeSymlink, Linkname: "."}},
	})
	err := ReadTar(buf, dir)
	if runtime.GOOS == "windows" {
		// Windows cleans ".." lexically, so "missing/.." is the root.
		if err == nil {
			requireLinkInside(t, dir, "b")
		}
	} else if err == nil {
		t.Fatal("accepted a link that resolves outside the root after a later entry")
	}
	requireNothingOutside(t, parent)
}

func TestReadTarAcceptsChainedLinkInsideTheRoot(t *testing.T) {
	requireSymlinks(t)
	parent, dir := restoreBelowSecret(t)
	// "a/.." is the root here, because "a" is a link to the directory "sub"
	// below the root.
	buf := buildTar(t, []tarEntry{
		{hdr: tar.Header{Name: "sub", Typeflag: tar.TypeDir, Mode: 0o755}},
		{hdr: tar.Header{Name: "a", Typeflag: tar.TypeSymlink, Linkname: "sub"}},
		{hdr: tar.Header{Name: "b", Typeflag: tar.TypeSymlink, Linkname: "a/.."}},
	})
	if err := ReadTar(buf, dir); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"a": "sub", "b": "a/.."} {
		got, err := os.Readlink(filepath.Join(dir, name))
		if err != nil || filepath.ToSlash(got) != want {
			t.Errorf("Readlink(%s) = %q, %v; want %q", name, got, err, want)
		}
	}
	requireLinkInside(t, dir, "b")
	requireNothingOutside(t, parent)
}

func TestReadTarAcceptsLinkThroughARegularFile(t *testing.T) {
	requireSymlinks(t)
	dir := t.TempDir()
	// A link below a regular file resolves to nothing and stays allowed
	// like a dangling link.
	buf := buildTar(t, []tarEntry{
		{hdr: tar.Header{Name: "f", Typeflag: tar.TypeReg, Mode: 0o644}, data: "x"},
		{hdr: tar.Header{Name: "l", Typeflag: tar.TypeSymlink, Linkname: "f/x"}},
	})
	if err := ReadTar(buf, dir); err != nil {
		t.Fatal(err)
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
	if b, err := os.ReadFile(filepath.Join(dir, "dirlink", "f")); err != nil || string(b) != "hello" {
		t.Fatalf("dirlink: %q, %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "real", "up")); err != nil || string(b) != "hello" {
		t.Fatalf("up: %q, %v", b, err)
	}
	if got, err := os.Readlink(filepath.Join(dir, "dangling")); err != nil || got != "missing" {
		t.Fatalf("dangling: %q, %v", got, err)
	}
}

func TestReadTarRejectsAbsoluteLookingLink(t *testing.T) {
	for _, link := range []string{"/etc", "/etc/passwd", "a/../../x", "//server/share/x"} {
		buf := buildTar(t, []tarEntry{
			{hdr: tar.Header{Name: "l", Typeflag: tar.TypeSymlink, Linkname: link}},
		})
		if err := ReadTar(buf, t.TempDir()); err == nil {
			t.Errorf("accepted link %q", link)
		}
	}
}

// windowsPathForms are tar names and link targets that are not local paths
// on Windows. On other systems a backslash and a colon are ordinary file
// name characters, so the same strings are local names there.
var windowsPathForms = []string{
	`\x`,
	`\etc\passwd`,
	`C:\x`,
	`C:x`,
	`C:/x`,
	`\\server\share`,
	`\\server\share\x`,
	`\\?\C:\x`,
	`a\..\..\x`,
}

func TestReadTarWindowsPathFormLinks(t *testing.T) {
	for _, link := range windowsPathForms {
		t.Run(link, func(t *testing.T) {
			buf := buildTar(t, []tarEntry{
				{hdr: tar.Header{Name: "l", Typeflag: tar.TypeSymlink, Linkname: link}},
			})
			dir := t.TempDir()
			err := ReadTar(buf, dir)
			if runtime.GOOS == "windows" {
				// The check runs before the link is created, so this part
				// does not need the symbolic link privilege.
				if err == nil {
					t.Fatalf("accepted link %q", link)
				}
				return
			}
			requireSymlinks(t)
			if err != nil {
				t.Fatalf("rejected link %q, which is a local name on %s: %v", link, runtime.GOOS, err)
			}
			if got, err := os.Readlink(filepath.Join(dir, "l")); err != nil || got != link {
				t.Fatalf("link target %q, %v; want %q", got, err, link)
			}
		})
	}
}

func TestReadTarWindowsPathFormNames(t *testing.T) {
	for _, name := range windowsPathForms {
		t.Run(name, func(t *testing.T) {
			buf := buildTar(t, []tarEntry{
				{hdr: tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644}, data: "x"},
			})
			parent := t.TempDir()
			dir := filepath.Join(parent, "restore")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			err := ReadTar(buf, dir)
			if runtime.GOOS == "windows" {
				if err == nil {
					t.Fatalf("accepted entry name %q", name)
				}
				return
			}
			if err != nil {
				t.Fatalf("rejected entry name %q, which is a local name on %s: %v", name, runtime.GOOS, err)
			}
			if b, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(b) != "x" {
				t.Fatalf("entry %q: %q, %v", name, b, err)
			}
		})
	}
}

func TestConfinedLinkTarget(t *testing.T) {
	windows := runtime.GOOS == "windows"
	for _, tc := range []struct {
		name, link string
		want       bool
	}{
		{"l", "f", true},
		{"l", "a/b", true},
		{"a/l", "../f", true},
		{"a/b/l", "../../f", true},
		{"l", ".", true},
		{"l", "", false},
		{"l", "f\x00", false},
		{"l", "/etc", false},
		{"l", "/etc/passwd", false},
		{"l", "//server/share", false},
		{"l", "..", false},
		{"l", "../x", false},
		{"a/l", "../../x", false},
		{"l", "a/../../x", false},
		{"l", `\x`, !windows},
		{"l", `\etc\passwd`, !windows},
		{"l", `C:\x`, !windows},
		{"l", `C:x`, !windows},
		{"l", `C:/x`, !windows},
		{"l", `\\server\share`, !windows},
		{"l", `\\?\C:\x`, !windows},
		{"a/l", `..\..\x`, !windows},
		{"l", "NUL", !windows},
	} {
		if got := confinedLinkTarget(filepath.FromSlash(tc.name), tc.link); got != tc.want {
			t.Errorf("confinedLinkTarget(%q, %q) = %v, want %v", tc.name, tc.link, got, tc.want)
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
