// SPDX-License-Identifier: FSL-1.1-ALv2

package sandboxes

import (
	"context"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

const snapshotCreateLockWaitTimeout = 5 * time.Second

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

			finish, done := startSnapshotCreatesWaitingForLock(t, e, cmd, 1)

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

			finish()
			res := <-done
			requireErrCode(t, res.err, ownsandv1.ErrorCode_ERROR_CODE_ABORTED, "waiting duplicate create")
			if res.entry != nil {
				t.Errorf("waiting duplicate entry = %v, want nil", res.entry)
			}
			requireNoSnapshotTrace(t, e, snapID)

			entry, err := e.m.CreateSnapshot(ctx, cmd)
			if err != nil || entry.GetState() != ownsandv1.SnapshotState_SNAPSHOT_STATE_READY {
				t.Fatalf("new create after deletion = %v, %v; want a ready snapshot", entry, err)
			}
		})
	}
}

// startSnapshotCreatesWaitingForLock holds the sandbox command lock and starts
// each claimed create after the previous ones are waiting. Calling finish
// releases the lock, joins the workers, and releases their claims.
func startSnapshotCreatesWaitingForLock(t *testing.T, e *env, cmd *ownsandv1.CreateSnapshotCommand, count int) (finish func(), results <-chan claimedCreateResult) {
	t.Helper()
	unlock := lockSandbox(t, e, cmd.GetSandboxId())
	claims := make([]*SnapshotCreateClaim, count)
	for i := range claims {
		claims[i] = e.m.ClaimSnapshotCreate(cmd.GetSnapshotId())
	}
	done := make(chan claimedCreateResult, count)
	var workers sync.WaitGroup
	finish = sync.OnceFunc(func() {
		unlock()
		workers.Wait()
		for _, claim := range claims {
			claim.Release()
		}
	})
	t.Cleanup(finish)

	// The lookup hook identifies the worker after it releases the manager and
	// hook mutexes. It can then block only on the command lock this fixture holds.
	afterLookup := make(chan string, count)
	e.m.SetSnapshotLookupHookForTest(func(id string) {
		if id == cmd.GetSnapshotId() {
			var buf [64]byte
			n := runtime.Stack(buf[:], false)
			// The "goroutine N" prefix stays the same when this worker blocks.
			worker, _, _ := strings.Cut(string(buf[:n]), " [")
			afterLookup <- worker
		}
	})
	defer e.m.SetSnapshotLookupHookForTest(nil)

	waiters := make(map[string]struct{}, count)
	for _, claim := range claims {
		workers.Go(func() {
			entry, err := e.m.CreateClaimedSnapshot(context.Background(), cmd, claim)
			done <- claimedCreateResult{entry, err}
		})
		deadline := time.Now().Add(snapshotCreateLockWaitTimeout)
		select {
		case worker := <-afterLookup:
			if _, seen := waiters[worker]; seen {
				t.Fatalf("snapshot lookup reported the same worker twice: %s", worker)
			}
			waiters[worker] = struct{}{}
		case <-time.After(time.Until(deadline)):
			t.Fatalf("snapshot create worker %d did not reach the sandbox lock", len(waiters)+1)
		}
		awaitSnapshotCreateLockWaiters(t, deadline, waiters)
	}
	return finish, done
}

func awaitSnapshotCreateLockWait(t *testing.T) {
	t.Helper()
	awaitSnapshotCreateLockWaiters(t, time.Now().Add(snapshotCreateLockWaitTimeout), nil)
}

// The mutex has no wait notification. Count the identified workers whose stack
// traces show a mutex wait in CreateClaimedSnapshot. With no worker identities,
// one matching trace is enough for the duplicate-create race fixture.
func awaitSnapshotCreateLockWaiters(t *testing.T, deadline time.Time, waiters map[string]struct{}) {
	t.Helper()
	want := max(len(waiters), 1)
	buf := make([]byte, 64<<10)
	waiting := 0
	for time.Now().Before(deadline) {
		n := runtime.Stack(buf, true)
		if n == len(buf) {
			buf = make([]byte, 2*len(buf))
			continue
		}
		waiting = 0
		for trace := range strings.SplitSeq(string(buf[:n]), "\n\n") {
			header, _, _ := strings.Cut(trace, "\n")
			worker, _, _ := strings.Cut(header, " [")
			if _, expected := waiters[worker]; len(waiters) > 0 && !expected {
				continue
			}
			blocked := strings.Contains(header, "sync.Mutex.Lock") || strings.Contains(header, "semacquire")
			if blocked && strings.Contains(trace, "(*Manager).CreateClaimedSnapshot") {
				waiting++
			}
		}
		if waiting >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("snapshot create workers waiting for the sandbox lock = %d, want %d", waiting, want)
}
