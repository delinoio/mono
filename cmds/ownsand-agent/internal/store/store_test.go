// SPDX-License-Identifier: FSL-1.1-ALv2

package store

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
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
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	type rec struct{ ID string }
	// loadIDs decodes every saved record and returns the sorted record IDs. It
	// fails the test on any load or decode error.
	loadIDs := func(load func(func([]byte) error) error) []string {
		t.Helper()
		ids := []string{}
		err := load(func(b []byte) error {
			var r rec
			if err := json.Unmarshal(b, &r); err != nil {
				return err
			}
			ids = append(ids, r.ID)
			return nil
		})
		if err != nil {
			t.Fatalf("load records: %v", err)
		}
		sort.Strings(ids)
		return ids
	}
	for _, id := range []string{"a", "b"} {
		if err := s.SaveSandbox(id, rec{id}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SaveSandbox("../x", rec{}); err == nil {
		t.Fatal("path traversal accepted")
	}
	if got := loadIDs(s.LoadSandboxes); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("sandboxes before delete %v", got)
	}
	if err := s.DeleteSandbox("a"); err != nil {
		t.Fatalf("delete sandbox: %v", err)
	}
	if _, err := os.Stat(s.Path(SandboxesDir, "a.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("deleted sandbox record kept: stat error is not ErrNotExist")
	}
	if got := loadIDs(s.LoadSandboxes); !reflect.DeepEqual(got, []string{"b"}) {
		t.Fatalf("sandboxes after delete %v", got)
	}
	if err := s.SaveSnapshot("s1", rec{"s1"}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(s.Path(SnapshotsDir, "partial"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := loadIDs(s.LoadSnapshots); !reflect.DeepEqual(got, []string{"s1"}) {
		t.Fatalf("snapshots %v", got)
	}
	if _, err := os.Stat(s.Path(SnapshotsDir, "partial")); !errors.Is(err, fs.ErrNotExist) {
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
	dir := t.TempDir()
	// "a-stuck" sorts first, so the failure comes before every other entry,
	// as the first failing entry of a lexical removal order. The removal of
	// a tree makes its read-only directories writable, so a read-only
	// directory no longer makes the removal fail; a seam does.
	stuck := filepath.Join(dir, "a-stuck")
	if err := os.Mkdir(stuck, 0o700); err != nil {
		t.Fatal(err)
	}
	orig := removeAll
	t.Cleanup(func() { removeAll = orig })
	removeAll = func(path string) error {
		if path == stuck {
			return errors.New("stuck entry")
		}
		return orig(path)
	}
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
