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
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	driveLetterHolderEnv     = "OWNSAND_TEST_DRIVE_LETTER_MUTEX"
	driveLetterHolderFailure = "OWNSAND_TEST_DISKPART_FAILURE"
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
	exitErr := errors.New("injected DiskPart failure")
	disk := &attachedDisk{number: 42}
	var release [1]byte
	err = disk.partitionWith(context.Background(), scratch, name, func() (uint32, error) {
		return 0, nil
	}, func(_ context.Context, tool string, args ...string) (string, error) {
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
		if os.Getenv(driveLetterHolderFailure) == "true" {
			return "injected tool output", exitErr
		}
		// Simulate DiskPart making the selected letters visible before exit.
		return "", os.WriteFile("assigned", nil, 0o600)
	})
	if os.Getenv(driveLetterHolderFailure) == "true" {
		if !errors.Is(err, exitErr) || !strings.Contains(err.Error(), "injected tool output") {
			t.Fatalf("partition error = %v, want injected DiskPart failure and output", err)
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
		name       string
		failHolder bool
		system     string
		windows    string
	}{
		{name: "fresh mask", system: "X", windows: "W"},
		{name: "holder failure", failHolder: true, system: "Z", windows: "Y"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			holderScratch, waiterScratch := t.TempDir(), t.TempDir()
			name := fmt.Sprintf(`Local\Delino.OwnSand.Test.DriveLetters.%d.%d`, os.Getpid(), time.Now().UnixNano())
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDriveLetterMutexHolder$", "-test.v")
			cmd.Dir = holderScratch
			cmd.Env = append(os.Environ(), driveLetterHolderEnv+"="+name,
				driveLetterHolderFailure+"="+strconv.FormatBool(tc.failHolder))
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
			ready := filepath.Join(holderScratch, "ready")
			for {
				if _, err := os.Stat(ready); err == nil {
					break
				} else if !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
				select {
				case <-holderDone:
					t.Fatalf("holder exited before assigning drive letters: %v\n%s", holderErr, &output)
				case <-ctx.Done():
					t.Fatal("holder did not reach DiskPart")
				case <-time.After(10 * time.Millisecond):
				}
			}

			waiter := &attachedDisk{number: 43}
			waitCtx, stop := context.WithTimeout(ctx, 150*time.Millisecond)
			err = waiter.partitionWith(waitCtx, waiterScratch, name, func() (uint32, error) {
				t.Error("canceled waiter read the drive mask")
				return 0, nil
			}, func(context.Context, string, ...string) (string, error) {
				t.Error("canceled waiter started DiskPart")
				return "", nil
			})
			stop()
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("canceled waiter error = %v, want deadline exceeded", err)
			}

			waiterDone := make(chan struct{})
			var waiterErr error
			go func() {
				defer close(waiterDone)
				waiterErr = waiter.partitionWith(ctx, waiterScratch, name, func() (uint32, error) {
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
				})
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
	name := fmt.Sprintf(`Local\Delino.OwnSand.Test.DriveLetters.%d.%d`, os.Getpid(), time.Now().UnixNano())
	disk := &attachedDisk{number: 42}
	err := disk.partitionWith(context.Background(), t.TempDir(), name, func() (uint32, error) {
		return ^uint32(1 << ('Z' - 'A')), nil
	}, func(context.Context, string, ...string) (string, error) {
		t.Fatal("started DiskPart with only one free letter")
		return "", nil
	})
	if err == nil || err.Error() != "imagebuild: no free drive letters for the image partitions" {
		t.Fatalf("partition error = %v, want insufficient drive letters", err)
	}
}
