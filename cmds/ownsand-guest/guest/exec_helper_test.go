// SPDX-License-Identifier: FSL-1.1-ALv2

package guest

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

const (
	helperModeEnv     = "OWNSAND_GUEST_TEST_HELPER_MODE"
	helperPidFileEnv  = "OWNSAND_GUEST_TEST_HELPER_PID_FILE"
	helperStopFileEnv = "OWNSAND_GUEST_TEST_HELPER_STOP_FILE"
	helperLifetimeEnv = "OWNSAND_GUEST_TEST_HELPER_LIFETIME"
	helperLifetime    = 60 * time.Second
)

// TestHelperProcess runs in a separate test process. The "exit" mode exits
// immediately. The "sleep" mode waits for a stop request or its deadline.
// The "spawn" mode starts a "sleep" child that inherits both output pipes
// and exits after recording the child's PID for liveness checks.
func TestHelperProcess(t *testing.T) {
	t.Parallel()
	mode := os.Getenv(helperModeEnv)
	if mode == "" {
		return
	}
	pidFile := os.Getenv(helperPidFileEnv)
	switch mode {
	case "exit":
		os.Exit(0)
	case "sleep":
		if pidFile != "" {
			_ = os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0o600)
		}
		lifetime := helperLifetime
		if value := os.Getenv(helperLifetimeEnv); value != "" {
			var err error
			lifetime, err = time.ParseDuration(value)
			if err != nil || lifetime <= 0 {
				os.Exit(5)
			}
		}
		if err := waitForExecHelperStop(os.Getenv(helperStopFileEnv), lifetime); err != nil {
			os.Exit(6)
		}
		os.Exit(0)
	case "spawn":
		child := exec.Command(helperCommand()[0], helperCommand()[1:]...)
		child.Env = append(os.Environ(), helperModeEnv+"=sleep", helperPidFileEnv+"=")
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		if err := os.WriteFile(pidFile, []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			os.Exit(3)
		}
		os.Exit(0)
	}
	os.Exit(4)
}

func helperCommand() []string {
	return []string{os.Args[0], "-test.run=^TestHelperProcess$"}
}

func waitForExecHelperStop(stopFile string, lifetime time.Duration) error {
	if stopFile == "" {
		time.Sleep(lifetime)
		return nil
	}
	deadline := time.NewTimer(lifetime)
	defer deadline.Stop()
	poll := time.NewTicker(20 * time.Millisecond)
	defer poll.Stop()
	for {
		select {
		case <-deadline.C:
			return nil
		case <-poll.C:
			if _, err := os.Stat(stopFile); err == nil {
				return os.Mkdir(stopFile+".ack", 0o700)
			}
		}
	}
}

// execSurvivorFiles gives the descendant a private stop file. Cleanup keeps
// the directory available until the descendant acknowledges the request or
// the wait expires. The descendant's own deadline bounds a failed stop.
func execSurvivorFiles(t *testing.T) (pidFile, stopFile string) {
	t.Helper()
	dir := t.TempDir()
	stopFile = filepath.Join(dir, "stop")
	t.Cleanup(func() {
		if err := os.WriteFile(stopFile, nil, 0o600); err != nil {
			t.Errorf("request fixture stop: %v", err)
			return
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(stopFile + ".ack"); err == nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Error("fixture did not acknowledge the stop request")
	})
	return filepath.Join(dir, "descendant.pid"), stopFile
}

func TestExecHelperExitsAtDeadline(t *testing.T) {
	t.Parallel()
	const lifetime = 100 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, helperCommand()[0], helperCommand()[1:]...)
	cmd.Env = append(os.Environ(),
		helperModeEnv+"=sleep",
		helperPidFileEnv+"=",
		helperStopFileEnv+"="+filepath.Join(t.TempDir(), "stop"),
		helperLifetimeEnv+"="+lifetime.String(),
	)
	start := time.Now()
	if err := cmd.Run(); err != nil {
		t.Fatalf("helper did not exit successfully at its deadline: %v", err)
	}
	if elapsed := time.Since(start); elapsed < lifetime {
		t.Fatalf("helper exited after %v, before its %v deadline", elapsed, lifetime)
	}
}
