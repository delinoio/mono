// SPDX-License-Identifier: FSL-1.1-ALv2

package guest

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// forkReleaseWait bounds how long newAfterRelease retries a start that finds
// the state directory lock still held.
const forkReleaseWait = 5 * time.Second

// newAfterRelease starts a server on a state directory that an earlier server
// may just have released.
//
// Workaround: the tests of this package run in parallel, and some of them run
// commands, which forks the test process. Until the child calls exec, it holds
// a copy of every open file descriptor of the process, including the state
// directory lock of a server that another test closes at that moment. The
// flock then stays held after Close, and a replacement on the same directory
// fails with ErrStateDirInUse. On Linux, os/exec forks with vfork while it
// holds syscall.ForkLock for writing, so taking the read lock waits until
// every pending child has called exec. Other systems release ForkLock before
// the child calls exec, so a start that still finds the lock held is retried
// for up to forkReleaseWait. Scope: test starts on a state directory that an
// earlier server released; a test that expects ErrStateDirInUse calls New
// directly. Remove it when the tests that run commands no longer run in
// parallel with the tests that restart a server.
func newAfterRelease(cfg Config) (*Server, error) {
	syscall.ForkLock.RLock()
	syscall.ForkLock.RUnlock()
	deadline := time.Now().Add(forkReleaseWait)
	for {
		srv, err := New(cfg)
		if !errors.Is(err, ErrStateDirInUse) || time.Now().After(deadline) {
			return srv, err
		}
		time.Sleep(time.Millisecond)
	}
}

func TestNewRejectsSecondServerOnStateDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	first, err := New(Config{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	// A retained read snapshot of the running helper.
	snapshot := filepath.Join(dir, "reads", "snapshot")
	if err := os.WriteFile(snapshot, []byte("kept"), 0o600); err != nil {
		t.Fatal(err)
	}

	second, err := New(Config{StateDir: dir})
	if err == nil {
		second.Close()
		t.Fatal("a second server started on a state directory in use")
	}
	if !errors.Is(err, ErrStateDirInUse) {
		t.Fatalf("error %v, want ErrStateDirInUse", err)
	}
	if strings.Contains(err.Error(), dir) {
		t.Fatalf("error %q contains the state directory path", err)
	}
	if data, err := os.ReadFile(snapshot); err != nil || string(data) != "kept" {
		t.Fatalf("snapshot of the running helper after the rejected start: %q, %v", data, err)
	}

	// After the first server released the directory, a replacement starts
	// and clears the snapshots of the earlier helper.
	first.Close()
	first.Close() // Close is idempotent.
	replacement, err := newAfterRelease(Config{StateDir: dir})
	if err != nil {
		t.Fatalf("replacement after Close: %v", err)
	}
	defer replacement.Close()
	if _, err := os.Stat(snapshot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("obsolete snapshot survived the replacement: %v", err)
	}
}

func TestNewReleasesStateDirAfterFailedStart(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// A regular file where the write records directory belongs makes the
	// start fail after the lock was taken.
	blocker := filepath.Join(dir, "writes")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if srv, err := New(Config{StateDir: dir}); err == nil {
		srv.Close()
		t.Fatal("start succeeded with a file in place of the write records directory")
	} else if errors.Is(err, ErrStateDirInUse) {
		t.Fatalf("unexpected lock error: %v", err)
	}
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	srv, err := newAfterRelease(Config{StateDir: dir})
	if err != nil {
		t.Fatalf("start after a failed start: %v", err)
	}
	srv.Close()
}
