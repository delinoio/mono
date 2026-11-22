// SPDX-License-Identifier: FSL-1.1-ALv2

package guest

import (
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// longDrivePath returns a drive path of at least 270 bytes below root, built
// from a repeated element.
func longDrivePath(root string) string {
	p := root
	for len(p) < 270 {
		p += `\` + strings.Repeat("d", 40)
	}
	return p
}

func TestExtendedPath(t *testing.T) {
	longDrive := longDrivePath(`C:\ownsand`)
	longUNC := `\\server\share` + strings.Repeat(`\`+strings.Repeat("u", 40), 6)
	// 247 bytes is the longest path that stays unchanged, 248 is the first
	// that converts (the limit that os.Open uses).
	at247 := `C:\` + strings.Repeat("a", 247-3)
	at248 := `C:\` + strings.Repeat("a", 248-3)
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"long drive path", longDrive, `\\?\` + longDrive},
		{"long drive path with slashes", strings.ReplaceAll(longDrive, `\`, `/`), `\\?\` + longDrive},
		{"long drive path with dot segments", longDrive + `\.\x\..\y`, `\\?\` + longDrive + `\y`},
		{"long UNC path", longUNC, `\\?\UNC\` + longUNC[2:]},
		{"long UNC path with slashes", strings.ReplaceAll(longUNC, `\`, `/`), `\\?\UNC\` + longUNC[2:]},
		{"already extended drive", `\\?\` + longDrive, `\\?\` + longDrive},
		{"already extended with slashes", `//?/` + longDrive, `//?/` + longDrive},
		{"already extended NT form", `\??\` + longDrive, `\??\` + longDrive},
		{"already extended UNC", `\\?\UNC\` + longUNC[2:], `\\?\UNC\` + longUNC[2:]},
		{"device path", `\\.\` + longDrive, `\\.\` + longDrive},
		{"short drive path", `C:\ownsand\file.txt`, `C:\ownsand\file.txt`},
		{"short drive path with slashes", `C:/ownsand/file.txt`, `C:/ownsand/file.txt`},
		{"short UNC path", `\\server\share\file.txt`, `\\server\share\file.txt`},
		{"short already extended", `\\?\C:\file.txt`, `\\?\C:\file.txt`},
		{"short device path", `\\.\NUL`, `\\.\NUL`},
		{"247 bytes", at247, at247},
		{"248 bytes", at248, `\\?\` + at248},
	}
	for _, c := range cases {
		if got := extendedPath(c.in); got != c.want {
			t.Errorf("%s: extendedPath(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

func TestExtendedPathRelative(t *testing.T) {
	// A short relative path in a short working directory keeps its form.
	t.Chdir(t.TempDir())
	if got := extendedPath(`sub\file`); got != `sub\file` {
		t.Fatalf("short relative path changed to %q", got)
	}
	long := strings.Repeat("r", 250)
	got := extendedPath(long)
	if !strings.HasPrefix(got, `\\?\`) || !strings.HasSuffix(got, `\`+long) {
		t.Fatalf("long relative path became %q", got)
	}
}

// The error of a failed stat names the path that the caller passed, not the
// prefixed form that CreateFile received.
func TestStatSourceErrorKeepsOriginalPath(t *testing.T) {
	for _, path := range []string{
		longDrivePath(filepath.Join(t.TempDir(), "missing")),
		filepath.Join(t.TempDir(), "missing"),
	} {
		_, err := statSource(path)
		var pe *fs.PathError
		if !errors.As(err, &pe) {
			t.Fatalf("statSource(%q) error %v is not a *fs.PathError", path, err)
		}
		if pe.Op != "stat" || pe.Path != path {
			t.Fatalf("PathError = {Op: %q, Path: %q}, want path %q", pe.Op, pe.Path, path)
		}
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("statSource(%q) error %v is not ErrNotExist", path, err)
		}
	}
}

// makeLongFile creates a file whose absolute path is longer than the Win32
// path limit and returns its ordinary path (without the extended prefix).
func makeLongFile(t *testing.T, content string) string {
	t.Helper()
	dir := longDrivePath(t.TempDir())
	if len(dir) < 270 {
		t.Fatalf("directory path has %d bytes", len(dir))
	}
	// The ordinary path of a directory this long is not accepted by every
	// Windows API, so create it through the extended-length form.
	if err := os.MkdirAll(`\\?\`+dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := dir + `\file.txt`
	if err := os.WriteFile(`\\?\`+path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// statSource and the os.Open that follows it must name the same file at a
// path that is longer than the Win32 path limit.
func TestStatSourceLongPath(t *testing.T) {
	path := makeLongFile(t, "hello")
	for name, p := range map[string]string{
		"ordinary":     path,
		"slashes":      strings.ReplaceAll(path, `\`, `/`),
		"extended":     `\\?\` + path,
		"dot segments": filepath.Dir(path) + `\.\sub\..\file.txt`,
	} {
		info, err := statSource(p)
		if err != nil {
			t.Fatalf("%s: statSource(%q): %v", name, p, err)
		}
		if !info.Mode().IsRegular() || info.Size() != 5 {
			t.Fatalf("%s: mode %v size %d", name, info.Mode(), info.Size())
		}
		in, err := os.Open(p)
		if err != nil {
			t.Fatalf("%s: os.Open(%q): %v", name, p, err)
		}
		inInfo, err := in.Stat()
		in.Close()
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(info, inInfo) {
			t.Fatalf("%s: statSource and os.Open name different files", name)
		}
	}
}

// The snapshot of a file at a long path takes the first stat and the open
// through the same file, so it neither fails nor reports a swap.
func TestMakeSnapshotLongPath(t *testing.T) {
	m, err := newReadManager(filepath.Join(t.TempDir(), "reads"), time.Now, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	path := makeLongFile(t, "hello")
	dst := filepath.Join(m.dir, "snap")
	if _, n, err := m.makeSnapshot(path, dst); err != nil || n != 5 {
		t.Fatalf("makeSnapshot of a long path: size %d, error %v", n, err)
	}
	if got, err := os.ReadFile(dst); err != nil || string(got) != "hello" {
		t.Fatalf("snapshot content %q, error %v", got, err)
	}
}
