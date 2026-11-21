// SPDX-License-Identifier: Apache-2.0

package store

import (
	"encoding/json"
	"errors"
	"os"
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
