// SPDX-License-Identifier: FSL-1.1-ALv2

package hcsrt

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/images"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func TestCheckWindowsResources(t *testing.T) {
	m := images.Metadata{GuestOS: ownsandv1.GuestOs_GUEST_OS_WINDOWS, MinimumCPUCount: 2, MinimumMemoryMiB: 2048}
	for _, tc := range []struct {
		vcpu int32
		mem  int64
		ok   bool
	}{{2, 2048, true}, {4, 8192, true}, {1, 4096, false}, {2, 2047, false}} {
		err := checkWindowsResources(&ownsandv1.Resources{VcpuCount: tc.vcpu, MemoryMib: tc.mem}, m)
		if (err == nil) != tc.ok || (err != nil && !errors.Is(err, vmruntime.ErrUnsupported)) {
			t.Errorf("%d vCPUs %d MiB: %v", tc.vcpu, tc.mem, err)
		}
	}
}

func TestWindowsOutboundUnsupported(t *testing.T) {
	r := &Runtime{opts: Options{WindowsImage: t.TempDir()}}
	_, err := r.Create(t.Context(), vmruntime.Spec{
		GuestOS: ownsandv1.GuestOs_GUEST_OS_WINDOWS, NetworkMode: ownsandv1.NetworkMode_NETWORK_MODE_OUTBOUND,
	})
	if !errors.Is(err, vmruntime.ErrUnsupported) || !strings.Contains(err.Error(), "Windows sandboxes run only with network mode NONE in this version") {
		t.Fatalf("outbound create = %v, want unsupported network mode", err)
	}
}

func writeWindowsGeneration(t *testing.T, dir, diskName, marker string, cpu int32, memory int64) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, diskName), []byte(marker), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := images.WriteMetadata(dir, images.Metadata{
		GuestOS: ownsandv1.GuestOs_GUEST_OS_WINDOWS, Source: marker, MinimumCPUCount: cpu, MinimumMemoryMiB: memory,
	}); err != nil {
		t.Fatal(err)
	}
}

func checkWindowsGeneration(t *testing.T, dir, disk, marker string) {
	t.Helper()
	data, err := os.ReadFile(disk)
	if err != nil || string(data) != marker {
		t.Fatalf("disk marker = %q, %v, want %q", data, err, marker)
	}
	meta, err := images.ReadMetadata(dir)
	if err != nil || meta.Source != marker {
		t.Fatalf("metadata marker = %q, %v, want %q", meta.Source, err, marker)
	}
}

func windowsGenerationFixture(t *testing.T) (*Runtime, vmruntime.Spec, vmCreateOps, error) {
	t.Helper()
	dir := t.TempDir()
	r := &Runtime{
		opts:   Options{WindowsImage: filepath.Join(dir, "windows"), VMsDir: dir},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		vms:    map[string]*vm{},
	}
	failOpenSystem(t, &hcsError{op: "HcsOpenComputeSystem", hr: hrSystemNotFound})
	spec := vmruntime.Spec{
		SandboxID: "sandbox",
		GuestOS:   ownsandv1.GuestOs_GUEST_OS_WINDOWS,
		Dir:       r.vmDir("sandbox"),
		Resources: &ownsandv1.Resources{VcpuCount: 1, MemoryMib: 2048, DiskGib: 32},
	}
	reachedHCS := errors.New("reached compute system creation")
	ops := vmCreateOps{
		copyFile: copyFile,
		growVHDX: func(_ string, size int64) (int64, error) { return size, nil },
		createSystem: func(string, any) (*system, error) {
			return nil, reachedHCS
		},
	}
	return r, spec, ops, reachedHCS
}

func TestWindowsBaseImageGeneration(t *testing.T) {
	r, spec, ops, reachedHCS := windowsGenerationFixture(t)
	base := r.opts.WindowsImage
	writeWindowsGeneration(t, base, images.WindowsDisk, "A", 1, 2048)
	build, err := images.TryLock(base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := build.Release(); err != nil {
			t.Error(err)
		}
	})

	installMustWait := func() {
		ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
		defer cancel()
		if err := images.Install(ctx, base); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("install during source copy = %v, want deadline exceeded", err)
		}
	}
	var copies int
	ops.copyFile = func(ctx context.Context, src, dst string) error {
		// These calls happen after validation, first before copying the disk
		// and then after the disk is in place and before copying metadata.
		if copies == 0 {
			writeWindowsGeneration(t, images.BuildDir(base), images.WindowsDisk, "B", 2, 4096)
		}
		installMustWait()
		copies++
		return copyFile(ctx, src, dst)
	}
	ops.growVHDX = func(disk string, size int64) (int64, error) {
		checkWindowsGeneration(t, spec.Dir, disk, "A")
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		if err := images.Install(ctx, base); err != nil {
			t.Fatalf("install during local disk growth = %v", err)
		}
		return size, nil
	}
	if _, err := r.createVM(t.Context(), spec, ops); !errors.Is(err, reachedHCS) || copies != 2 {
		t.Fatalf("first create = %v, copied %d files, want both files and compute system creation", err, copies)
	}

	ops.copyFile = copyFile
	ops.growVHDX = func(disk string, size int64) (int64, error) {
		checkWindowsGeneration(t, spec.Dir, disk, "B")
		return size, nil
	}
	for _, limit := range []struct {
		cpu    int32
		memory int64
		want   string
	}{
		{1, 4096, "at least 2 vCPUs"},
		{2, 2048, "at least 4096 MiB"},
	} {
		spec.Resources.VcpuCount, spec.Resources.MemoryMib = limit.cpu, limit.memory
		if _, err := r.createVM(t.Context(), spec, ops); !errors.Is(err, vmruntime.ErrUnsupported) || !strings.Contains(err.Error(), limit.want) {
			t.Fatalf("create below B's minimum = %v, want %q", err, limit.want)
		}
	}
	spec.Resources.VcpuCount, spec.Resources.MemoryMib = 2, 4096
	if _, err := r.createVM(t.Context(), spec, ops); !errors.Is(err, reachedHCS) {
		t.Fatalf("create with B = %v, want compute system creation", err)
	}
}

func TestWindowsBaseImageGenerationRecoveryAndCopyFailure(t *testing.T) {
	r, spec, ops, reachedHCS := windowsGenerationFixture(t)
	base := r.opts.WindowsImage
	if _, err := r.createVM(t.Context(), spec, ops); !errors.Is(err, vmruntime.ErrUnsupported) || !strings.Contains(err.Error(), "base image is not built") {
		t.Fatalf("missing image create = %v, want unsupported", err)
	}
	writeWindowsGeneration(t, images.OldDir(base), images.WindowsDisk, "A", 1, 2048)
	ops.growVHDX = func(disk string, size int64) (int64, error) {
		checkWindowsGeneration(t, spec.Dir, disk, "A")
		return size, nil
	}
	if _, err := r.createVM(t.Context(), spec, ops); !errors.Is(err, reachedHCS) {
		t.Fatalf("create with recovered backup = %v, want compute system creation", err)
	}

	copyErr := errors.New("injected disk copy failure")
	ops.copyFile = func(_ context.Context, _, dst string) error {
		if err := os.WriteFile(dst, []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
		return copyErr
	}
	if _, err := r.createVM(t.Context(), spec, ops); !errors.Is(err, copyErr) {
		t.Fatalf("create with a source copy failure = %v", err)
	}
	if _, err := os.Stat(spec.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("VM directory after a failed copy: %v", err)
	}
	build, err := images.TryLock(base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := build.Release(); err != nil {
			t.Error(err)
		}
	})
	writeWindowsGeneration(t, images.BuildDir(base), images.WindowsDisk, "B", 2, 4096)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := images.Install(ctx, base); err != nil {
		t.Fatalf("install after failed copy = %v", err)
	}
}

func TestWindowsBaseImageGenerationCancellationAndSnapshot(t *testing.T) {
	r, spec, ops, reachedHCS := windowsGenerationFixture(t)
	base := r.opts.WindowsImage
	writeWindowsGeneration(t, base, images.WindowsDisk, "B", 2, 4096)
	// Hold exclusive access on a separate handle, as a publisher would.
	lock, err := os.OpenFile(base+".publication.lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := windows.LockFileEx(windows.Handle(lock.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, new(windows.Overlapped)); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	if _, err := r.createVM(ctx, spec, ops); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("create waiting on publication = %v, want deadline exceeded", err)
	}
	if _, err := os.Stat(spec.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("VM directory after a canceled wait: %v", err)
	}

	spec.Snapshot = &vmruntime.SnapshotSource{Dir: filepath.Join(r.opts.VMsDir, "snapshot")}
	writeWindowsGeneration(t, spec.Snapshot.Dir, snapWindowsDisk, "A", 1, 2048)
	ops.growVHDX = func(disk string, size int64) (int64, error) {
		checkWindowsGeneration(t, spec.Dir, disk, "A")
		return size, nil
	}
	ctx, cancelRestore := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelRestore()
	spec.Resources.MemoryMib = 2047
	if _, err := r.createVM(ctx, spec, ops); !errors.Is(err, vmruntime.ErrUnsupported) || !strings.Contains(err.Error(), "at least 2048 MiB") {
		t.Fatalf("restore below the snapshot minimum = %v, want unsupported", err)
	}
	spec.Resources.MemoryMib = 2048
	if _, err := r.createVM(ctx, spec, ops); !errors.Is(err, reachedHCS) {
		t.Fatalf("restore while the base image is locked = %v, want compute system creation", err)
	}
}
