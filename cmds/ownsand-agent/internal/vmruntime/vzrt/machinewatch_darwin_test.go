// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build darwin && cgo && arm64

package vzrt

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/Code-Hex/vz/v3"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func TestVirtualizationFailedRestoreWatcher(t *testing.T) {
	kernel, rootfs := os.Getenv("OWNSAND_VZ_TEST_KERNEL"), os.Getenv("OWNSAND_VZ_TEST_ROOTFS")
	if kernel == "" || rootfs == "" {
		t.Skip("set OWNSAND_VZ_TEST_KERNEL and OWNSAND_VZ_TEST_ROOTFS and sign the test binary")
	}
	r := &Runtime{opts: Options{Kernel: kernel}, logger: slog.Default()}
	state := filepath.Join(t.TempDir(), snapState)
	if err := os.WriteFile(state, []byte("invalid machine state"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := vmruntime.Spec{Resources: &ownsandv1.Resources{VcpuCount: 2, MemoryMib: 512}}
	for attempt := range 3 {
		dir := t.TempDir()
		if err := copyFile(rootfs, filepath.Join(dir, diskFile)); err != nil {
			t.Fatal(err)
		}
		console, err := vmruntime.OpenCappedLog(filepath.Join(dir, consoleFile), vmruntime.ConsoleLogLimit,
			r.logger, "failed-restore", vmruntime.ConsoleLogKindConsole)
		if err != nil {
			t.Fatal(err)
		}
		v := &vm{id: "failed-restore", dir: dir, console: console}
		t.Cleanup(func() { r.closeConsole(v) })
		cfg, err := r.configure(v, spec, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.newMachine(v, cfg); err != nil {
			t.Fatal(err)
		}
		w := v.watcher
		t.Cleanup(w.cancel)
		if err := restoreMemory(v.machine, state); err == nil {
			t.Fatal("the framework accepted invalid machine state")
		}
		if state := v.machine.State(); state != vz.VirtualMachineStateStopped {
			t.Fatalf("state after rejected restore = %v, want stopped", state)
		}
		if err := r.stopVM(v); err != nil {
			t.Fatal(err)
		}
		if v.machine != nil {
			t.Fatal("cleanup retained the stopped machine")
		}
		select {
		case <-w.done:
		default:
			t.Fatal("cleanup left the discarded machine's watcher active")
		}
		t.Logf("attempt %d: the framework rejected the restore, and cleanup released the stopped VM and watcher", attempt+1)
	}
}
