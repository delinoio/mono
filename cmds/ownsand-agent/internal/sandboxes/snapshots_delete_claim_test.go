// SPDX-License-Identifier: FSL-1.1-ALv2

package sandboxes

import (
	"context"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func TestDeleteSnapshotCompletionSkipsReplacement(t *testing.T) {
	e := newEnv(t)
	sandboxID := e.create(10*time.Minute, 2*time.Hour).GetSandboxId()
	snapID, _ := uuidv7.NewString()
	ctx := context.Background()
	create := &ownsandv1.CreateSnapshotCommand{SnapshotId: snapID, SandboxId: sandboxID}
	del := &ownsandv1.DeleteSnapshotCommand{SnapshotId: snapID}
	if _, err := e.m.CreateSnapshot(ctx, create); err != nil {
		t.Fatal(err)
	}
	earlierClaim := e.m.ClaimSnapshotCreate(snapID)
	defer earlierClaim.Release()

	completeDelete := e.m.ReceiveSnapshotDelete(del)
	// A second delete removes the old entry while the first worker waits.
	if err := e.m.DeleteSnapshot(del); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.CreateSnapshot(ctx, create); err != nil {
		t.Fatalf("create replacement: %v", err)
	}
	if err := completeDelete(); err != nil {
		t.Fatalf("complete earlier delete: %v", err)
	}
	dir, entry, ok := e.m.SnapshotContent(snapID)
	if !ok || entry.GetState() != ownsandv1.SnapshotState_SNAPSHOT_STATE_READY {
		t.Fatalf("replacement = %v, want READY", entry)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("replacement content: %v", err)
	}

	entry, err := e.m.CreateClaimedSnapshot(ctx, create, earlierClaim)
	requireErrCode(t, err, ownsandv1.ErrorCode_ERROR_CODE_ABORTED, "create received before the delete")
	if entry != nil {
		t.Fatalf("earlier create returned replacement %v", entry)
	}
}

func TestDeletePublishedSnapshotCancelsWaitingDuplicate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reader bool
	}{
		{name: "immediate"},
		{name: "deferred_by_reader", reader: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			sandboxID := e.create(10*time.Minute, 2*time.Hour).GetSandboxId()
			snapID, _ := uuidv7.NewString()
			ctx := context.Background()
			cmd := &ownsandv1.CreateSnapshotCommand{SnapshotId: snapID, SandboxId: sandboxID}

			unlock := lockSandbox(t, e, sandboxID)
			claim := e.m.ClaimSnapshotCreate(snapID)
			defer claim.Release()
			done := make(chan claimedCreateResult, 1)
			defer func() {
				unlock()
				for range done {
				}
			}()
			go func() {
				defer close(done)
				entry, err := e.m.CreateClaimedSnapshot(ctx, cmd, claim)
				done <- claimedCreateResult{entry, err}
			}()
			awaitSnapshotCreateLockWait(t)

			// Publish a copy while the duplicate waits, as an earlier create
			// would do before releasing the sandbox lock.
			if _, err := e.m.AddImportedSnapshot(ctx, &ownsandv1.SnapshotStateEntry{
				SnapshotId: snapID, SourceSandboxId: sandboxID, GuestOs: ownsandv1.GuestOs_GUEST_OS_LINUX,
			}); err != nil {
				t.Fatal(err)
			}
			var releaseReader func()
			if tc.reader {
				_, release, ok := e.m.AcquireSnapshot(snapID)
				if !ok {
					t.Fatal("published snapshot is not usable")
				}
				releaseReader = release
				defer releaseReader()
			}
			if err := e.m.DeleteSnapshot(&ownsandv1.DeleteSnapshotCommand{SnapshotId: snapID}); err != nil {
				t.Fatal(err)
			}
			if tc.reader {
				if !e.m.HasSnapshot(snapID) {
					t.Fatal("delete removed the snapshot while a reader held it")
				}
				releaseReader()
			}
			if e.m.HasSnapshot(snapID) {
				t.Fatal("delete left the snapshot published")
			}

			unlock()
			res := <-done
			requireErrCode(t, res.err, ownsandv1.ErrorCode_ERROR_CODE_ABORTED, "waiting duplicate create")
			if res.entry != nil {
				t.Errorf("waiting duplicate entry = %v, want nil", res.entry)
			}
			claim.Release()
			requireNoSnapshotTrace(t, e, snapID)

			entry, err := e.m.CreateSnapshot(ctx, cmd)
			if err != nil || entry.GetState() != ownsandv1.SnapshotState_SNAPSHOT_STATE_READY {
				t.Fatalf("new create after deletion = %v, %v; want a ready snapshot", entry, err)
			}
		})
	}
}

// The mutex has no wait notification. Check the goroutine state before
// publishing the copy so the duplicate has already passed its first lookup.
func awaitSnapshotCreateLockWait(t *testing.T) {
	t.Helper()
	buf := make([]byte, 64<<10)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		n := runtime.Stack(buf, true)
		if n == len(buf) {
			buf = make([]byte, 2*len(buf))
			continue
		}
		for trace := range strings.SplitSeq(string(buf[:n]), "\n\n") {
			header, _, _ := strings.Cut(trace, "\n")
			blocked := strings.Contains(header, "sync.Mutex.Lock") || strings.Contains(header, "semacquire")
			if blocked && strings.Contains(trace, "(*Manager).CreateClaimedSnapshot") {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("duplicate create did not wait for the sandbox lock")
}
