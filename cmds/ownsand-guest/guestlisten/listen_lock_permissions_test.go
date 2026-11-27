// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build linux || darwin

package guestlisten

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestUnixLockPermissions(t *testing.T) {
	const childEnv = "OWNSAND_TEST_UNIX_LOCK_PERMISSIONS"
	if os.Getenv(childEnv) != "1" {
		// Isolate the process-wide umask changes from other tests.
		binary, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(t.Context(), binary, "-test.run=^TestUnixLockPermissions$", "-test.v")
		cmd.Env = append(os.Environ(), childEnv+"=1")
		cmd.Dir = t.TempDir()
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("lock permissions subprocess: %v\n%s", err, output)
		}
		t.Logf("%s", output)
		return
	}

	t.Run("create and reopen", func(t *testing.T) {
		const socket = "reopen.sock"
		var first os.FileInfo
		for _, mask := range []int{0o200, 0o200, 0o077} {
			syscall.Umask(mask)
			listener, err := Listen("unix://" + socket)
			if err != nil {
				t.Fatalf("Listen with umask %03o: %v", mask, err)
			}
			if err := listener.Close(); err != nil {
				t.Fatal(err)
			}
			info := requireLockMode(t, socket, 0o600)
			if first == nil {
				first = info
			} else if !os.SameFile(first, info) {
				t.Fatal("listener replaced its sidecar")
			}
		}
	})

	t.Run("creator restores permissions", func(t *testing.T) {
		const socket = "race.sock"
		createRestrictedLock(t, socket)
		restoreLockSeams(t)
		first := requireLockMode(t, socket, 0o400)
		unixLockBudget = 2 * unixLockPollInterval
		now := time.Now()
		unixLockNow = func() time.Time { return now }
		unixLockSleep = func(time.Duration) {
			if err := os.Chmod(socket+unixLockSuffix, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		release, err := lockUnixAddress(socket)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		if info := requireLockMode(t, socket, 0o600); !os.SameFile(first, info) {
			t.Fatal("listener replaced the creator's sidecar")
		}
	})

	t.Run("legacy recovery", func(t *testing.T) {
		const socket = "legacy.sock"
		createRestrictedLock(t, socket)
		restoreLockSeams(t)
		first := requireLockMode(t, socket, 0o400)
		unixLockBudget = unixLockPollInterval
		now := time.Now()
		unixLockNow = func() time.Time { return now }
		unixLockSleep = func(d time.Duration) { now = now.Add(d) }
		if listener, err := Listen("unix://" + socket); err == nil {
			listener.Close()
			t.Fatal("listener opened an inaccessible sidecar")
		} else if !errors.Is(err, ErrLockFailed) || !errors.Is(err, syscall.EACCES) || strings.Contains(err.Error(), socket) {
			t.Fatalf("Listen = %v, want path-free lock permission error", err)
		}
		if info := requireLockMode(t, socket, 0o400); !os.SameFile(first, info) {
			t.Fatal("failed start replaced the inaccessible sidecar")
		}
		if err := os.Chmod(socket+unixLockSuffix, 0o600); err != nil {
			t.Fatal(err)
		}
		listener, err := Listen("unix://" + socket)
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		if info := requireLockMode(t, socket, 0o600); !os.SameFile(first, info) {
			t.Fatal("manual recovery replaced the sidecar")
		}
	})

	t.Run("permission and flock retries share the deadline", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root can open a mode-0400 sidecar for writing")
		}
		const socket = "shared-budget.sock"
		makeStaleSocket(t, socket)
		before, err := os.Lstat(socket)
		if err != nil {
			t.Fatal(err)
		}
		holder, err := os.OpenFile(socket+unixLockSuffix, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer holder.Close()
		if locked, err := tryLockFile(holder); !locked || err != nil {
			t.Fatalf("take fixture lock: locked = %t, error = %v", locked, err)
		}
		if err := holder.Chmod(0o400); err != nil {
			t.Fatal(err)
		}

		restoreLockSeams(t)
		now := time.Now()
		unixLockNow = func() time.Time { return now }
		const remaining = time.Millisecond
		polls := 0
		unixLockSleep = func(d time.Duration) {
			polls++
			switch polls {
			case 1:
				if d != unixLockPollInterval {
					t.Fatalf("permission pause = %v, want %v", d, unixLockPollInterval)
				}
				now = now.Add(unixLockWaitBudget - remaining)
				if err := holder.Chmod(0o600); err != nil {
					t.Fatal(err)
				}
			case 2:
				if d != remaining {
					t.Fatalf("flock pause = %v, want remaining %v", d, remaining)
				}
				now = now.Add(d)
				unlockFile(holder)
			default:
				t.Fatal("wait retried at the deadline")
			}
		}
		l, err := Listen("unix://" + socket)
		if l != nil {
			defer l.Close()
		}
		if polls != 2 || l != nil || !errors.Is(err, ErrLockTimeout) {
			t.Fatalf("Listen after %d pauses = (%v, %v), want timeout after two pauses", polls, l, err)
		}
		if after, err := os.Lstat(socket); err != nil || !os.SameFile(before, after) {
			t.Fatalf("socket file changed: %v", err)
		}
	})

	t.Run("preserve accessible mode", func(t *testing.T) {
		const socket = "existing.sock"
		syscall.Umask(0o022)
		if err := os.WriteFile(socket+unixLockSuffix, nil, 0o640); err != nil {
			t.Fatal(err)
		}
		first := requireLockMode(t, socket, 0o640)
		release, err := lockUnixAddress(socket)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		if info := requireLockMode(t, socket, 0o640); !os.SameFile(first, info) {
			t.Fatal("listener replaced the accessible sidecar")
		}
	})
}

func createRestrictedLock(t *testing.T, socket string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root can open a mode-0400 sidecar for writing")
	}
	syscall.Umask(0o077)
	if err := os.WriteFile(socket+unixLockSuffix, nil, 0o400); err != nil {
		t.Fatal(err)
	}
}

func requireLockMode(t *testing.T, socket string, mode os.FileMode) os.FileInfo {
	t.Helper()
	info, err := os.Stat(socket + unixLockSuffix)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode() != mode {
		t.Fatalf("sidecar mode = %04o, want %04o", info.Mode(), mode)
	}
	return info
}
