// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
)

// The service log stays open for the whole run; a device removal must still
// delete the state directory content, including the log and the identity.
func TestResetDeletesStateWhileServiceLogIsOpen(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{store.IdentityFile, store.AgentFile} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f, err := openServiceLog(filepath.Join(dir, "agent.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString("{\"msg\":\"first\"}\n"); err != nil {
		t.Fatal(err)
	}
	if err := store.ResetDir(dir); err != nil {
		t.Fatalf("Reset with the log open: %v", err)
	}
	// The open handle can still write after the delete.
	if _, err := f.WriteString("{\"msg\":\"after\"}\n"); err != nil {
		t.Fatalf("write after the delete: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{store.IdentityFile, store.AgentFile, "agent.log"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s still exists after Reset: %v", name, err)
		}
	}
}

// Without removal, the log keeps appending across opens and keeps its
// earlier content.
func TestOpenServiceLogAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.log")
	for _, line := range []string{"one\n", "two\n"} {
		f, err := openServiceLog(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString(line); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.ReplaceAll(string(b), "\r\n", "\n"); got != "one\ntwo\n" {
		t.Fatalf("log content = %q", got)
	}
}

// A second handle can read the log while the service holds it open.
func TestOpenServiceLogAllowsConcurrentRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.log")
	f, err := openServiceLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString("x"); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "x" {
		t.Fatalf("read while open = %q, %v", b, err)
	}
}

func TestOpenServiceLogWin32Path(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	longTail := strings.Repeat("a", 90) + `\` + strings.Repeat("b", 90) + `\` + strings.Repeat("c", 90) + `\agent.log`
	shortUnicode := `C:\state\` + strings.Repeat("界", 90) + `\agent.log`
	for _, tc := range []struct {
		name, input, want string
	}{
		{"short drive", `C:\state\agent.log`, `C:\state\agent.log`},
		{"short UNC", `\\server\share\agent.log`, `\\server\share\agent.log`},
		{"short in UTF-16", shortUnicode, shortUnicode},
		{"long drive", `C:/state/unused/../` + longTail, `\\?\C:\state\` + longTail},
		{"long UNC", `//server/share/unused/../` + longTail, `\\?\UNC\server\share\` + longTail},
		{"long relative", `unused\..\` + longTail, `\\?\` + filepath.Join(dir, longTail)},
		{"extended drive", `\\?\C:\state\` + longTail, `\\?\C:\state\` + longTail},
		{"extended UNC", `\\?\UNC\server\share\` + longTail, `\\?\UNC\server\share\` + longTail},
		{"NT namespace", `\??\C:\state\` + longTail, `\??\C:\state\` + longTail},
		{"device namespace", `\\.\C:\state\` + longTail, `\\.\C:\state\` + longTail},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := serviceLogWin32Path(tc.input); got != tc.want {
				t.Errorf("serviceLogWin32Path(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestOpenServiceLogLongStateDirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), strings.Repeat("a", 90))
	dir := filepath.Join(root, strings.Repeat("b", 90), strings.Repeat("c", 90))
	path := filepath.Join(dir, "agent.log")
	if !filepath.IsAbs(path) || len(path) <= windows.MAX_PATH {
		t.Fatalf("test log must have an absolute path longer than MAX_PATH: %q", path)
	}
	// Use an extended path for fixture operations so the test itself works
	// with long-path opt-in disabled.
	t.Cleanup(func() {
		if err := os.RemoveAll(`\\?\` + root); err != nil {
			t.Error(err)
		}
	})

	openAndWrite := func(stateDir, record string) *os.File {
		t.Helper()
		f, failure := setupServiceLog(stateDir)
		if f == nil || failure != "" {
			t.Fatalf("setupServiceLog: %s", failure)
		}
		t.Cleanup(func() { _ = f.Close() })
		if f.Name() != filepath.Join(stateDir, "agent.log") {
			t.Errorf("log handle name = %q", f.Name())
		}
		if _, err := f.WriteString(record); err != nil {
			t.Fatal(err)
		}
		return f
	}
	const first, second, third = "{\"msg\":\"first\"}\n", "{\"msg\":\"second\"}\n", "{\"msg\":\"third\"}\n"
	if err := openAndWrite(dir, first).Close(); err != nil {
		t.Fatal(err)
	}
	writer := openAndWrite(dir, second)
	if err := openAndWrite(`\\?\`+dir, third).Close(); err != nil {
		t.Fatal(err)
	}

	extendedPath := `\\?\` + path
	if data, err := os.ReadFile(extendedPath); err != nil || string(data) != first+second+third {
		t.Fatalf("read while open = %q, %v", data, err)
	}
	if err := os.Remove(extendedPath); err != nil {
		t.Fatalf("delete while open: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(extendedPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat after delete and close = %v, want os.ErrNotExist", err)
	}

	if err := os.Mkdir(extendedPath, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := openServiceLog(path)
	var pathErr *os.PathError
	if !errors.As(err, &pathErr) || pathErr.Path != path {
		t.Fatalf("open a directory at the log path = %v, want PathError with the original path", err)
	}
	if f, failure := setupServiceLog(dir); f != nil || !strings.HasPrefix(failure, "could not open agent.log (Windows error ") {
		t.Fatalf("obstructed setupServiceLog = %v, %q", f, failure)
	}
}
