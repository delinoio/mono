// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/config"
)

func TestLauncherStopsPayloadProcess(t *testing.T) {
	const role = "OWNSAND_TEST_LAUNCHER_ROLE"
	const readyPath = "OWNSAND_TEST_PAYLOAD_READY"

	switch os.Getenv(role) {
	case "payload":
		// Console-launched programs can inherit a setting that ignores Ctrl+C.
		if ok, _, err := setConsoleCtrlHandler.Call(0, 1); ok == 0 {
			t.Fatalf("ignore Ctrl+C in the payload: %v", err)
		}
		ctx, stop := signal.NotifyContext(t.Context(), os.Interrupt)
		defer stop()
		if err := os.WriteFile(os.Getenv(readyPath), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(30 * time.Second):
			t.Fatal("the payload did not receive the console interrupt")
		}
	case "launcher":
		t.Setenv(role, "payload")
		ctx, stop := context.WithCancel(t.Context())
		defer stop()
		go func() {
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if _, err := os.Stat(os.Getenv(readyPath)); err == nil {
						stop()
						return
					}
				}
			}
		}()
		code, err := runPayloadProcess(ctx, os.Args[0], []string{"-test.run=^TestLauncherStopsPayloadProcess$"}, os.Stderr)
		if err != nil || code != exitOK {
			t.Fatalf("stop the payload: code %d, error %v", code, err)
		}
	default:
		t.Setenv(role, "launcher")
		t.Setenv(readyPath, filepath.Join(t.TempDir(), "ready"))
		ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		defer cancel()
		child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLauncherStopsPayloadProcess$")
		child.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS}
		if output, err := child.CombinedOutput(); err != nil {
			t.Fatalf("launch and stop a payload: %v\n%s", err, output)
		}
	}
}

const stubbornPayloadFixture = "ownsand-test-stubborn-payload"

func stubbornPayloadArgs(ready string) []string {
	return []string{"-test.run=^TestServicePayloadFixture$", "--", stubbornPayloadFixture, ready}
}

func TestServicePayloadFixture(t *testing.T) {
	args := flag.Args()
	if len(args) != 2 || args[0] != stubbornPayloadFixture {
		t.Skip("run as a child of the Windows launcher tests")
	}
	signalCtx, stop := signal.NotifyContext(t.Context(), os.Interrupt)
	defer stop()
	if err := os.WriteFile(args[1], nil, 0o600); err != nil {
		t.Fatal(err)
	}
	lifetime := time.NewTimer(45 * time.Second)
	defer lifetime.Stop()
	select {
	case <-lifetime.C:
		return
	case <-signalCtx.Done():
		if err := os.WriteFile(args[1]+".interrupted", nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	<-lifetime.C
}

func waitForPayloadFixture(ctx context.Context, ready string) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		_, err := os.Stat(ready)
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func TestLauncherForcedStopWaitsForPayloadExit(t *testing.T) {
	args := flag.Args()
	if len(args) != 2 || args[0] != "ownsand-test-forced-stop" {
		// The launcher attaches to the child's console. Keep the parent test
		// process attached to its own console.
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLauncherForcedStopWaitsForPayloadExit$", "--",
			"ownsand-test-forced-stop", filepath.Join(t.TempDir(), "ready"))
		child.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS}
		if output, err := child.CombinedOutput(); err != nil {
			t.Fatalf("force the payload to stop: %v\n%s", err, output)
		}
		return
	}

	ready := args[1]
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	readyResult := make(chan error, 1)
	go func() {
		readyCtx, stop := context.WithTimeout(ctx, 8*time.Second)
		defer stop()
		readyResult <- waitForPayloadFixture(readyCtx, ready)
		cancel()
	}()
	ops := windowsPayloadProcessOps()
	ops.stopTimeout = 3 * time.Second
	var collectedCode uint32
	var collectedErr error
	ops.wait = func(process windows.Handle, cancel <-chan struct{}) (uint32, error) {
		collectedCode, collectedErr = waitPayloadProcess(process, cancel)
		return collectedCode, collectedErr
	}
	code, err := runPayloadProcessWith(ctx, os.Args[0], stubbornPayloadArgs(ready), os.Stderr, ops)
	if err := <-readyResult; err != nil {
		t.Fatalf("wait for the payload to start: %v", err)
	}
	if code != 0 || err == nil || err.Error() != "the service payload did not stop within 3s" {
		t.Fatalf("force the payload to stop: code %d, error %v", code, err)
	}
	if _, err := os.Stat(ready + ".interrupted"); err != nil {
		t.Fatalf("the payload did not record the console interrupt: %v", err)
	}
	if collectedCode != 1 || collectedErr != nil {
		t.Fatalf("collected process exit: code %d, error %v", collectedCode, collectedErr)
	}
}

func TestHandlerCompletesAfterPayloadTerminationFails(t *testing.T) {
	for _, tc := range []struct {
		name      string
		command   svc.Cmd
		signalErr error
		closeErr  error
		want      string
	}{
		{"stop after signal failure", svc.Stop, windows.ERROR_INVALID_HANDLE, nil, "signal the service payload to stop"},
		{"shutdown after grace expires", svc.Shutdown, nil, windows.ERROR_GEN_FAILURE, "the service payload did not stop within"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ready := filepath.Join(t.TempDir(), "ready")
			ops := windowsPayloadProcessOps()
			ops.signal = func(uint32) error { return tc.signalErr }
			ops.freeConsole = func() {}
			ops.stopTimeout = 0
			var job, process, observedProcess windows.Handle
			var jobCloses, processCloses, terminations int
			waiterStarted := make(chan struct{})
			waiterReturned := make(chan struct{})
			ops.start = func(handle windows.Handle, exe string, args []string, stderr *os.File) (windows.ProcessInformation, error) {
				// The extra job handle keeps the payload alive even after the
				// launcher closes its handle. Cleanup then triggers kill-on-close.
				duplicate := func(handle windows.Handle) (windows.Handle, error) {
					current := windows.CurrentProcess()
					var copy windows.Handle
					err := windows.DuplicateHandle(current, handle, current, &copy, 0, false, windows.DUPLICATE_SAME_ACCESS)
					if err == nil {
						t.Cleanup(func() { windows.CloseHandle(copy) })
					}
					return copy, err
				}
				job = handle
				if _, err := duplicate(job); err != nil {
					return windows.ProcessInformation{}, err
				}
				child, err := startPayloadProcess(job, exe, args, stderr)
				if err != nil {
					return child, err
				}
				process = child.Process
				observedProcess, err = duplicate(process)
				if err != nil {
					windows.CloseHandle(child.Thread)
					windows.CloseHandle(child.Process)
				}
				return child, err
			}
			ops.wait = func(process windows.Handle, cancel <-chan struct{}) (uint32, error) {
				close(waiterStarted)
				defer close(waiterReturned)
				return waitPayloadProcess(process, cancel)
			}
			ops.terminate = func(windows.Handle, uint32) error {
				terminations++
				return windows.ERROR_ACCESS_DENIED
			}
			ops.closeHandle = func(handle windows.Handle) error {
				if handle == process {
					processCloses++
					select {
					case <-waiterReturned:
					default:
						t.Error("the launcher closed the process handle before the waiter returned")
					}
				}
				err := windows.CloseHandle(handle)
				if handle == job {
					jobCloses++
					if err == nil {
						return tc.closeErr
					}
				}
				return err
			}

			launchResult := make(chan error, 1)
			h := &handler{run: func(ctx context.Context, _ config.Config) (int, bool) {
				ctx, cancel := context.WithCancel(ctx)
				defer cancel()
				stop := context.AfterFunc(t.Context(), cancel)
				defer stop()
				_, err := runPayloadProcessWith(ctx, os.Args[0], stubbornPayloadArgs(ready), os.Stderr, ops)
				launchResult <- err
				return exitError, false
			}}
			requests := make(chan svc.ChangeRequest, 1)
			status := make(chan svc.Status, 3)
			type outcome struct {
				specific bool
				exit     uint32
			}
			result := make(chan outcome, 1)
			go func() {
				specific, exit := h.Execute(nil, requests, status)
				result <- outcome{specific, exit}
			}()
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			wantStatus := func(want svc.State) {
				t.Helper()
				select {
				case got := <-status:
					if got.State != want {
						t.Fatalf("service status = %v, want %v", got.State, want)
					}
				case <-ctx.Done():
					t.Fatalf("service did not report %v: %v", want, ctx.Err())
				}
			}
			wantStatus(svc.StartPending)
			wantStatus(svc.Running)
			if err := waitForPayloadFixture(ctx, ready); err != nil {
				t.Fatalf("wait for the payload to start: %v", err)
			}
			select {
			case <-waiterStarted:
			case <-ctx.Done():
				t.Fatalf("payload waiter did not start: %v", ctx.Err())
			}
			requests <- svc.ChangeRequest{Cmd: tc.command}
			wantStatus(svc.StopPending)
			select {
			case got := <-result:
				if got != (outcome{}) || h.code != exitError {
					t.Fatalf("service outcome = %+v, launcher code = %d", got, h.code)
				}
			case <-ctx.Done():
				t.Fatalf("service stayed in StopPending: %v", ctx.Err())
			}
			err := <-launchResult
			if !errors.Is(err, windows.ERROR_ACCESS_DENIED) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("launcher error = %v, want termination error and %q", err, tc.want)
			}
			if tc.signalErr != nil && !errors.Is(err, tc.signalErr) {
				t.Errorf("launcher error = %v, want cause %v", err, tc.signalErr)
			}
			if tc.closeErr != nil && (!errors.Is(err, tc.closeErr) || !strings.Contains(err.Error(), "close the service payload job")) {
				t.Errorf("launcher error = %v, want job close error %v", err, tc.closeErr)
			}
			if terminations != 1 || jobCloses != 1 || processCloses != 1 {
				t.Errorf("termination calls = %d, job handle closes = %d, process handle closes = %d, want one each",
					terminations, jobCloses, processCloses)
			}
			if state, err := windows.WaitForSingleObject(observedProcess, 0); err != nil || state != uint32(windows.WAIT_TIMEOUT) {
				t.Fatalf("the launcher did not return while the payload was alive: wait status = %#x, error = %v", state, err)
			}
		})
	}
}
