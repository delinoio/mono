// SPDX-License-Identifier: Apache-2.0

package credstore

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"
)

const (
	envHelperDir  = "CREDSTORE_TEST_HELPER_DIR"
	envHelperHold = "CREDSTORE_TEST_HELPER_HOLD_MS"
)

// TestHelperHoldLock is not a test: the parent runs it in a child process,
// which takes the store lock, announces it and holds it for a while.
func TestHelperHoldLock(t *testing.T) {
	dir := os.Getenv(envHelperDir)
	if dir == "" {
		t.Skip("helper process only")
	}
	hold, _ := strconv.Atoi(os.Getenv(envHelperHold))
	unlock, err := Store{Dir: dir}.lock()
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println("locked")
	time.Sleep(time.Duration(hold) * time.Millisecond)
	unlock()
}

// TestLockExcludesOtherProcesses checks that the lock works between
// processes, which is what the CLI instances need.
func TestLockExcludesOtherProcesses(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	if err := s.Save(&Credentials{RefreshToken: "r"}); err != nil {
		t.Fatal(err)
	}
	const hold = 500 * time.Millisecond
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperHoldLock$")
	cmd.Env = append(os.Environ(), envHelperDir+"="+s.Dir, envHelperHold+"="+strconv.Itoa(int(hold/time.Millisecond)))
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Wait() }()
	// Wait until the child holds the lock.
	buf := make([]byte, 64)
	for total := 0; total < len("locked"); {
		n, err := out.Read(buf[total:])
		if err != nil {
			t.Fatalf("helper did not lock: %v", err)
		}
		total += n
	}
	start := time.Now()
	if err := s.Delete(); err != nil {
		t.Fatal(err)
	}
	if waited := time.Since(start); waited < hold/2 {
		t.Fatalf("Delete did not wait for the other process: %v", waited)
	}
}
