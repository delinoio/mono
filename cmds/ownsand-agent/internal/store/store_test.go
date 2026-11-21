// SPDX-License-Identifier: FSL-1.1-ALv2

package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestIdentityAndAgentState(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadIdentity(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing identity: %v", err)
	}
	id := &Identity{DeviceID: "d", PrivateKeySeed: make([]byte, 32)}
	if err := s.SaveIdentity(id); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		st, _ := os.Stat(s.Path(IdentityFile))
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("identity mode %v", st.Mode())
		}
	}
	got, err := s.LoadIdentity()
	if err != nil || got.DeviceID != "d" {
		t.Fatalf("identity %v %v", got, err)
	}
	st, err := s.UpdateAgent(func(a *AgentState) error { a.RevisionCounter = 5; a.APIURL = "http://x"; return nil })
	if err != nil || st.RevisionCounter != 5 {
		t.Fatal(err)
	}
	if _, err := s.UpdateAgent(func(a *AgentState) error { a.RevisionCounter++; return nil }); err != nil {
		t.Fatal(err)
	}
	a, _ := s.LoadAgent()
	if a.RevisionCounter != 6 || a.APIURL != "http://x" {
		t.Fatalf("agent state %+v", a)
	}
	// Wiping local data keeps the identity.
	if err := s.WipeLocalData(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadIdentity(); err != nil {
		t.Fatal("identity lost by WipeLocalData")
	}
	if err := s.Reset(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadIdentity(); !errors.Is(err, ErrNotFound) {
		t.Fatal("identity kept by Reset")
	}
}

func TestSandboxAndSnapshotRecords(t *testing.T) {
	s, _ := Open(t.TempDir())
	type rec struct{ ID string }
	for _, id := range []string{"a", "b"} {
		if err := s.SaveSandbox(id, rec{id}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SaveSandbox("../x", rec{}); err == nil {
		t.Fatal("path traversal accepted")
	}
	var ids []string
	s.LoadSandboxes(func(b []byte) error {
		var r rec
		json.Unmarshal(b, &r)
		ids = append(ids, r.ID)
		return nil
	})
	if len(ids) != 2 {
		t.Fatalf("sandboxes %v", ids)
	}
	s.DeleteSandbox("a")
	if err := s.SaveSnapshot("s1", rec{"s1"}); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(s.Path(SnapshotsDir, "partial"), 0o700)
	n := 0
	s.LoadSnapshots(func([]byte) error { n++; return nil })
	if n != 1 {
		t.Fatalf("snapshots %d", n)
	}
	if _, err := os.Stat(s.Path(SnapshotsDir, "partial")); !os.IsNotExist(err) {
		t.Fatal("partial snapshot kept")
	}
}

func TestLoadSnapshotsRemovesAbandonedTransferArchives(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := s.Path(SnapshotsDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	abandoned := []string{".export-snap1-456.tar", ".import-snap1-123.tar"}
	kept := []string{"notes.txt", ".export-snap1-abc.tar", ".import-.tar", "export-snap1-1.tar"}
	for _, n := range append(append([]string{}, abandoned...), kept...) {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SaveSnapshot("ready", map[string]string{"id": "ready"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		n := 0
		if err := s.LoadSnapshots(func([]byte) error { n++; return nil }); err != nil {
			t.Fatalf("load %d: %v", i, err)
		}
		if n != 1 {
			t.Fatalf("load %d: snapshots %d", i, n)
		}
		for _, name := range abandoned {
			if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
				t.Fatalf("load %d: %s kept", i, name)
			}
		}
		for _, name := range kept {
			if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
				t.Fatalf("load %d: %s removed: %v", i, name, err)
			}
		}
	}
}

// Reset keeps deleting the other entries when one entry cannot be deleted,
// so that a stuck log file never keeps identity.json on disk, and it reports
// the failure.
func TestResetDeletesIdentityWhenAnotherEntryFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permissions do not block deletion on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root deletes entries of a read-only directory")
	}
	dir := t.TempDir()
	// "a-stuck" sorts first, so the failure comes before every other entry,
	// as the first failing entry of a lexical removal order.
	stuck := filepath.Join(dir, "a-stuck")
	if err := os.Mkdir(stuck, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stuck, "x"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stuck, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(stuck, 0o700) })
	for _, name := range []string{AgentFile, IdentityFile, "agent.log"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Reset(); err == nil {
		t.Fatal("Reset did not report the entry that it could not delete")
	}
	for _, name := range []string{AgentFile, IdentityFile, "agent.log"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s still exists after Reset: %v", name, err)
		}
	}
}
