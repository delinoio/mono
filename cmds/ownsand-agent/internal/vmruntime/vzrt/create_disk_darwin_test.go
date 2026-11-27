// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build darwin && cgo

package vzrt

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func TestVirtualizationRejectsOversizedCopiedBaseDisk(t *testing.T) {
	const requestedBytes = 1 << 30
	const replacementBytes = 2 << 30

	dir := t.TempDir()
	kernel := filepath.Join(dir, "vmlinux")
	rootfs := filepath.Join(dir, "rootfs.ext4")
	replacement := filepath.Join(dir, "staged-rootfs.ext4")
	for path, size := range map[string]int64{kernel: 0, rootfs: requestedBytes, replacement: replacementBytes} {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(path, size); err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.Stat(replacement)
	if err != nil {
		t.Fatal(err)
	}

	r := &Runtime{
		opts:   Options{Kernel: kernel, Rootfs: rootfs, VMsDir: dir},
		logger: slog.Default(),
		vms:    make(map[string]*vm),
	}
	spec := vmruntime.Spec{
		SandboxID: "sandbox",
		Dir:       r.vmDir("sandbox"),
		GuestOS:   ownsandv1.GuestOs_GUEST_OS_LINUX,
		Resources: &ownsandv1.Resources{DiskGib: 1},
	}
	ops := vmCreateOps{
		cloneFile: func(src, dst string, flags int) error {
			if err := os.Rename(replacement, rootfs); err != nil {
				t.Fatal(err)
			}
			if err := unix.Clonefile(src, dst, flags); err != nil {
				t.Fatalf("clone replacement (set TMPDIR to an APFS volume): %v", err)
			}
			return nil
		},
		growFile: func(string, int64) error {
			t.Fatal("create tried to grow the oversized copy")
			return nil
		},
	}
	got, err := r.createVM(t.Context(), spec, ops)
	want := fmt.Sprintf("requires %d bytes, but the request allocates %d bytes", replacementBytes, requestedBytes)
	if got != nil || !errors.Is(err, vmruntime.ErrUnsupported) || !strings.Contains(err.Error(), want) {
		t.Fatalf("create = %v, %v, want unsupported with %q", got, err, want)
	}
	if _, err := os.Stat(spec.Dir); !errors.Is(err, os.ErrNotExist) || len(r.vms) != 0 {
		t.Fatalf("failed create left a directory or VM: stat error %v, %d VMs", err, len(r.vms))
	}
	after, err := os.Stat(rootfs)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("create changed the replacement rootfs")
	}
}
