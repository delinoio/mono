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

// TestSnapshotResumeFailureRecordsSourcePaused checks that a snapshot whose
// runtime paused the running source and could not resume it fails, publishes
// no capture, publishes the source as PAUSED with a newer revision, and that
// one explicit resume then reaches the runtime. The fake runtime runs the
// capture through the shared vmruntime.SnapshotWithPause helper, like the
// macOS and Hyper-V runtimes.
func TestSnapshotResumeFailureRecordsSourcePaused(t *testing.T) {
	e := newEnv(t)
	e.rt.snapshotViaHelper = true
	id := e.create(10*time.Minute, 2*time.Hour).GetSandboxId()
	before := e.out.lastEntry(id)
	e.m.AckSandboxUpdate(&ownsandv1.SandboxUpdateAck{SandboxId: id, Revision: before.GetRevision()})
	snapID, _ := uuidv7.NewString()

	e.rt.mu.Lock()
	e.rt.resumeErr = errors.New("hcs: resume rejected")
	e.rt.mu.Unlock()
	entry, err := e.m.CreateSnapshot(context.Background(), &ownsandv1.CreateSnapshotCommand{SnapshotId: snapID, SandboxId: id})
	requireSnapshotFailedClean(t, e, snapID, entry, err)
	if got := e.sandboxState(id); got != ownsandv1.SandboxState_SANDBOX_STATE_PAUSED {
		t.Fatalf("source state %v, want PAUSED", got)
	}
	last := e.out.lastEntry(id)
	if last.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_PAUSED || last.GetRevision().GetCounter() <= before.GetRevision().GetCounter() {
		t.Fatalf("published %v, want PAUSED with a revision newer than %d", last, before.GetRevision().GetCounter())
	}
	if got := e.m.StateReport(2, nil).GetSandboxes()[0].GetState(); got != ownsandv1.SandboxState_SANDBOX_STATE_PAUSED {
		t.Fatalf("state report %v, want PAUSED", got)
	}

	// The runtime accepts a resume again; one explicit resume reaches it.
	e.rt.mu.Lock()
	e.rt.resumeErr = nil
	e.rt.mu.Unlock()
	requireResumeReachesRuntime(t, e, id)
}

// TestSnapshotCaptureAndResumeFailureRecordsSourcePaused checks that a
// capture error stays the command result while the paused source is still
// recorded.
func TestSnapshotCaptureAndResumeFailureRecordsSourcePaused(t *testing.T) {
	e := newEnv(t)
	e.rt.snapshotViaHelper = true
	id := e.create(10*time.Minute, 2*time.Hour).GetSandboxId()
	snapID, _ := uuidv7.NewString()
	e.rt.mu.Lock()
	e.rt.snapshotErr = errors.New("disk copy failed")
	e.rt.resumeErr = errors.New("hcs: resume rejected")
	e.rt.mu.Unlock()
	entry, err := e.m.CreateSnapshot(context.Background(), &ownsandv1.CreateSnapshotCommand{SnapshotId: snapID, SandboxId: id})
	requireSnapshotFailedClean(t, e, snapID, entry, err)
	if got := e.sandboxState(id); got != ownsandv1.SandboxState_SANDBOX_STATE_PAUSED {
		t.Fatalf("source state %v, want PAUSED", got)
	}
	e.rt.mu.Lock()
	e.rt.resumeErr = nil
	e.rt.mu.Unlock()
	requireResumeReachesRuntime(t, e, id)
}

func TestSnapshotTerminalSourceReleasesCommandLock(t *testing.T) {
	e := newEnv(t)
	id := e.create(time.Hour, 2*time.Hour).GetSandboxId()
	snapID, err := uuidv7.NewString()
	if err != nil {
		t.Fatal(err)
	}

	started, release := make(chan struct{}), make(chan struct{})
	e.rt.snapStarted, e.rt.snapBlock = started, release
	e.rt.snapshotErr = errors.New("resume after snapshot: VM reached a terminal state")
	source := e.rt.vms[id]
	go func() {
		<-started
		source.stop()
		close(release)
	}()

	entry, err := e.m.CreateSnapshot(context.Background(), &ownsandv1.CreateSnapshotCommand{SnapshotId: snapID, SandboxId: id})
	requireSnapshotFailedClean(t, e, snapID, entry, err)
	waitState(t, e.m, id, ownsandv1.SandboxState_SANDBOX_STATE_FAILED)
	if reason := endReason(e.m, id); reason != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_LOST_ON_DEVICE {
		t.Fatalf("source end reason %v, want LOST_ON_DEVICE", reason)
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
	t.Run("capture error with successful resume", func(t *testing.T) {
		e := newEnv(t)
		e.rt.snapshotViaHelper = true
		id := e.create(10*time.Minute, 2*time.Hour).GetSandboxId()
		snapID, _ := uuidv7.NewString()
		e.rt.mu.Lock()
		e.rt.snapshotErr = errors.New("disk copy failed")
		e.rt.mu.Unlock()
		entry, err := e.m.CreateSnapshot(context.Background(), &ownsandv1.CreateSnapshotCommand{SnapshotId: snapID, SandboxId: id})
		requireSnapshotFailedClean(t, e, snapID, entry, err)
		if got := e.sandboxState(id); got != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
			t.Fatalf("source state %v, want RUNNING", got)
		}
	})
	t.Run("successful snapshot", func(t *testing.T) {
		e := newEnv(t)
		e.rt.snapshotViaHelper = true
		id := e.create(10*time.Minute, 2*time.Hour).GetSandboxId()
		snapID, _ := uuidv7.NewString()
		en, err := e.m.CreateSnapshot(context.Background(), &ownsandv1.CreateSnapshotCommand{SnapshotId: snapID, SandboxId: id})
		if err != nil || en.GetState() != ownsandv1.SnapshotState_SNAPSHOT_STATE_READY {
			t.Fatalf("snapshot %v %v", en, err)
		}
		if got := e.sandboxState(id); got != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
			t.Fatalf("source state %v, want RUNNING", got)
		}
	})
	t.Run("already paused source is not resumed", func(t *testing.T) {
		e := newEnv(t)
		e.rt.snapshotViaHelper = true
		id := e.create(10*time.Minute, 2*time.Hour).GetSandboxId()
		if _, err := e.m.Pause(context.Background(), &ownsandv1.PauseSandboxCommand{SandboxId: id}); err != nil {
			t.Fatal(err)
		}
		// A resume of this fake would fail; the capture must not call it.
		e.rt.mu.Lock()
		e.rt.resumeErr = errors.New("must not run")
		e.rt.snapshotErr = errors.New("disk copy failed")
		e.rt.mu.Unlock()
		snapID, _ := uuidv7.NewString()
		entry, err := e.m.CreateSnapshot(context.Background(), &ownsandv1.CreateSnapshotCommand{SnapshotId: snapID, SandboxId: id})
		requireSnapshotFailedClean(t, e, snapID, entry, err)
		if got := e.sandboxState(id); got != ownsandv1.SandboxState_SANDBOX_STATE_PAUSED {
			t.Fatalf("source state %v, want PAUSED", got)
		}
		if got := e.resumeCalls(); got != 0 {
			t.Fatalf("%d runtime Resume calls for a source that was already paused", got)
		}
		e.rt.mu.Lock()
		e.rt.snapshotErr = nil
		e.rt.mu.Unlock()
		snapID, _ = uuidv7.NewString()
		en, err := e.m.CreateSnapshot(context.Background(), &ownsandv1.CreateSnapshotCommand{SnapshotId: snapID, SandboxId: id})
		if err != nil || en.GetState() != ownsandv1.SnapshotState_SNAPSHOT_STATE_READY {
			t.Fatalf("snapshot of the paused source %v %v", en, err)
		}
		if got := e.sandboxState(id); got != ownsandv1.SandboxState_SANDBOX_STATE_PAUSED || e.resumeCalls() != 0 {
			t.Fatalf("source state %v after %d resumes, want PAUSED and none", got, e.resumeCalls())
		}
	})
}

// TestSnapshotPausedSourcePublicationFailureIsLogged checks that a source
// state that cannot be published is logged with the sandbox ID and that the
// snapshot stays failed.
func TestSnapshotPausedSourcePublicationFailureIsLogged(t *testing.T) {
	logs := &lockedBuffer{}
	e := newEnvWithLogger(t, slog.New(slog.NewJSONHandler(logs, nil)))
	e.rt.snapshotViaHelper = true
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
	e.rt.resumeErr = errors.New("hcs: resume rejected")
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
