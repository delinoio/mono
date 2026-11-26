// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestWaitInitHelperReleasesPidfd(t *testing.T) {
	const subprocess = "OWNSAND_GUEST_TEST_INIT_PIDFD"
	if os.Getenv(subprocess) != "1" {
		// Wait4(-1) must run outside the main test process so it cannot
		// reap children started by other tests.
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestWaitInitHelperReleasesPidfd$", "-test.v")
		cmd.Env = append(os.Environ(), subprocess+"=1", "GOGC=off", "GOMEMLIMIT=off")
		out, err := cmd.CombinedOutput()
		t.Logf("wait subprocess output:\n%s", out)
		if err != nil {
			t.Fatalf("wait subprocess: %v", err)
		}
		if strings.Contains(string(out), "--- SKIP: TestWaitInitHelperReleasesPidfd") {
			t.Skip("Go process handles do not use pidfds in this environment")
		}
		return
	}

	baseline := countInitTestPidfds(t)
	// FindProcess uses the Go runtime's pidfd support check.
	probe, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	supported := countInitTestPidfds(t) > baseline
	if err := probe.Release(); err != nil {
		t.Fatal(err)
	}
	if !supported {
		t.Skip("Go process handles do not use pidfds in this environment")
	}

	for cycle := range 4 {
		cmd := exec.Command("/bin/true")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		pid := cmd.Process.Pid
		if got := countInitTestPidfds(t); got != baseline+1 {
			t.Fatalf("cycle %d: after start got %d pidfds, want %d", cycle, got, baseline+1)
		}
		if cycle == 3 {
			// Reap the child first to exercise the wait-error exit.
			if _, err := syscall.Wait4(pid, nil, 0, nil); err != nil {
				t.Fatal(err)
			}
		}
		waitInitHelper(cmd.Process)
		if got := countInitTestPidfds(t); got != baseline {
			t.Fatalf("cycle %d: after wait got %d pidfds, want %d", cycle, got, baseline)
		}
		if _, err := syscall.Wait4(pid, nil, syscall.WNOHANG, nil); !errors.Is(err, syscall.ECHILD) {
			t.Fatalf("cycle %d: wait after reap got %v, want ECHILD", cycle, err)
		}
	}
}

func countInitTestPidfds(t *testing.T) int {
	t.Helper()
	const fdDir = "/proc/self/fd"
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join(fdDir, entry.Name()))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if target == "anon_inode:[pidfd]" {
			count++
		}
	}
	return count
}
