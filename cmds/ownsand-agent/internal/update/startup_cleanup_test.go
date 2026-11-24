// SPDX-License-Identifier: FSL-1.1-ALv2

package update

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func TestNewRemovesAbandonedUpdateFiles(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	abandoned := map[string]int64{
		"ownsand-agent-1.1.0-1":             10,
		"ownsand-agent-1.2.0-7":             20,
		".ownsand-agent-1.3.0-2.tmp-123456": 30,
	}
	for name, size := range abandoned {
		writeUpdateFile(t, st, name, int(size))
	}
	writeUpdateFile(t, st, "keep.txt", 1)
	writeUpdateFile(t, st, ".ownsand-agent-1.3.0-2.tmp-note", 1)
	if err := os.Mkdir(st.Path(store.UpdateDir, "ownsand-agent-1.9.0-1"), 0o700); err != nil {
		t.Fatal(err)
	}
	want := []string{".ownsand-agent-1.3.0-2.tmp-note", "keep.txt", "ownsand-agent-1.9.0-1"}
	linkTarget := st.Path("keep-target")
	if err := os.WriteFile(linkTarget, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	linkName := "ownsand-agent-9.9.9-1"
	linkPath := st.Path(store.UpdateDir, linkName)
	linkErr := os.Symlink(linkTarget, linkPath)
	if linkErr == nil {
		want = append(want, linkName)
	} else {
		t.Logf("symlink unavailable: %v", linkErr)
	}

	var logs bytes.Buffer
	if _, err := New(Options{Store: st, Logger: slog.New(slog.NewJSONHandler(&logs, nil))}); err != nil {
		t.Fatal(err)
	}
	requireUpdateEntries(t, st, want...)
	if linkErr == nil {
		if got, err := os.Readlink(linkPath); err != nil || got != linkTarget {
			t.Fatalf("symlink target = %q, %v, want %q", got, err, linkTarget)
		}
	}
	if got := readString(t, linkTarget); got != "keep" {
		t.Fatalf("symlink target contents = %q", got)
	}

	dec := json.NewDecoder(&logs)
	for {
		var record struct {
			Level string `json:"level"`
			Msg   string `json:"msg"`
			Name  string `json:"name"`
			Size  int64  `json:"size_bytes"`
		}
		err := dec.Decode(&record)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		size, ok := abandoned[record.Name]
		if record.Level != "INFO" || record.Msg != "abandoned update artifact removed" || !ok || record.Size != size {
			t.Fatalf("unexpected log record: %+v", record)
		}
		delete(abandoned, record.Name)
	}
	if len(abandoned) != 0 {
		t.Fatalf("missing removal logs for %v", abandoned)
	}
}

func TestNewSurvivesUnreadableUpdateDir(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := st.Path(store.UpdateDir)
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	u, err := New(Options{Store: st, Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	if state, target := u.State(); state != ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_UP_TO_DATE || target != "" {
		t.Fatalf("state after listing failure: %v, target %q", state, target)
	}
	var record struct {
		Level string `json:"level"`
		Msg   string `json:"msg"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(logs.Bytes(), &record); err != nil {
		t.Fatalf("want one log entry: %v (%s)", err, logs.String())
	}
	if record.Level != "WARN" || record.Msg != "update directory listing failed" || record.Error == "" {
		t.Fatalf("unexpected log record: %+v", record)
	}
}

func TestDownloadAfterRestartLeavesOneArtifact(t *testing.T) {
	busy := &atomic.Bool{}
	busy.Store(true)
	before, installer, _ := newUpdater(t, newRelease(t, nil), "1.0.0", busy)
	st := before.opts.Store
	writeUpdateFile(t, st, "ownsand-agent-1.0.5-3", 512)

	after, err := New(before.opts)
	if err != nil {
		t.Fatal(err)
	}
	requireUpdateEntries(t, st)
	if err := update(after, "1.1.0", false); err != nil {
		t.Fatal(err)
	}
	requireUpdateEntries(t, st, "ownsand-agent-1.1.0-1")

	busy.Store(false)
	if installed, err := after.TryInstall(); !installed || err != nil || installer.installed.Load() != 1 {
		t.Fatalf("install = %t, %v, calls = %d", installed, err, installer.installed.Load())
	}
	requireUpdateEntries(t, st)
}

func TestNewKeepsUpdateStateAfterConfirmedInstall(t *testing.T) {
	r := newRelease(t, nil)
	for _, tc := range []struct {
		running string
		state   ownsandv1.AgentUpdateState
		target  string
	}{
		{"1.1.0", ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_UP_TO_DATE, ""},
		{"1.0.0", ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_FAILED, "1.1.0"},
	} {
		t.Run(tc.state.String(), func(t *testing.T) {
			busy := &atomic.Bool{}
			busy.Store(true)
			before, _, _ := newUpdater(t, r, "1.0.0", busy)
			st := before.opts.Store
			if err := update(before, "1.1.0", false); err != nil {
				t.Fatal(err)
			}
			busy.Store(false)
			if installed, err := before.TryInstall(); !installed || err != nil {
				t.Fatalf("install = %t, %v", installed, err)
			}
			requireUpdateEntries(t, st)
			// Model a process that stopped just after replacing its executable.
			writeUpdateFile(t, st, "ownsand-agent-1.1.0-1", 10)
			after, err := New(Options{Store: st, Running: tc.running})
			if err != nil {
				t.Fatal(err)
			}
			requireUpdateEntries(t, st)
			if state, target := after.State(); state != tc.state || target != tc.target {
				t.Fatalf("state = %v, target = %q, want %v, %q", state, target, tc.state, tc.target)
			}
			saved, err := st.LoadAgent()
			if err != nil {
				t.Fatal(err)
			}
			if saved.PendingUpdate != nil {
				t.Fatal("restart did not clear the pending update")
			}
		})
	}
}

func writeUpdateFile(t *testing.T, st *store.Store, name string, size int) {
	t.Helper()
	if err := os.WriteFile(st.Path(store.UpdateDir, name), make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
}

func requireUpdateEntries(t *testing.T, st *store.Store, want ...string) {
	t.Helper()
	entries, err := os.ReadDir(st.Path(store.UpdateDir))
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(entries))
	for _, entry := range entries {
		got = append(got, entry.Name())
	}
	if !slices.Equal(got, want) {
		t.Fatalf("update directory entries = %v, want %v", got, want)
	}
}
