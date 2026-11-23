// SPDX-License-Identifier: Apache-2.0

package credstore

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

const (
	envHelperDir = "CREDSTORE_TEST_HELPER_DIR"
	// envHelperSkipLock makes the helper announce the lock without taking
	// it. The test uses it to prove that the contention check can fail.
	envHelperSkipLock = "CREDSTORE_TEST_HELPER_SKIP_LOCK"

	helperLockedLine = "locked"
	helperReleaseMsg = "release\n"

	// helperExitDeadline only bounds a hung helper. No assertion depends on
	// how long the helper or the parent actually take.
	helperExitDeadline = 30 * time.Second
)

// TestHelperHoldLock is not a test: the parent runs it in a child process,
// which takes the store lock, announces it and holds it until the parent
// writes a line to its standard input (or closes the pipe).
func TestHelperHoldLock(t *testing.T) {
	dir := os.Getenv(envHelperDir)
	if dir == "" {
		t.Skip("helper process only")
	}
	if os.Getenv(envHelperSkipLock) == "" {
		unlock, err := Store{Dir: dir}.lock()
		if err != nil {
			t.Fatal(err)
		}
		defer unlock()
	}
	fmt.Println(helperLockedLine)
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
}

// startLockHelper starts the helper process and waits until it announces the
// lock. The returned release function tells the helper to let go and returns
// the outcome of the helper process. The helper is killed and reaped when the
// test ends, whatever happens.
func startLockHelper(t *testing.T, dir string, skipLock bool) (release func() error) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperHoldLock$")
	cmd.Env = append(os.Environ(), envHelperDir+"="+dir)
	if skipLock {
		cmd.Env = append(cmd.Env, envHelperSkipLock+"=1")
	}
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
	type announcement struct {
		line string
		err  error
	}
	announced := make(chan announcement, 1)
	done := make(chan error, 1)
	// Register the kill and reap right after Start, so that no failure path
	// below (a hang, an early exit or a bad line) can orphan the helper.
	// Killing an exited process is a harmless no-op; the kill also closes
	// the helper's stdout, which lets the reader goroutine below finish.
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = stdin.Close()
		select {
		case <-done:
		case <-time.After(helperExitDeadline):
		}
	})
	// Read in a goroutine: cmd.Wait must not run before all reads of the
	// stdout pipe have completed, and the read itself must not block the test
	// without a deadline.
	go func() {
		reader := bufio.NewReader(stdout)
		line, err := reader.ReadString('\n')
		announced <- announcement{line: line, err: err}
		// Drain the rest of the output so the helper never blocks on it.
		_, _ = io.Copy(io.Discard, reader)
		done <- cmd.Wait()
	}()
	select {
	case a := <-announced:
		if a.err != nil {
			t.Fatalf("lock helper process exited or closed its output before announcing the lock: %v", a.err)
		}
		if a.line != helperLockedLine+"\n" && a.line != helperLockedLine+"\r\n" {
			t.Fatalf("unexpected lock helper output %q", a.line)
		}
	case <-time.After(helperExitDeadline):
		t.Fatalf("lock helper process did not announce the lock within %v", helperExitDeadline)
	}
	return func() error {
		if _, err := io.WriteString(stdin, helperReleaseMsg); err != nil {
			return err
		}
		_ = stdin.Close()
		select {
		case err := <-done:
			done <- err // keep the result for the cleanup above
			return err
		case <-time.After(helperExitDeadline):
			return errors.New("helper did not exit after the release signal")
		}
	}
}

// contenderAcquires reports whether a separately opened handle on the lock
// file can take the lock right now, without waiting. It never keeps the lock.
func contenderAcquires(t *testing.T, dir string) bool {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(dir, LockFileName), os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	ok, err := tryLockFile(f)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		_ = unlockFile(f)
	}
	return ok
}

// TestLockExcludesOtherProcesses checks that the lock works between
// processes, which is what the CLI instances need. It does not depend on how
// long the helper holds the lock: the helper holds it until the parent
// releases it, so the parent may be delayed arbitrarily after the helper
// announces the lock.
func TestLockExcludesOtherProcesses(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	if err := s.Save(&Credentials{RefreshToken: "r"}); err != nil {
		t.Fatal(err)
	}
	release := startLockHelper(t, s.Dir, false)
	if contenderAcquires(t, s.Dir) {
		t.Fatal("a contender took the lock before the helper released it")
	}
	if err := release(); err != nil {
		t.Fatalf("helper failed: %v", err)
	}
	if err := s.Delete(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("credential file still present after Delete: stat error is %T", err)
	}
}

// TestLockContentionCheckDetectsMissingLock is the counterfactual of
// TestLockExcludesOtherProcesses: when the helper announces the lock without
// taking it, the contention check must see the unexpected acquisition.
func TestLockContentionCheckDetectsMissingLock(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	if err := s.Save(&Credentials{RefreshToken: "r"}); err != nil {
		t.Fatal(err)
	}
	release := startLockHelper(t, s.Dir, true)
	if !contenderAcquires(t, s.Dir) {
		t.Fatal("the contention check did not notice that the helper held no lock")
	}
	if err := release(); err != nil {
		t.Fatalf("helper failed: %v", err)
	}
}
