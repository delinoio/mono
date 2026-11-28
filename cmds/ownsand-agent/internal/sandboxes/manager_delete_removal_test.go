// SPDX-License-Identifier: FSL-1.1-ALv2

package sandboxes

import (
	"bytes"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func TestDeleteRefusesUnknownAndTrackedSandboxesDuringRemoval(t *testing.T) {
	e := newEnv(t)
	id := e.create(time.Hour, time.Hour).GetSandboxId()
	unknown := e.createCmd(1, time.Hour, time.Hour).GetSandboxId()
	sb := e.m.sandboxes[id]
	canceled := false
	sb.createCancel = func() { canceled = true }

	if err := e.m.fenceSnapshotImports(t.Context()); err != nil {
		t.Fatal(err)
	}
	checkUnchanged := checkNoDeleteWrites(t, e, id)
	for _, deleteID := range []string{unknown, id} {
		entry, err := e.m.Delete(t.Context(), &ownsandv1.DeleteSandboxCommand{SandboxId: deleteID})
		requireFailedPrecondition(t, err)
		if entry != nil {
			t.Errorf("refused Delete returned %v", entry)
		}
	}
	checkUnchanged()
	if canceled || sb.ending {
		t.Error("refused Delete canceled a create or marked it as ending")
	}
	if e.m.StateEntry(unknown) != nil {
		t.Error("refused Delete left a marker in memory")
	}
	if _, err := os.Stat(e.st.Path(store.SandboxesDir, unknown+".json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stat of refused delete marker = %v, want not exist", err)
	}
}

func TestDeleteRechecksAdmissionAfterWaitingForSandboxCommand(t *testing.T) {
	for _, tc := range []struct {
		name     string
		removing bool
		want     ownsandv1.ErrorCode
	}{
		{"device removal", true, ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION},
		{"replaced record", false, ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			id := e.create(time.Hour, time.Hour).GetSandboxId()
			sb := e.m.sandboxes[id]
			lookedUp := make(chan struct{})
			// The cancel callback tells the test when Delete found the record
			// under m.mu. Keep cmdMu held until the admission state changes.
			sb.createCancel = func() { close(lookedUp) }
			sb.cmdMu.Lock()
			unlock := sync.OnceFunc(sb.cmdMu.Unlock)
			defer unlock()
			checkUnchanged := checkNoDeleteWrites(t, e, id)

			done := make(chan error, 1)
			go func() {
				entry, err := e.m.Delete(t.Context(), &ownsandv1.DeleteSandboxCommand{SandboxId: id})
				if entry != nil {
					t.Errorf("refused Delete returned %v", entry)
				}
				done <- err
			}()
			select {
			case <-lookedUp:
			case <-time.After(5 * time.Second):
				t.Fatal("Delete did not reach the sandbox lookup")
			}
			if tc.removing {
				if err := e.m.fenceSnapshotImports(t.Context()); err != nil {
					t.Fatal(err)
				}
			} else {
				// Keep the same ID but transfer ownership to a different record.
				e.m.mu.Lock()
				e.m.sandboxes[id] = &sandbox{rec: sb.rec}
				e.m.mu.Unlock()
			}
			unlock()

			select {
			case err := <-done:
				var ce *CommandError
				if !errors.As(err, &ce) || ce.Code != tc.want {
					t.Errorf("Delete error = %v, want %v", err, tc.want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Delete did not return after the command lock was released")
			}
			checkUnchanged()
		})
	}
}

func TestRemoveAllWaitsForAdmittedDeleteBeforeWipe(t *testing.T) {
	e := newEnv(t)
	id := e.create(time.Hour, time.Hour).GetSandboxId()
	sb := e.m.sandboxes[id]
	started, block := make(chan struct{}), make(chan struct{})
	e.rt.deleteStarted, e.rt.deleteBlock = started, block
	release := sync.OnceFunc(func() { close(block) })
	defer release()

	deleted := make(chan error, 1)
	go func() {
		_, err := e.m.Delete(t.Context(), &ownsandv1.DeleteSandboxCommand{SandboxId: id})
		deleted <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("Delete did not enter runtime teardown")
	}
	if sb.cmdMu.TryLock() {
		sb.cmdMu.Unlock()
		t.Error("Delete released the sandbox command lock during teardown")
	}

	removed := make(chan error, 1)
	go func() {
		if err := e.removeAllAndWipe(t.Context()); err != nil {
			removed <- err
			return
		}
		removed <- e.st.Reset()
	}()
	// Wait for RemoveAll to fence admission while Delete holds cmdMu.
	deadline := time.Now().Add(5 * time.Second)
	for !time.Now().After(deadline) {
		e.m.mu.Lock()
		removing := e.m.removing
		e.m.mu.Unlock()
		if removing {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if time.Now().After(deadline) {
		t.Fatal("RemoveAll did not close admission")
	}
	if got := persistedRecord(t, e.st, id).State; got != ownsandv1.SandboxState_SANDBOX_STATE_DELETING {
		t.Errorf("record while teardown is blocked = %v, want DELETING", got)
	}
	release()

	for _, done := range []<-chan error{deleted, removed} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("teardown or removal did not finish")
		}
	}
	if calls := e.rt.deleteCallCount(); calls != 1 {
		t.Errorf("runtime Delete calls = %d, want 1", calls)
	}
	entries, err := os.ReadDir(e.st.Dir())
	if err != nil || len(entries) != 1 || entries[0].Name() != store.EnrollmentLockFile {
		t.Errorf("state after removal = %v, %v, want only %s", entries, err, store.EnrollmentLockFile)
	}
}

func checkNoDeleteWrites(t *testing.T, e *env, id string) func() {
	t.Helper()
	read := func(path string) []byte {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	agentPath := e.st.Path(store.AgentFile)
	recordPath := e.st.Path(store.SandboxesDir, id+".json")
	agent, record := read(agentPath), read(recordPath)
	count := e.out.count()
	return func() {
		t.Helper()
		if !bytes.Equal(agent, read(agentPath)) || !bytes.Equal(record, read(recordPath)) {
			t.Error("refused Delete changed the agent revision or sandbox record")
		}
		if e.out.count() != count || e.rt.deleteCallCount() != 0 {
			t.Error("refused Delete sent a sandbox update or entered runtime teardown")
		}
	}
}
