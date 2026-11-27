// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
)

// The service log stays open for the whole run; a device removal must still
// delete the state directory content, including the log and the identity.
func TestResetDeletesStateWhileServiceLogIsOpen(t *testing.T) {
	requireElevatedServiceLogTest(t)
	dir := filepath.Join(t.TempDir(), "state")
	f, failure := setupServiceLog(dir)
	if f == nil || failure != "" {
		t.Fatalf("setupServiceLog = %v, %q", f, failure)
	}
	defer f.Close()
	for _, name := range []string{store.IdentityFile, store.AgentFile} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
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

// The sharing modes allow reading and deleting an open log without elevation.
func TestOpenServiceLogAllowsConcurrentReadAndDelete(t *testing.T) {
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
	if err := os.Remove(path); err != nil {
		t.Fatalf("delete while open: %v", err)
	}
	if _, err := f.WriteString("after delete"); err != nil {
		t.Fatalf("write after delete: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat after delete and close = %v, want os.ErrNotExist", err)
	}
}

func requireElevatedServiceLogTest(t *testing.T) {
	t.Helper()
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("the service state DACL requires an elevated administrator")
	}
}

func assertServiceStateACL(t *testing.T, path string, protected bool) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	control, _, err := sd.Control()
	if err != nil || (control&windows.SE_DACL_PROTECTED != 0) != protected {
		t.Fatalf("DACL control for %q = %#x, %v, protected want %v", path, control, err, protected)
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil || dacl.AceCount != 2 {
		t.Fatalf("DACL for %q must have two entries: %v", path, err)
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		t.Fatal(err)
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		t.Fatal(err)
	}
	flags := uint8(windows.INHERITED_ACE)
	if protected {
		flags = windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE
	}
	sawSystem, sawAdmins := false, false
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			t.Fatal(err)
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags != flags || ace.Mask != 0x001F01FF {
			t.Fatalf("unexpected access entry at %q: %+v", path, ace)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		sawSystem = sawSystem || sid.Equals(system)
		sawAdmins = sawAdmins || sid.Equals(admins)
	}
	if !sawSystem || !sawAdmins {
		t.Fatalf("DACL for %q must grant access to SYSTEM and Administrators: %s", path, sd)
	}
}

func TestSetupServiceLogProtectsNewStateDir(t *testing.T) {
	requireElevatedServiceLogTest(t)
	dir := filepath.Join(t.TempDir(), "missing", "state")
	f, failure := setupServiceLog(dir)
	if f == nil || failure != "" {
		t.Fatalf("setupServiceLog = %v, %q", f, failure)
	}
	defer f.Close()
	assertServiceStateACL(t, dir, true)
	assertServiceStateACL(t, filepath.Join(dir, "agent.log"), false)
}

func TestSetupServiceLogRejectsRootJunction(t *testing.T) {
	root, target := filepath.Join(t.TempDir(), "state"), t.TempDir()
	output, err := exec.Command("cmd.exe", "/d", "/c", "mklink", "/J", root, target).CombinedOutput()
	if err != nil {
		t.Fatalf("create fixture junction: %v: %s", err, output)
	}
	t.Cleanup(func() {
		if err := os.Remove(root); err != nil {
			t.Errorf("remove fixture junction: %v", err)
		}
	})
	before, err := windows.GetNamedSecurityInfo(target, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	if f, failure := setupServiceLog(root); f != nil || !strings.Contains(failure, "reparse point") {
		if f != nil {
			f.Close()
		}
		t.Fatalf("setupServiceLog on a junction = %v, %q", f, failure)
	}
	if _, err := os.Lstat(filepath.Join(target, "agent.log")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("junction target agent.log check = %v, want absent", err)
	}
	after, err := windows.GetNamedSecurityInfo(target, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil || after.String() != before.String() {
		t.Fatalf("junction target DACL changed: %v", err)
	}
}

func TestOpenServiceLogRejectsLeafJunction(t *testing.T) {
	path, target := filepath.Join(t.TempDir(), "agent.log"), t.TempDir()
	output, err := exec.Command("cmd.exe", "/d", "/c", "mklink", "/J", path, target).CombinedOutput()
	if err != nil {
		t.Fatalf("create fixture junction: %v: %s", err, output)
	}
	t.Cleanup(func() {
		if err := os.Remove(path); err != nil {
			t.Errorf("remove fixture junction: %v", err)
		}
	})
	if f, err := openServiceLog(path); f != nil || err == nil {
		if f != nil {
			f.Close()
		}
		t.Fatalf("openServiceLog on a junction = %v, %v", f, err)
	}
	if entries, err := os.ReadDir(target); err != nil || len(entries) != 0 {
		t.Fatalf("junction target entries = %v, %v", entries, err)
	}
}

func TestOpenServiceLogRejectsFileSymlink(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.log")
	target := filepath.Join(t.TempDir(), "fixture")
	if err := os.WriteFile(target, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		if errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD) || errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			t.Skipf("file symlink creation is unavailable: %v", err)
		}
		t.Fatal(err)
	}
	f, err := openServiceLog(path)
	if f != nil {
		f.Close()
	}
	var rejection *store.StatePathError
	if f != nil || !errors.As(err, &rejection) || rejection.Reason != store.StatePathReparsePoint {
		t.Fatalf("openServiceLog on a symlink = %v, %v", f, err)
	}
	if b, err := os.ReadFile(target); err != nil || string(b) != "fixture" {
		t.Fatalf("symlink target = %q, %v", b, err)
	}
}

func TestOpenServiceLogRejectsHardLink(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.log")
	target := filepath.Join(t.TempDir(), "fixture")
	if err := os.WriteFile(target, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(target, path); err != nil {
		t.Fatal(err)
	}
	f, err := openServiceLog(path)
	if f != nil {
		f.Close()
	}
	var rejection *store.StatePathError
	if f != nil || !errors.As(err, &rejection) || rejection.Reason != store.StatePathMultipleLinks {
		t.Fatalf("openServiceLog on a hard link = %v, %v", f, err)
	}
	if b, err := os.ReadFile(target); err != nil || string(b) != "fixture" {
		t.Fatalf("hard link target = %q, %v", b, err)
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
			if got := store.Win32StatePath(tc.input); got != tc.want {
				t.Errorf("store.Win32StatePath(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestOpenServiceLogLongStateDirectory(t *testing.T) {
	requireElevatedServiceLogTest(t)
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
