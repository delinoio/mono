// SPDX-License-Identifier: FSL-1.1-ALv2

package store

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime"
	"testing"
	"testing/synctest"
	"time"
)

const lockHelperDirEnv = "OWNSAND_STORE_LOCK_HELPER_DIR"

// openStore returns a Store on dir without Open: the lock needs only the
// directory, and on Windows Open fails in a process that is not an elevated
// administrator (it restricts the directory to SYSTEM and Administrators), so
// these tests run in any Windows session.
func openStore(t *testing.T, dir string) *Store {
	t.Helper()
	return &Store{dir: dir}
}

// requireBusy checks that the enrollment lock of s is held elsewhere: a
// lock attempt that waits for the deadline fails with ErrEnrollmentBusy and
// the deadline error.
func requireBusy(t *testing.T, s *Store) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	unlock, err := s.LockEnrollment(ctx)
	if err == nil {
		unlock()
		t.Fatal("the enrollment lock was free; want it held")
	}
	if !errors.Is(err, ErrEnrollmentBusy) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("busy error = %v, want ErrEnrollmentBusy and DeadlineExceeded", err)
	}
}

func lockEnrollment(t *testing.T, s *Store) func() {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	unlock, err := s.LockEnrollment(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return unlock
}

func TestEnrollmentLockRejectsEndedContext(t *testing.T) {
	s := openStore(t, t.TempDir())
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	expired, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	for _, ctx := range []context.Context{canceled, expired} {
		t.Run(ctx.Err().Error(), func(t *testing.T) {
			unlock, err := s.LockEnrollment(ctx)
			if unlock != nil {
				unlock()
				t.Fatal("acquired the lock with an ended context")
			}
			if !errors.Is(err, ErrEnrollmentBusy) || !errors.Is(err, ctx.Err()) {
				t.Fatalf("lock error = %v, want ErrEnrollmentBusy and %v", err, ctx.Err())
			}
			lockEnrollment(t, s)()
		})
	}
}

func TestEnrollmentLockReleasesAcquisitionAfterCancellation(t *testing.T) {
	s := openStore(t, t.TempDir())
	releaseHolder := lockEnrollment(t, s)
	defer releaseHolder()
	f, err := openEnrollmentLock(s.Dir())
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	attempts := 0
	err = waitEnrollmentLock(ctx, f, func(f *os.File) (bool, error) {
		ok, err := tryLockFile(f)
		attempts++
		if attempts == 1 {
			if ok || err != nil {
				t.Fatalf("first attempt = (%t, %v), want contention", ok, err)
			}
			releaseHolder()
		} else {
			if !ok || err != nil {
				t.Fatalf("retry = (%t, %v), want acquisition", ok, err)
			}
			cancel()
		}
		return ok, err
	})
	if attempts != 2 || !errors.Is(err, ErrEnrollmentBusy) || !errors.Is(err, context.Canceled) {
		t.Fatalf("lock after %d attempts = %v, want ErrEnrollmentBusy and Canceled after two attempts", attempts, err)
	}
	// The descriptor is still open, so this also verifies the explicit unlock.
	lockEnrollment(t, s)()
}

// deadlineBeforeSignal models a context whose timer has not reported expiry yet.
type deadlineBeforeSignal struct {
	context.Context
	deadline time.Time
}

func (c deadlineBeforeSignal) Deadline() (time.Time, bool) { return c.deadline, true }

func TestEnrollmentLockChecksDeadlineBeforeRetry(t *testing.T) {
	s := openStore(t, t.TempDir())
	releaseHolder := lockEnrollment(t, s)
	defer releaseHolder()
	f, err := openEnrollmentLock(s.Dir())
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	synctest.Test(t, func(t *testing.T) {
		ctx := deadlineBeforeSignal{t.Context(), time.Now().Add(enrollmentLockPoll / 2)}
		attempts := 0
		err := waitEnrollmentLock(ctx, f, func(f *os.File) (bool, error) {
			attempts++
			if attempts != 1 {
				t.Fatal("retried after the deadline")
			}
			ok, err := tryLockFile(f)
			releaseHolder()
			return ok, err
		})
		if attempts != 1 || !errors.Is(err, ErrEnrollmentBusy) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("lock after %d attempts = %v, want ErrEnrollmentBusy and DeadlineExceeded after one attempt", attempts, err)
		}
	})
}

// TestEnrollmentLockExcludesOtherStores checks that the lock excludes
// another Store value on the same directory until it is released, that
// unlock is idempotent, and that another directory is independent.
func TestEnrollmentLockExcludesOtherStores(t *testing.T) {
	dir := t.TempDir()
	first, second := openStore(t, dir), openStore(t, dir)
	other := openStore(t, t.TempDir())

	unlock := lockEnrollment(t, first)
	requireBusy(t, second)
	// An unrelated state directory is not affected.
	lockEnrollment(t, other)()

	unlock()
	unlock() // a second call changes nothing
	lockEnrollment(t, second)()
}

// TestEnrollmentLockWaitsForRelease checks that a waiting attempt takes the
// lock once the holder releases it.
func TestEnrollmentLockWaitsForRelease(t *testing.T) {
	dir := t.TempDir()
	first, second := openStore(t, dir), openStore(t, dir)
	unlock := lockEnrollment(t, first)

	type result struct {
		unlock func()
		err    error
	}
	got := make(chan result, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		u, err := second.LockEnrollment(ctx)
		got <- result{u, err}
	}()
	select {
	case r := <-got:
		if r.unlock != nil {
			r.unlock()
		}
		t.Fatalf("the waiting attempt returned while the lock was held: %v", r.err)
	case <-time.After(100 * time.Millisecond):
	}
	unlock()
	r := <-got
	if r.err != nil {
		t.Fatal(r.err)
	}
	r.unlock()
}

// TestEnrollmentLockFileMode checks that the lock file is private where the
// platform has file modes.
func TestEnrollmentLockFileMode(t *testing.T) {
	s := openStore(t, t.TempDir())
	defer lockEnrollment(t, s)()
	info, err := os.Stat(s.Path(EnrollmentLockFile))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("lock file mode = %v, want 0600", info.Mode().Perm())
	}
}

// TestEnrollmentLockHelperProcess is the child of
// TestEnrollmentLockAcrossProcesses, not a test: it takes the enrollment
// lock of the directory in lockHelperDirEnv, prints "locked", and holds the
// lock until its standard input closes.
func TestEnrollmentLockHelperProcess(t *testing.T) {
	dir := os.Getenv(lockHelperDirEnv)
	if dir == "" {
		t.Skip("helper process of TestEnrollmentLockAcrossProcesses")
	}
	s := &Store{dir: dir}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	unlock, err := s.LockEnrollment(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	os.Stdout.WriteString("locked\n")
	io.Copy(io.Discard, os.Stdin)
}

// TestEnrollmentLockAcrossProcesses checks the lock between two processes:
// a child process (this test binary) holds the lock, the parent cannot take
// it, and the parent takes it after the child exits. It runs on every
// platform, so it covers LockFileEx on Windows.
func TestEnrollmentLockAcrossProcesses(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir)

	cmd := exec.Command(os.Args[0], "-test.run=^TestEnrollmentLockHelperProcess$", "-test.v")
	cmd.Env = append(os.Environ(), lockHelperDirEnv+"="+dir)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Kill and reap the child on every path, so a failed test leaves no
	// helper behind. Killing it also unblocks the read below.
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})
	watchdog := time.AfterFunc(20*time.Second, func() { cmd.Process.Kill() })
	defer watchdog.Stop()

	lines := bufio.NewScanner(stdout)
	for {
		if !lines.Scan() {
			t.Fatalf("the helper process ended before it took the lock: %v", lines.Err())
		}
		if lines.Text() == "locked" {
			break
		}
	}
	requireBusy(t, s)

	stdin.Close()
	// Drain the output so the child can exit, then wait for it.
	io.Copy(io.Discard, stdout)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("helper process: %v", err)
	}
	lockEnrollment(t, s)()
}
