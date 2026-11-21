// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Reset(); err != nil {
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
