// SPDX-License-Identifier: FSL-1.1-ALv2

package hcsrt

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// failOpenSystem makes openSystemFunc return err for the test.
func failOpenSystem(t *testing.T, err error) {
	t.Helper()
	orig := openSystemFunc
	openSystemFunc = func(string) (*system, error) { return nil, err }
	t.Cleanup(func() { openSystemFunc = orig })
}

// newDeleteTestRuntime returns a runtime without a Hyper-V VM and a VM
// directory with a file, and skips the test where the HCS DLL does not load,
// because Delete then skips the orphan stop.
func newDeleteTestRuntime(t *testing.T, sandboxID string) (*Runtime, string) {
	t.Helper()
	if err := hcsLoad(); err != nil {
		t.Skipf("the HCS is not available: %v", err)
	}
	r := &Runtime{
		opts:   Options{VMsDir: t.TempDir()},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		vms:    map[string]*vm{},
	}
	dir := r.vmDir(sandboxID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "disk.vhd"), []byte("disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	return r, dir
}

func TestStopOrphanOpenOutcome(t *testing.T) {
	transient := &hcsError{op: "HcsOpenComputeSystem", hr: 0x80070005}
	for _, tc := range []struct {
		name    string
		openErr error
		wantErr error
	}{
		{"not found", &hcsError{op: "HcsOpenComputeSystem", hr: hrSystemNotFound}, nil},
		{"already stopped", &hcsError{op: "HcsOpenComputeSystem", hr: hrSystemAlreadyStopped}, nil},
		{"transient open failure", transient, transient},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failOpenSystem(t, tc.openErr)
			r := &Runtime{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
			err := r.stopOrphan("sb")
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("stopOrphan = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestDeleteKeepsVMDirWhenOrphanStopOutcomeUnknown(t *testing.T) {
	r, dir := newDeleteTestRuntime(t, "sb")
	transient := &hcsError{op: "HcsOpenComputeSystem", hr: 0x80070005}
	failOpenSystem(t, transient)
	err := r.Delete(t.Context(), "sb")
	if !errors.Is(err, transient) {
		t.Fatalf("Delete = %v, want the open error", err)
	}
	if _, serr := os.Stat(filepath.Join(dir, "disk.vhd")); serr != nil {
		t.Errorf("the VM directory content was removed: %v", serr)
	}
}

func TestDeleteRemovesVMDirWhenSystemAbsent(t *testing.T) {
	for _, hr := range []uint32{hrSystemNotFound, hrSystemAlreadyStopped} {
		r, dir := newDeleteTestRuntime(t, "sb")
		failOpenSystem(t, &hcsError{op: "HcsOpenComputeSystem", hr: hr})
		if err := r.Delete(t.Context(), "sb"); err != nil {
			t.Fatalf("Delete with hr %#x = %v", hr, err)
		}
		if _, serr := os.Stat(dir); !os.IsNotExist(serr) {
			t.Errorf("hr %#x: the VM directory still exists: %v", hr, serr)
		}
	}
}

// TestCopyFileEndsWithItsContext checks that copyFile copies the whole file on
// a live context, refuses a context that already ended, and removes the
// partial destination of a copy that a context cancellation interrupted.
func TestCopyFileEndsWithItsContext(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, bytes.Repeat([]byte{7}, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "dst")
	if err := copyFile(context.Background(), src, dst); err != nil {
		t.Fatalf("copy on a live context = %v", err)
	}
	if got, err := os.ReadFile(dst); err != nil || len(got) != 1<<20 {
		t.Fatalf("copy = %d bytes, %v", len(got), err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := copyFile(canceled, src, dst); !errors.Is(err, context.Canceled) {
		t.Fatalf("copy on a canceled context = %v, want context.Canceled", err)
	}
	// A copy that is canceled while it runs. The file is large so that the
	// copy outlasts the cancellation; the test skips when the file system
	// copies it before the cancellation arrives (for example a ReFS clone).
	big := filepath.Join(dir, "big")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(2 << 30); err != nil {
		f.Close()
		t.Skipf("the large source file could not be created: %v", err)
	}
	f.Close()
	bigDst := filepath.Join(dir, "big-dst")
	ctx, cancelMid := context.WithCancel(context.Background())
	time.AfterFunc(time.Millisecond, cancelMid)
	err = copyFile(ctx, big, bigDst)
	if err == nil {
		t.Skip("the copy finished before the cancellation arrived")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled copy = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(bigDst); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial copy remains after a canceled copy: %v", err)
	}
}

// recordOpenSystem makes openSystemFunc return err and record whether the
// marker file of the VM directory still exists when the system is opened.
func recordOpenSystem(t *testing.T, marker string, err error) *[]bool {
	t.Helper()
	var markerPresent []bool
	orig := openSystemFunc
	openSystemFunc = func(string) (*system, error) {
		_, serr := os.Stat(marker)
		markerPresent = append(markerPresent, serr == nil)
		return nil, err
	}
	t.Cleanup(func() { openSystemFunc = orig })
	return &markerPresent
}

func TestResetVMDirStopsLeftoverSystemBeforeRemovingDir(t *testing.T) {
	for _, tc := range []struct {
		name    string
		openErr error
	}{
		{"system absent", &hcsError{op: "HcsOpenComputeSystem", hr: hrSystemNotFound}},
		{"unknown stop outcome", &hcsError{op: "HcsOpenComputeSystem", hr: 0x80070005}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &Runtime{
				opts:   Options{VMsDir: t.TempDir()},
				logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
				vms:    map[string]*vm{},
			}
			dir := r.vmDir("sb")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(dir, "disk.vhd")
			if err := os.WriteFile(marker, []byte("disk"), 0o600); err != nil {
				t.Fatal(err)
			}
			seen := recordOpenSystem(t, marker, tc.openErr)
			if err := r.resetVMDir("sb", dir); err != nil {
				t.Fatalf("resetVMDir = %v, want the create to go on", err)
			}
			if len(*seen) != 1 || !(*seen)[0] {
				t.Errorf("the leftover system must be opened once before the directory is removed, got %v", *seen)
			}
			if _, serr := os.Stat(marker); !os.IsNotExist(serr) {
				t.Errorf("the old VM directory content was not removed: %v", serr)
			}
			if st, serr := os.Stat(dir); serr != nil || !st.IsDir() {
				t.Errorf("the VM directory was not recreated: %v", serr)
			}
		})
	}
}
