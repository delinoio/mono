// SPDX-License-Identifier: FSL-1.1-ALv2

package imagebuild

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const (
	driveLetterHolderEnv     = "OWNSAND_TEST_DRIVE_LETTER_MUTEX"
	driveLetterHolderOutcome = "OWNSAND_TEST_DISKPART_OUTCOME"
	firstDrivePairMask       = 1<<('Z'-'A') | 1<<('Y'-'A')
)

func TestDriveLetterMutexHolder(t *testing.T) {
	name := os.Getenv(driveLetterHolderEnv)
	if name == "" {
		t.Skip("subprocess helper")
	}
	scratch, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	outcome := os.Getenv(driveLetterHolderOutcome)
	exitErr := errors.New("injected DiskPart failure")
	var wantErr error
	switch outcome {
	case "failure":
		wantErr = exitErr
	case "canceled":
		wantErr = context.Canceled
	}
	disk := &attachedDisk{number: 42}
	var release [1]byte
	err = disk.partitionWith(ctx, scratch, name, func() (uint32, error) {
		return 0, nil
	}, func(ctx context.Context, tool string, args ...string) (string, error) {
		script, err := os.ReadFile(args[1])
		if err != nil {
			return "", err
		}
		if tool != "diskpart.exe" || args[0] != "/s" || !bytes.Equal(script, []byte(diskpartScript(42, "Z", "Y"))) {
			return "", errors.New("unexpected first partition command")
		}
		if err := os.WriteFile("ready", nil, 0o600); err != nil {
			return "", err
		}
		if _, err := io.ReadFull(os.Stdin, release[:]); err != nil {
			return "", err
		}
		switch outcome {
		case "failure":
			return "injected tool output", exitErr
		case "canceled":
			cancel()
			return "injected tool output", ctx.Err()
		}
		// Simulate DiskPart making the selected letters visible before exit.
		return "", os.WriteFile("assigned", nil, 0o600)
	}, func(delay time.Duration) {
		if delay != 15*time.Second {
			t.Fatalf("DiskPart shutdown delay = %v, want 15s", delay)
		}
		// Keep the shutdown interval open until the parent tests a canceled waiter.
		if err := os.WriteFile("shutdown", nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(os.Stdin, release[:]); err != nil {
			t.Fatal(err)
		}
	})
	if wantErr != nil {
		if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "injected tool output") {
			t.Fatalf("partition error = %v, want %v and the tool output", err, wantErr)
		}
	} else if err != nil {
		t.Fatal(err)
	}
	// Stay alive until the waiter finishes, so process exit cannot release
	// the mutex on behalf of a partition call that failed to release it.
	if _, err := io.ReadFull(os.Stdin, release[:]); err != nil {
		t.Fatal(err)
	}
}

func TestPartitionWaitsForDriveLetterAssignments(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outcome string
		system  string
		windows string
	}{
		{name: "fresh mask", system: "X", windows: "W"},
		{name: "holder failure", outcome: "failure", system: "Z", windows: "Y"},
		{name: "holder cancellation", outcome: "canceled", system: "Z", windows: "Y"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			holderScratch, waiterScratch := t.TempDir(), t.TempDir()
			name := fmt.Sprintf(`Local\Delino.OwnSand.Test.DriveLetters.%d.%d`, os.Getpid(), time.Now().UnixNano())
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDriveLetterMutexHolder$", "-test.v")
			cmd.Dir = holderScratch
			cmd.Env = append(os.Environ(), driveLetterHolderEnv+"="+name,
				driveLetterHolderOutcome+"="+tc.outcome)
			var output bytes.Buffer
			cmd.Stdout, cmd.Stderr = &output, &output
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			holderDone := make(chan struct{})
			var holderErr error
			go func() {
				holderErr = cmd.Wait()
				close(holderDone)
			}()
			defer func() {
				cancel()
				<-holderDone
			}()
			waitForHolder := func(marker string) {
				t.Helper()
				for {
					if _, err := os.Stat(filepath.Join(holderScratch, marker)); err == nil {
						return
					} else if !errors.Is(err, os.ErrNotExist) {
						t.Fatal(err)
					}
					select {
					case <-holderDone:
						t.Fatalf("holder exited before %s: %v\n%s", marker, holderErr, &output)
					case <-ctx.Done():
						t.Fatalf("holder did not reach %s", marker)
					case <-time.After(10 * time.Millisecond):
					}
				}
			}
			waitForHolder("ready")

			waiter := &attachedDisk{number: 43}
			waiterDone := make(chan struct{})
			waiterMayReadMask := make(chan struct{})
			var waiterErr error
			go func() {
				defer close(waiterDone)
				waiterErr = waiter.partitionWith(ctx, waiterScratch, name, func() (uint32, error) {
					select {
					case <-waiterMayReadMask:
					default:
						return 0, errors.New("waiter read the drive mask before the holder finished")
					}
					_, err := os.Stat(filepath.Join(holderScratch, "assigned"))
					if errors.Is(err, os.ErrNotExist) {
						return 0, nil
					}
					return firstDrivePairMask, err
				}, func(_ context.Context, tool string, args ...string) (string, error) {
					script, err := os.ReadFile(args[1])
					if err == nil && (tool != "diskpart.exe" || args[0] != "/s" ||
						!bytes.Equal(script, []byte(diskpartScript(43, tc.system, tc.windows)))) {
						err = fmt.Errorf("unexpected second partition script: %s", script)
					}
					return "", err
				}, func(time.Duration) {})
			}()
			defer func() {
				cancel()
				<-waiterDone
			}()
			select {
			case <-waiterDone:
				t.Fatalf("waiter returned while the holder was still in DiskPart: %v", waiterErr)
			case <-time.After(150 * time.Millisecond):
			}
			if _, err := stdin.Write([]byte{1}); err != nil {
				t.Fatal(err)
			}
			waitForHolder("shutdown")
			waitCtx, stop := context.WithTimeout(ctx, 150*time.Millisecond)
			canceledWaiter := &attachedDisk{number: 44}
			err = canceledWaiter.partitionWith(waitCtx, waiterScratch, name, func() (uint32, error) {
				t.Error("canceled waiter read the drive mask during shutdown")
				return 0, nil
			}, func(context.Context, string, ...string) (string, error) {
				t.Error("canceled waiter started DiskPart during shutdown")
				return "", nil
			}, func(time.Duration) {
				t.Error("canceled waiter started a shutdown wait")
			})
			stop()
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("canceled waiter error = %v, want deadline exceeded", err)
			}
			close(waiterMayReadMask)
			if _, err := stdin.Write([]byte{1}); err != nil {
				t.Fatal(err)
			}
			select {
			case <-waiterDone:
				if waiterErr != nil {
					t.Fatal(waiterErr)
				}
			case <-holderDone:
				t.Fatalf("holder exited before the waiter completed: %v\n%s", holderErr, &output)
			}
			if waiter.System != tc.system+":" || waiter.Windows != tc.windows+":" {
				t.Fatalf("waiter selected %s and %s", waiter.System, waiter.Windows)
			}
			if _, err := stdin.Write([]byte{1}); err != nil {
				t.Fatal(err)
			}
			<-holderDone
			if holderErr != nil {
				t.Fatalf("holder failed: %v\n%s", holderErr, &output)
			}
		})
	}
}

func TestPartitionRequiresTwoFreeDriveLetters(t *testing.T) {
	maskErr := errors.New("injected drive mask error")
	for _, tc := range []struct {
		name        string
		mask        uint32
		maskErr     error
		wantErr     error
		wantMessage string
	}{
		{name: "one free letter", mask: ^uint32(1 << ('Z' - 'A')), wantMessage: "imagebuild: no free drive letters for the image partitions"},
		{name: "mask read failure", maskErr: maskErr, wantErr: maskErr},
		{name: "script write failure", wantErr: os.ErrNotExist},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := fmt.Sprintf(`Local\Delino.OwnSand.Test.DriveLetters.%d.%d`, os.Getpid(), time.Now().UnixNano())
			// Require a different thread for the waiter. Windows mutexes allow
			// the owning thread to acquire the mutex again without blocking.
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			// The missing scratch directory forces a script write error if mask validation succeeds.
			scratch := filepath.Join(t.TempDir(), "missing")
			disk := &attachedDisk{number: 42}
			err := disk.partitionWith(context.Background(), scratch, name, func() (uint32, error) {
				return tc.mask, tc.maskErr
			}, func(context.Context, string, ...string) (string, error) {
				t.Fatal("started DiskPart after a pre-launch failure")
				return "", nil
			}, func(time.Duration) {
				t.Fatal("waited for DiskPart shutdown after a pre-launch failure")
			})
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("partition error = %v, want %v", err, tc.wantErr)
			}
			if tc.wantMessage != "" && (err == nil || err.Error() != tc.wantMessage) {
				t.Fatalf("partition error = %v, want %q", err, tc.wantMessage)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			waiter := make(chan error, 1)
			go func() { waiter <- withDriveLetterMutex(ctx, name, func() error { return nil }) }()
			if err := <-waiter; err != nil {
				t.Fatalf("acquire mutex after pre-launch failure: %v", err)
			}
		})
	}
}
