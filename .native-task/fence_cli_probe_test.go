// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build linux || darwin

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/images"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/testcp"
)

// Run through a Go overlay so the probe remains outside versioned sources.
func TestImageFenceCLIProbe(t *testing.T) {
	bin := os.Getenv("OWNSAND_IMAGE_FENCE_BIN_DIR")
	if bin == "" {
		bin = buildProcessRuntimeBinaries(t)
	}
	agentBinary := filepath.Join(bin, "ownsand-agent")
	guestBinary := filepath.Join(bin, "ownsand-guest")
	ipsw := os.Getenv("OWNSAND_IMAGE_FENCE_IPSW")
	t.Run("authenticated-removal", func(t *testing.T) {
		cp := testcp.New(t)
		dir := t.TempDir()
		env := processRuntimeEnv(dir, guestBinary)
		enrollProcessRuntimeAgent(t, bin, cp, env)
		st, err := store.OpenForRead(dir)
		if err != nil {
			t.Fatal(err)
		}
		id, err := st.LoadIdentity()
		if err != nil {
			t.Fatal(err)
		}
		stage, finishBuild := holdImageForCLIProbe(t, st, agentBinary, guestBinary, env, ipsw)
		destination := st.Path(store.ImagesDir, images.MacOSDir)
		buildLock := images.LockPath(destination)
		before, err := os.Stat(buildLock)
		if err != nil {
			t.Fatal(err)
		}

		cp.MarkRemoved(id.DeviceID)
		agent := startProcessRuntimeAgent(t, bin, env)
		awaitFenceProbe(t, "the authenticated removal notice", binaryAgentTimeout, func() bool {
			record, err := st.LoadAgent()
			return err == nil && record.Removal.NoticeReceived
		})
		requireWaiting := func() {
			t.Helper()
			record, err := st.LoadAgent()
			if err != nil || !record.Removal.NoticeReceived || record.Removal.CleanupDone || cp.Confirms() != 0 {
				t.Fatalf("removal advanced while the builder was held: state error %v, confirmations %d", err, cp.Confirms())
			}
			if after, err := os.Stat(buildLock); err != nil || !os.SameFile(before, after) {
				t.Fatalf("the active build lock was removed or replaced: %v", err)
			}
			if _, err := os.Stat(stage); err != nil {
				t.Fatalf("the active build staging directory was removed: %v", err)
			}
		}
		requireWaiting()
		build := exec.Command(agentBinary, "image", "build", "--os", "macos")
		build.Env = env
		if out, err := build.CombinedOutput(); cliExit(err) != exitError || !strings.Contains(string(out), store.ErrRemovalPending.Error()) {
			t.Fatalf("the second build was not refused by the notice: %v, %s", err, out)
		}
		if err := agent.cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		if err := agent.wait(t, time.Now().Add(binaryAgentTimeout)); cliExit(err) != exitError || !strings.Contains(agent.logs.String(), store.ErrEnrollmentBusy.Error()) {
			t.Fatalf("the removal process did not cancel its lock wait: %v\n%s", err, agent.logs.String())
		}
		requireWaiting()
		if _, err := st.LoadIdentity(); err != nil {
			t.Fatalf("canceled removal lost the identity: %v", err)
		}
		t.Log("An authenticated removal kept the active build lock and staging. A second CLI build was refused. Cancellation preserved the notice and identity and sent zero confirmations.")

		retry := startProcessRuntimeAgent(t, bin, env)
		awaitFenceProbe(t, "the removal retry", binaryAgentTimeout, func() bool {
			return strings.Contains(retry.logs.String(), "continuing an interrupted device removal")
		})
		requireWaiting()
		finishBuild()
		if err := retry.wait(t, time.Now().Add(binaryAgentTimeout)); cliExit(err) != exitNotEnrolled || cp.Confirms() != 1 {
			t.Fatalf("removal retry: exit %v, confirmations %d\n%s", err, cp.Confirms(), retry.logs.String())
		}
		requireOnlyFenceProbeLock(t, dir)
		t.Log("The retry waited for build release, confirmed once and left only the empty enrollment lock.")
	})
	t.Run("standalone-reset", func(t *testing.T) {
		dir := t.TempDir()
		env := processRuntimeEnv(dir, guestBinary)
		st, err := store.OpenForRead(dir)
		if err != nil {
			t.Fatal(err)
		}
		stage, finishBuild := holdImageForCLIProbe(t, st, agentBinary, guestBinary, env, "")
		reset := exec.Command(agentBinary, "reset", "--yes")
		reset.Env = env
		done := make(chan error, 1)
		logs := &logBuffer{}
		reset.Stdout, reset.Stderr = logs, logs
		if err := reset.Start(); err != nil {
			t.Fatal(err)
		}
		go func() { done <- reset.Wait() }()
		t.Cleanup(func() { _ = reset.Process.Kill() })
		select {
		case err := <-done:
			t.Fatalf("reset returned before the build released its lock: %v\n%s", err, logs.String())
		case <-time.After(250 * time.Millisecond):
		}
		if _, err := os.Stat(stage); err != nil {
			t.Fatalf("reset removed active staging: %v", err)
		}
		finishBuild()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("reset: %v\n%s", err, logs.String())
			}
		case <-time.After(binaryAgentTimeout):
			t.Fatal("reset remained blocked after the build released its lock")
		}
		requireOnlyFenceProbeLock(t, dir)
		t.Log("Standalone reset waited for the builder and then left only the empty enrollment lock.")
	})
}

func cliExit(err error) int {
	if err == nil {
		return 0
	}
	var status *exec.ExitError
	if errors.As(err, &status) {
		return status.ExitCode()
	}
	return -1
}

func awaitFenceProbe(t *testing.T, event string, timeout time.Duration, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !ready() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", event)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func requireOnlyFenceProbeLock(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != store.EnrollmentLockFile {
		t.Fatalf("state after cleanup: %v, %v", entries, err)
	}
	lock, err := entries[0].Info()
	if err != nil || lock.Size() != 0 {
		t.Fatalf("enrollment lock is not empty: %v, %v", lock, err)
	}
}

func holdImageForCLIProbe(t *testing.T, st *store.Store, agentBinary, guestBinary string, env []string, ipsw string) (string, func()) {
	t.Helper()
	dir := st.Path(store.ImagesDir, images.MacOSDir)
	stage := images.BuildDir(dir)
	if ipsw == "" {
		// This test process acts as the admitted builder. The real agent,
		// image CLI and reset run in separate OS processes.
		unlock, err := st.LockImageBuild(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(unlock)
		lock, err := images.TryLock(dir)
		if err != nil {
			t.Fatal(err)
		}
		finish := sync.OnceFunc(func() {
			if err := lock.Release(); err != nil {
				t.Error(err)
			}
			unlock()
		})
		t.Cleanup(finish)
		if err := os.MkdirAll(stage, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(stage, "fixture"), []byte("test build staging"), 0o600); err != nil {
			t.Fatal(err)
		}
		return stage, finish
	}

	if os.Geteuid() != 0 {
		t.Fatal("a real macOS image build requires root")
	}
	build := exec.Command(agentBinary, "image", "build", "--os", "macos", "--ipsw", ipsw, "--guest-binary", guestBinary)
	build.Env = env
	logs := &logBuffer{}
	build.Stdout, build.Stderr = logs, logs
	if err := build.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var buildErr error
	go func() {
		buildErr = build.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		_ = build.Process.Signal(syscall.SIGCONT)
		_ = build.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			_ = build.Process.Kill()
			<-done
		}
	})
	awaitFenceProbe(t, "native macOS disk staging", 3*time.Minute, func() bool {
		select {
		case <-done:
			t.Fatalf("native builder exited before staging its disk: %v\n%s", buildErr, logs.String())
		default:
		}
		_, err := os.Stat(filepath.Join(stage, images.MacOSDisk))
		return err == nil
	})
	if err := build.Process.Signal(syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	t.Log("The signed macOS builder reached native disk staging and was paused before publication.")
	return stage, func() {
		if err := build.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		if err := build.Process.Signal(syscall.SIGCONT); err != nil {
			t.Fatal(err)
		}
		select {
		case <-done:
			if cliExit(buildErr) != exitError || !strings.Contains(logs.String(), "context canceled") {
				t.Fatalf("native build cancellation: %v\n%s", buildErr, logs.String())
			}
		case <-time.After(60 * time.Second):
			t.Fatalf("native build cancellation did not finish\n%s", logs.String())
		}
	}
}
