// SPDX-License-Identifier: FSL-1.1-ALv2

package hcsrt

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
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
