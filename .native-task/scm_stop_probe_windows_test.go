// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/config"
)

// TestLauncherSCMStop runs only when the coordinator adds this file to a
// disposable Windows checkout by overlay. It owns its service and payload.
func TestLauncherSCMStop(t *testing.T) {
	args := flag.Args()
	if len(args) == 3 && args[0] == "ownsand-scm-probe" {
		runSCMProbeService(t, args[1], args[2])
		return
	}
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Fatal("run the disposable SCM probe from an elevated shell")
	}
	manager, err := mgr.Connect()
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Disconnect()

	name := "OwnSandStopProbe-" + rand.Text()
	dir := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	service, err := manager.CreateService(name, exe, mgr.Config{StartType: mgr.StartManual},
		"-test.run=^TestLauncherSCMStop$", "--", "ownsand-scm-probe", name, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("created disposable service %s", name)
	defer service.Close()
	defer func() {
		// The payload also has a fixed lifetime, so teardown can wait for
		// the service even if the launcher regresses to the original hang.
		if status, err := service.Query(); err == nil && status.State != svc.Stopped {
			if status.State == svc.Running {
				_, _ = service.Control(svc.Stop)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
			defer cancel()
			if _, err := waitForSCMProbeState(ctx, service, svc.Stopped); err != nil {
				t.Errorf("stop disposable service %s: %v", name, err)
			}
		}
		if err := service.Delete(); err != nil {
			t.Errorf("delete disposable service %s: %v", name, err)
		}
	}()

	if err := service.Start(); err != nil {
		t.Fatal(err)
	}
	startCtx, cancelStart := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancelStart()
	if _, err := waitForSCMProbeState(startCtx, service, svc.Running); err != nil {
		t.Fatal(err)
	}
	if err := waitForPayloadFixture(startCtx, filepath.Join(dir, "ready")); err != nil {
		t.Fatalf("wait for the disposable payload: %v", err)
	}
	pidPath := filepath.Join(dir, "pid")
	if err := waitForPayloadFixture(startCtx, pidPath); err != nil {
		t.Fatalf("wait for the disposable payload ID: %v", err)
	}
	pidText, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.ParseUint(string(pidText), 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	process, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(process)
	if state, err := windows.WaitForSingleObject(process, 0); err != nil || state != uint32(windows.WAIT_TIMEOUT) {
		t.Fatalf("the disposable payload must be alive before Stop: state %#x, error %v", state, err)
	}

	started := time.Now()
	if _, err := service.Control(svc.Stop); err != nil {
		t.Fatal(err)
	}
	stopCtx, cancelStop := context.WithDeadline(t.Context(), started.Add(8*time.Second))
	defer cancelStop()
	stopped, err := waitForSCMProbeState(stopCtx, service, svc.Stopped)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.Win32ExitCode != 0 || stopped.ServiceSpecificExitCode != 0 {
		t.Fatalf("SCM stop exit codes: %+v", stopped)
	}
	if state, err := windows.WaitForSingleObject(process, 5000); err != nil || state != windows.WAIT_OBJECT_0 {
		t.Fatalf("job close did not end the disposable payload: wait state %#x, error %v", state, err)
	}
	logBytes, err := os.ReadFile(filepath.Join(dir, "agent.log"))
	if err != nil {
		t.Fatal(err)
	}
	log := string(logBytes)
	for _, want := range []string{"signal the service payload to stop", "terminate the service payload job", "agent exits under the Windows service", `"stop_requested":true`, `"reported_exit_code":0`} {
		if !strings.Contains(log, want) {
			t.Fatalf("missing log value %q in %s", want, log)
		}
	}
	t.Logf("SCM reached Stopped with exit code zero in %s. The job close ended the payload.", time.Since(started).Round(time.Millisecond))
	t.Log(log)
}

func runSCMProbeService(t *testing.T, name, dir string) {
	dispatch := func(_ string, h svc.Handler) error { return svc.Run(name, h) }
	run := func(ctx context.Context, cfg config.Config, logFile *os.File) (int, bool) {
		ops := windowsPayloadProcessOps()
		ops.signal = func(uint32) error { return windows.ERROR_INVALID_HANDLE }
		ops.freeConsole = func() {}
		ops.terminate = func(windows.Handle, uint32) error { return windows.ERROR_ACCESS_DENIED }
		ops.start = func(job windows.Handle, exe string, args []string, stderr *os.File) (windows.ProcessInformation, error) {
			child, err := startPayloadProcess(job, exe, args, stderr)
			if err == nil {
				if err := publishSCMProbePID(dir, child.ProcessId); err != nil {
					newLogger(cfg).Error("write SCM probe payload ID", "error", err)
				}
			}
			return child, err
		}
		_, err := runPayloadProcessWith(ctx, os.Args[0], stubbornPayloadArgs(filepath.Join(dir, "ready")), logFile, ops)
		newLogger(cfg).Error("SCM probe payload stop", "error", err)
		if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return exitOK, false
		}
		return exitError, false
	}
	reportStartupError := func(message string) error {
		return os.WriteFile(filepath.Join(dir, "startup-error"), []byte(message), 0o600)
	}
	code := runUnderServiceManager(config.Config{StateDir: dir}, nil, dispatch, reportStartupError, run)
	if code != exitError {
		t.Fatalf("probe launcher exit = %d, want %d", code, exitError)
	}
}

func publishSCMProbePID(dir string, pid uint32) error {
	pending := filepath.Join(dir, "pid.pending")
	if err := os.WriteFile(pending, []byte(strconv.FormatUint(uint64(pid), 10)), 0o600); err != nil {
		return err
	}
	return os.Rename(pending, filepath.Join(dir, "pid"))
}

func waitForSCMProbeState(ctx context.Context, service *mgr.Service, want svc.State) (svc.Status, error) {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		status, err := service.Query()
		if err != nil {
			return status, err
		}
		if err := ctx.Err(); err != nil {
			return status, fmt.Errorf("SCM state = %v, want %v: %w", status.State, want, err)
		}
		if status.State == want {
			return status, nil
		}
		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}
}
