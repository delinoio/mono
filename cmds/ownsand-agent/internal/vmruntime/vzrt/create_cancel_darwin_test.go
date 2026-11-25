// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build darwin && cgo

package vzrt

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/images"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func TestVirtualizationCreateCancelsDiskPreparation(t *testing.T) {
	const (
		linux = ownsandv1.GuestOs_GUEST_OS_LINUX
		macos = ownsandv1.GuestOs_GUEST_OS_MACOS
	)
	type stage int
	const (
		stream stage = iota
		clone
		grow
		fit
	)
	for _, tc := range []struct {
		name       string
		os         ownsandv1.GuestOs
		fallback   bool
		cancelAt   stage
		copyNumber int
	}{
		{"linux disk stream", linux, false, stream, 1},
		{"linux fallback disk stream", linux, true, stream, 2},
		{"linux clone", linux, false, clone, 1},
		{"linux growth", linux, false, grow, 1},
		{"macOS disk stream", macos, false, stream, 1},
		{"macOS auxiliary stream", macos, false, stream, 2},
		{"macOS fallback disk stream", macos, true, stream, 3},
		{"macOS fallback auxiliary stream", macos, true, stream, 4},
		{"macOS disk clone", macos, false, clone, 1},
		{"macOS growth", macos, false, grow, 2},
		{"macOS partition table", macos, false, fit, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.fallback && !memoryStateSupported {
				t.Skip("the host cannot restore memory state")
			}
			dir := t.TempDir()
			source, kernel := filepath.Join(dir, snapDisk), filepath.Join(dir, "kernel")
			data := bytes.Repeat([]byte("disk image\n"), 1<<17)
			write := func(path string, data []byte) {
				t.Helper()
				if err := os.WriteFile(path, data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			write(source, data)
			write(kernel, nil)
			r := &Runtime{
				opts:   Options{Kernel: kernel, Rootfs: source, VMsDir: dir},
				logger: slog.Default(),
				vms:    make(map[string]*vm),
			}
			spec := vmruntime.Spec{
				SandboxID:   "sandbox",
				Dir:         r.vmDir("sandbox"),
				Resources:   &ownsandv1.Resources{DiskGib: 1},
				GuestOS:     tc.os,
				NetworkMode: ownsandv1.NetworkMode_NETWORK_MODE_NONE,
			}
			if tc.os == macos {
				image := os.Getenv("OWNSAND_VZ_TEST_MACOS_IMAGE")
				if !macOSGuestsSupported || image == "" {
					t.Skip("set OWNSAND_VZ_TEST_MACOS_IMAGE on Apple silicon for a supported hardware model")
				}
				model, err := os.ReadFile(filepath.Join(image, images.MacOSHardwareModel))
				if err != nil {
					t.Fatal(err)
				}
				write(filepath.Join(dir, images.MacOSHardwareModel), model)
				write(filepath.Join(dir, images.MacOSAuxStorage), data)
			}
			if tc.os == macos || tc.fallback {
				// Omit the machine identifier to force recoverable memory restore failure.
				spec.Snapshot = &vmruntime.SnapshotSource{Dir: dir, HasMemoryState: tc.fallback, Resources: spec.Resources}
				spec.MemoryRestoreAllowed = tc.fallback
			}

			base, cancel := context.WithCancel(t.Context())
			defer cancel()
			ctx := &cancelStreamedCopy{Context: base, cancel: cancel}
			copies, grows, fits := 0, 0, 0
			ops := vmCreateOps{
				cloneFile: func(_, dst string, _ int) error {
					copies++
					if tc.cancelAt == stream && copies == tc.copyNumber {
						ctx.path = dst
						return unix.EXDEV
					}
					write(dst, data)
					if tc.cancelAt == clone && copies == tc.copyNumber {
						cancel()
					}
					return nil
				},
				growFile: func(path string, size int64) error {
					grows++
					err := growFile(path, size)
					if tc.cancelAt == grow {
						cancel()
					}
					return err
				},
				fitGPT: func(string) error {
					fits++
					if tc.cancelAt == fit {
						cancel()
					}
					return nil
				},
			}
			got, err := r.createVM(ctx, spec, ops)
			if got != nil || !errors.Is(err, context.Canceled) {
				t.Fatalf("create = %v, %v, want context.Canceled", got, err)
			}
			if tc.cancelAt == stream && (ctx.bytesCopied == 0 || ctx.bytesCopied >= int64(len(data))) {
				t.Fatalf("stream wrote %d of %d bytes before cancellation", ctx.bytesCopied, len(data))
			}
			wantGrows, wantFits := 0, 0
			if tc.cancelAt == grow || tc.cancelAt == fit {
				wantGrows = 1
			}
			if tc.cancelAt == fit {
				wantFits = 1
			}
			if copies != tc.copyNumber || grows != wantGrows || fits != wantFits {
				t.Fatalf("copies, grows, GPT fits = %d, %d, %d, want %d, %d, %d",
					copies, grows, fits, tc.copyNumber, wantGrows, wantFits)
			}
			if _, err := os.Stat(spec.Dir); !errors.Is(err, os.ErrNotExist) || len(r.vms) != 0 {
				t.Fatalf("failed create left a directory or VM: stat error %v, %d VMs", err, len(r.vms))
			}
		})
	}
}

func TestVirtualizationCanceledBootAndRestoreSkipConfiguration(t *testing.T) {
	r := &Runtime{}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, step := range []func(context.Context, *vm, vmruntime.Spec) error{
		r.coldBoot, r.coldBootMacOS, r.restoreMemory, r.restoreMacOS,
	} {
		// An incomplete VM and spec cannot reach native configuration.
		if err := step(ctx, nil, vmruntime.Spec{}); !errors.Is(err, context.Canceled) {
			t.Fatalf("boot or restore returned %v, want context.Canceled", err)
		}
	}
}
