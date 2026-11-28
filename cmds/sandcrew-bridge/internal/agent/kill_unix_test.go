// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package agent

import (
	"bufio"
	"errors"
	"io"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestKillEndsAToolThatIgnoresSIGTERM starts a leader that exits on SIGTERM
// and a child in its process group that ignores SIGTERM. Kill must still
// send SIGKILL to the group, and Wait must return only after that.
func TestKillEndsAToolThatIgnoresSIGTERM(t *testing.T) {
	old := killGrace
	killGrace = 300 * time.Millisecond
	defer func() { killGrace = old }()

	// Ignored signals stay ignored across exec, so sleep ignores SIGTERM.
	// The child prints its PID after the trap is set, and the inner sh
	// becomes the sleep with the same PID. Like a tool that the adapter
	// starts with its own pipes, the child does not keep the agent's
	// stderr.
	script := `(trap '' TERM; exec sh -c 'echo $$; exec sleep 60 2>/dev/null') & wait`
	process, err := ExecProcess("sh", []string{"-c", script}, t.TempDir(), nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(process.Stdout()).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	child, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatal(err)
	}
	waited := make(chan Exit, 1)
	go func() { waited <- process.Wait() }()
	process.Kill()
	select {
	case <-waited:
	case <-time.After(10 * time.Second):
		t.Fatal("Wait did not return after Kill")
	}
	// The orphaned child is reaped by init after the SIGKILL.
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := syscall.Kill(child, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			syscall.Kill(child, syscall.SIGKILL)
			t.Fatalf("the child %d that ignores SIGTERM still runs after Kill (kill 0: %v)", child, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
