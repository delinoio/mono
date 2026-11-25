// SPDX-License-Identifier: FSL-1.1-ALv2

package service

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
)

func TestLauncherRecoversInterruptedPayloadPublication(t *testing.T) {
	for _, tc := range []struct {
		name       string
		moveOld    bool
		publishNew bool
	}{
		{name: "after staging"},
		{name: "after moving the running executable", moveOld: true},
		{name: "after publication", moveOld: true, publishNew: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payloads := Payloads{Dir: t.TempDir(), Write: store.WriteFileAtomic}
			old, next := []byte("prior executable"), []byte("new executable")
			if err := payloads.Initialize(old); err != nil {
				t.Fatal(err)
			}
			running, err := payloads.Recover()
			if err != nil {
				t.Fatal(err)
			}
			stagePayload(t, running.Path, next, tc.moveOld, tc.publishNew)

			selected, err := payloads.Recover()
			if err != nil {
				t.Fatal(err)
			}
			if tc.publishNew {
				assertPayloadBytes(t, selected.Path, next)
			} else {
				assertPayloadBytes(t, selected.Path, old)
				// Retry the full publication sequence from the recovered path.
				stagePayload(t, selected.Path, next, true, true)
				selected, err = payloads.Recover()
				if err != nil {
					t.Fatal(err)
				}
				assertPayloadBytes(t, selected.Path, next)
			}
			if err := payloads.Prune(selected); err != nil {
				t.Fatal(err)
			}
			assertPayloadBytes(t, payloads.retained(running.Digest), old)
		})
	}
}

func TestLauncherRetriesRecordingCommittedPayload(t *testing.T) {
	for _, point := range []string{"payload-", "run-", payloadSelection} {
		t.Run(point, func(t *testing.T) {
			payloads := Payloads{Dir: t.TempDir(), Write: store.WriteFileAtomic}
			old, next := []byte("prior executable"), []byte("new executable")
			if err := payloads.Initialize(old); err != nil {
				t.Fatal(err)
			}
			running, err := payloads.Recover()
			if err != nil {
				t.Fatal(err)
			}
			stagePayload(t, running.Path, next, true, true)

			interrupted := errors.New("launcher interrupted")
			payloads.Write = func(path string, data []byte, perm fs.FileMode) error {
				if filepath.Base(path) == point || filepath.Base(path) == point+digestBytes(next)+".exe" {
					return interrupted
				}
				return store.WriteFileAtomic(path, data, perm)
			}
			if _, err := payloads.Recover(); !errors.Is(err, interrupted) {
				t.Fatalf("Recover returned %v, want interruption", err)
			}
			assertPayloadBytes(t, payloads.retained(running.Digest), old)
			payloads.Write = store.WriteFileAtomic
			selected, err := payloads.Recover()
			if err != nil {
				t.Fatal(err)
			}
			assertPayloadBytes(t, selected.Path, next)
			// Simulate another startup after the launcher committed its record.
			restarted, err := payloads.Recover()
			if err != nil || restarted != selected {
				t.Fatalf("Recover after commit = %+v, %v, want %+v", restarted, err, selected)
			}
		})
	}
}

func stagePayload(t *testing.T, running string, data []byte, moveOld, publishNew bool) {
	t.Helper()
	if err := store.WriteFileAtomic(running+".new", data, 0o700); err != nil {
		t.Fatal(err)
	}
	if moveOld {
		_ = os.Remove(running + ".old")
		if err := os.Rename(running, running+".old"); err != nil {
			t.Fatal(err)
		}
	}
	if publishNew {
		if err := os.Rename(running+".new", running); err != nil {
			t.Fatal(err)
		}
	}
}

func assertPayloadBytes(t *testing.T, path string, want []byte) {
	t.Helper()
	if got, err := os.ReadFile(path); err != nil || string(got) != string(want) {
		t.Fatalf("read %s = %q, %v, want %q", path, got, err, want)
	}
}
