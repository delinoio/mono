// SPDX-License-Identifier: FSL-1.1-ALv2

package sandboxes

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// lockedBuffer is a log sink that the manager and the test goroutine share.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// requireSnapshotFailedClean checks that a failed capture left no snapshot.
func requireSnapshotFailedClean(t *testing.T, e *env, snapID string, entry *ownsandv1.SnapshotStateEntry, err error) {
	t.Helper()
	var ce *CommandError
	if !errors.As(err, &ce) || ce.Code != ownsandv1.ErrorCode_ERROR_CODE_INTERNAL || entry != nil {
		t.Fatalf("snapshot %v %v, want an INTERNAL error and no entry", entry, err)
	}
	if e.m.HasSnapshot(snapID) {
		t.Fatal("failed snapshot is known to the manager")
	}
	if got := e.m.StateReport(2, nil).GetSnapshots(); len(got) != 0 {
		t.Fatalf("state report holds %v after a failed capture", got)
	}
	dir, derr := e.st.SnapshotDir(snapID)
	if derr != nil {
		t.Fatal(derr)
	}
	if _, serr := os.Stat(dir); !os.IsNotExist(serr) {
		t.Fatalf("snapshot directory %s remains: %v", dir, serr)
	}
}

func (e *env) sandboxState(id string) ownsandv1.SandboxState {
	e.t.Helper()
	s, ok := e.m.State(id)
	if !ok {
		e.t.Fatalf("sandbox %s unknown", id)
	}
	return s
}

func (e *env) resumeCalls() int {
	e.rt.mu.Lock()
	defer e.rt.mu.Unlock()
	return e.rt.resumeCalls
}

// requireResumeReachesRuntime sends one explicit resume and checks that it
// reaches the runtime and records RUNNING.
func requireResumeReachesRuntime(t *testing.T, e *env, id string) {
	t.Helper()
	before := e.resumeCalls()
	en, err := e.m.Resume(context.Background(), &ownsandv1.ResumeSandboxCommand{SandboxId: id})
	if err != nil || en.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		t.Fatalf("resume %v %v, want RUNNING", en, err)
	}
	if got := e.resumeCalls(); got != before+1 {
		t.Fatalf("%d runtime Resume calls, want %d: the explicit resume did not reach the runtime", got, before+1)
	}
	e.rt.mu.Lock()
	paused := e.rt.paused[id]
	e.rt.mu.Unlock()
	if paused {
		t.Fatal("the VM is still paused after the explicit resume")
	}
}

// TestSnapshotRuntimeReportedPausedSourceIsRecorded checks the outcome that a
// runtime returns directly with its error, like the Firecracker runtime.
func TestSnapshotRuntimeReportedPausedSourceIsRecorded(t *testing.T) {
	e := newEnv(t)
	id := e.create(10*time.Minute, 2*time.Hour).GetSandboxId()
	snapID, _ := uuidv7.NewString()
	e.rt.mu.Lock()
	e.rt.snapshotErr = errors.New("fcrt: resume after snapshot: rejected")
	e.rt.snapshotSourceState = ownsandv1.SandboxState_SANDBOX_STATE_PAUSED
	e.rt.paused[id] = true
	e.rt.mu.Unlock()
	entry, err := e.m.CreateSnapshot(context.Background(), &ownsandv1.CreateSnapshotCommand{SnapshotId: snapID, SandboxId: id})
	requireSnapshotFailedClean(t, e, snapID, entry, err)
	if got := e.sandboxState(id); got != ownsandv1.SandboxState_SANDBOX_STATE_PAUSED {
		t.Fatalf("source state %v, want PAUSED", got)
	}
	requireResumeReachesRuntime(t, e, id)
}

// TestSnapshotErrorsThatKeepTheSourceState checks the cases in which the
// source record does not change.
func TestSnapshotErrorsThatKeepTheSourceState(t *testing.T) {
	t.Run("ordinary error with unspecified outcome", func(t *testing.T) {
		e := newEnv(t)
		id := e.create(10*time.Minute, 2*time.Hour).GetSandboxId()
		snapID, _ := uuidv7.NewString()
		before := e.out.lastEntry(id)
		e.rt.mu.Lock()
		e.rt.snapshotErr = errors.New("disk copy failed")
		e.rt.mu.Unlock()
		entry, err := e.m.CreateSnapshot(context.Background(), &ownsandv1.CreateSnapshotCommand{SnapshotId: snapID, SandboxId: id})
		requireSnapshotFailedClean(t, e, snapID, entry, err)
		if got := e.sandboxState(id); got != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
			t.Fatalf("source state %v, want RUNNING", got)
		}
		if last := e.out.lastEntry(id); last != before {
			t.Fatalf("a state update %v was published, want none", last)
		}
	})
}

// TestSnapshotPausedSourcePublicationFailureIsLogged checks that a source
// state that cannot be published is logged with the sandbox ID and that the
// snapshot stays failed.
func TestSnapshotPausedSourcePublicationFailureIsLogged(t *testing.T) {
	logs := &lockedBuffer{}
	e := newEnvWithLogger(t, slog.New(slog.NewJSONHandler(logs, nil)))
	id := e.create(10*time.Minute, 2*time.Hour).GetSandboxId()
	n := e.out.count()
	snapID, _ := uuidv7.NewString()

	// A directory in place of agent.json makes the revision write fail, like
	// in the idle report test; the capture itself does not use that file.
	agentFile := e.st.Path(store.AgentFile)
	savedAgentFile := agentFile + ".saved"
	if err := os.Rename(agentFile, savedAgentFile); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(agentFile, 0o700); err != nil {
		t.Fatal(err)
	}
	restoreAgentFile := func() {
		if _, err := os.Stat(savedAgentFile); err != nil {
			return
		}
		if err := os.Remove(agentFile); err != nil {
			t.Error(err)
			return
		}
		if err := os.Rename(savedAgentFile, agentFile); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(restoreAgentFile)

	e.rt.mu.Lock()
	e.rt.snapshotErr = errors.New("fcrt: resume after snapshot: rejected")
	e.rt.snapshotSourceState = ownsandv1.SandboxState_SANDBOX_STATE_PAUSED
	e.rt.mu.Unlock()
	entry, err := e.m.CreateSnapshot(context.Background(), &ownsandv1.CreateSnapshotCommand{SnapshotId: snapID, SandboxId: id})
	requireSnapshotFailedClean(t, e, snapID, entry, err)
	if e.out.count() != n {
		t.Fatal("a sandbox update was published although the revision write failed")
	}
	if !strings.Contains(logs.String(), "could not be recorded as paused") || !strings.Contains(logs.String(), id) {
		t.Fatalf("no log with the sandbox ID %s and the publication failure:\n%s", id, logs.String())
	}
	restoreAgentFile()
}
