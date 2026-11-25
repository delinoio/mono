// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build darwin && cgo

package vzrt_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/gpt"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/images"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/ops"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/sandboxes"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime/vzrt"
	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

const baseDiskGiB = 1 << 30

type baseDiskCase struct {
	name  string
	guest ownsandv1.GuestOs
	image ownsandv1.BaseImage
}

var baseDiskCases = []baseDiskCase{
	{name: "Linux", guest: ownsandv1.GuestOs_GUEST_OS_LINUX, image: ownsandv1.BaseImage_BASE_IMAGE_LINUX},
	{name: "macOS", guest: ownsandv1.GuestOs_GUEST_OS_MACOS, image: ownsandv1.BaseImage_BASE_IMAGE_MACOS},
}

func macOSDiskTestImage(t *testing.T) (string, images.Metadata) {
	t.Helper()
	const imageEnv = "OWNSAND_VZ_TEST_MACOS_IMAGE"
	image := os.Getenv(imageEnv)
	if runtime.GOARCH != "arm64" || image == "" {
		t.Skipf("set %s to a built image on Apple silicon", imageEnv)
	}
	meta, err := images.ReadMetadata(image)
	if err != nil {
		t.Fatal(err)
	}
	return image, meta
}

func newBaseDiskTestEnv(t *testing.T, opts vzrt.Options) (*env, vmruntime.Runtime) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	opts.VMsDir = st.Path(store.VMsDir)
	rt, err := vzrt.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := rt.Close(); err != nil {
			t.Error(err)
		}
	})
	m, err := sandboxes.New(context.Background(), sandboxes.Options{Store: st, Runtime: rt})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	d, err := ops.New(m, st, ops.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	return &env{t: t, m: m, d: d, st: st}, rt
}

func baseDiskCommand(t *testing.T, tc baseDiskCase, cpu int32, memoryMiB, diskGiB int64) *ownsandv1.CreateSandboxCommand {
	t.Helper()
	id, err := uuidv7.NewString()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	return &ownsandv1.CreateSandboxCommand{
		SandboxId:   id,
		Source:      &ownsandv1.CreateSandboxCommand_BaseImage{BaseImage: tc.image},
		GuestOs:     tc.guest,
		Resources:   &ownsandv1.Resources{VcpuCount: cpu, MemoryMib: memoryMiB, DiskGib: diskGiB},
		NetworkMode: ownsandv1.NetworkMode_NETWORK_MODE_NONE,
		IdleTtl:     durationpb.New(time.Hour),
		MaxTtl:      durationpb.New(time.Hour),
		CreateTime:  timestamppb.New(now),
		MaxDeadline: timestamppb.New(now.Add(time.Hour)),
		Limits:      &ownsandv1.DeviceLimits{VcpuCount: cpu, MemoryMib: memoryMiB, DiskGib: diskGiB, VcpuOvercommitPercent: 100},
	}
}

func TestVirtualizationRejectsUndersizedSparseBaseDisk(t *testing.T) {
	for _, tc := range baseDiskCases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			disk := filepath.Join(dir, "raw-disk")
			opts := vzrt.Options{Kernel: filepath.Join(dir, "kernel"), Rootfs: disk}
			cpu, mem := int32(2), int64(512)
			if tc.guest == ownsandv1.GuestOs_GUEST_OS_MACOS {
				image, meta := macOSDiskTestImage(t)
				model, err := os.ReadFile(filepath.Join(image, images.MacOSHardwareModel))
				if err != nil {
					t.Fatal(err)
				}
				for name, data := range map[string][]byte{images.MacOSHardwareModel: model, images.MacOSAuxStorage: nil} {
					if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				meta.DiskBytes = 2 * baseDiskGiB
				if err := images.WriteMetadata(dir, meta); err != nil {
					t.Fatal(err)
				}
				opts = vzrt.Options{MacOSImage: dir}
				disk = filepath.Join(dir, images.MacOSDisk)
				cpu, mem = max(cpu, meta.MinimumCPUCount), max(mem, meta.MinimumMemoryMiB)
			} else if err := os.WriteFile(opts.Kernel, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(disk, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Truncate(disk, 2*baseDiskGiB); err != nil {
				t.Fatal(err)
			}
			e, rt := newBaseDiskTestEnv(t, opts)
			cmd := baseDiskCommand(t, tc, cpu, mem, 1)
			spec := vmruntime.Spec{
				SandboxID: cmd.GetSandboxId(), Dir: t.TempDir(), GuestOS: tc.guest, BaseImage: tc.image,
				Resources: cmd.GetResources(), NetworkMode: cmd.GetNetworkMode(),
			}
			marker := filepath.Join(spec.Dir, "before-create")
			if err := os.WriteFile(marker, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := rt.Create(context.Background(), spec)
			want := fmt.Sprintf("requires %d bytes, but the request allocates %d bytes", 2*baseDiskGiB, baseDiskGiB)
			if !errors.Is(err, vmruntime.ErrUnsupported) || !strings.Contains(err.Error(), want) {
				t.Fatalf("direct create error = %v, want unsupported with %q", err, want)
			}
			if _, err := os.Stat(marker); err != nil {
				t.Fatalf("runtime touched the sandbox directory before rejecting the create: %v", err)
			}
			entry, err := e.m.Create(context.Background(), cmd)
			var cmdErr *sandboxes.CommandError
			if !errors.As(err, &cmdErr) || cmdErr.Code != ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION {
				t.Fatalf("manager create error = %v, want FAILED_PRECONDITION", err)
			}
			if entry.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_FAILED || entry.GetEndReason() != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_CREATE_FAILED {
				t.Fatalf("manager entry = %v, want FAILED / CREATE_FAILED", entry)
			}
			ids, err := rt.Running(context.Background())
			if err != nil || len(ids) != 0 || e.m.Busy() {
				t.Fatalf("rejected create left runtime VMs %v (error %v) or a busy manager", ids, err)
			}
			if _, err := os.Stat(e.st.Path(store.VMsDir, cmd.GetSandboxId())); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("rejected create left a sandbox directory: stat error %v", err)
			}
		})
	}
}

func TestVirtualizationBaseImageDiskCapacity(t *testing.T) {
	for _, tc := range baseDiskCases {
		t.Run(tc.name, func(t *testing.T) {
			opts := vzrt.Options{Kernel: os.Getenv(envKernel), Rootfs: os.Getenv(envRootfs)}
			baseDisk := opts.Rootfs
			cpu, mem := int32(2), int64(512)
			guestSize := "read -r sectors < /sys/class/block/vda/size && echo $((sectors * 512))"
			if tc.guest == ownsandv1.GuestOs_GUEST_OS_MACOS {
				image, meta := macOSDiskTestImage(t)
				opts = vzrt.Options{MacOSImage: image}
				baseDisk = filepath.Join(image, images.MacOSDisk)
				cpu, mem = max(cpu, meta.MinimumCPUCount), max(int64(4096), meta.MinimumMemoryMiB)
				guestSize = "diskutil info -plist disk0 | plutil -extract TotalSize raw -"
			} else if opts.Kernel == "" || opts.Rootfs == "" {
				t.Skipf("set %s and %s and sign the test binary to boot real VMs", envKernel, envRootfs)
			}
			base, err := os.Stat(baseDisk)
			if err != nil {
				t.Fatal(err)
			}
			if tc.guest == ownsandv1.GuestOs_GUEST_OS_LINUX && base.Size() < 2*baseDiskGiB {
				// The default Linux rootfs is smaller than 2 GiB. Enlarge a
				// private clone to exercise the exact-capacity boundary.
				baseDisk = filepath.Join(t.TempDir(), "rootfs.ext4")
				if err := unix.Clonefile(opts.Rootfs, baseDisk, 0); err != nil {
					t.Fatalf("clone Linux rootfs (set TMPDIR to its APFS volume): %v", err)
				}
				if err := os.Truncate(baseDisk, 2*baseDiskGiB); err != nil {
					t.Fatal(err)
				}
				opts.Rootfs = baseDisk
				base, err = os.Stat(baseDisk)
				if err != nil {
					t.Fatal(err)
				}
			}
			if base.Size() < baseDiskGiB || base.Size()%baseDiskGiB != 0 {
				t.Fatalf("use an image whose logical size is a positive whole number of GiB, got %d bytes", base.Size())
			}
			for _, diskGiB := range []int64{base.Size() / baseDiskGiB, base.Size()/baseDiskGiB + 1} {
				t.Run(fmt.Sprintf("%dGiB", diskGiB), func(t *testing.T) {
					e, _ := newBaseDiskTestEnv(t, opts)
					cmd := baseDiskCommand(t, tc, cpu, mem, diskGiB)
					entry, err := e.m.Create(context.Background(), cmd)
					if err != nil || entry.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
						t.Fatalf("create = %v, %v, want RUNNING", entry, err)
					}
					want := diskGiB * baseDiskGiB
					disk := e.st.Path(store.VMsDir, cmd.GetSandboxId(), "disk.img")
					info, err := os.Stat(disk)
					if err != nil {
						t.Fatal(err)
					}
					if info.Size() != want {
						t.Fatalf("prepared disk = %d bytes, want %d", info.Size(), want)
					}
					if tc.guest == ownsandv1.GuestOs_GUEST_OS_MACOS {
						f, err := os.Open(disk)
						if err != nil {
							t.Fatal(err)
						}
						defer f.Close()
						signature := make([]byte, len("EFI PART"))
						if _, err := f.ReadAt(signature, want-gpt.SectorSize); err != nil || string(signature) != "EFI PART" {
							t.Fatalf("backup GPT at disk end = %q, %v", signature, err)
						}
					}
					out, ex := e.exec(cmd.GetSandboxId(), "/bin/sh", "-c", guestSize)
					if !execSucceeded(ex) || strings.TrimSpace(out) != strconv.FormatInt(want, 10) {
						t.Fatalf("guest disk size = %q, exit %v, want %d bytes", out, ex, want)
					}
					after, err := os.Stat(baseDisk)
					if err != nil {
						t.Fatal(err)
					}
					if !os.SameFile(base, after) || after.Size() != base.Size() || !after.ModTime().Equal(base.ModTime()) {
						t.Fatal("create changed the source disk")
					}
					t.Logf("prepared and guest disk sizes both %d bytes, source unchanged", want)
				})
			}
		})
	}
}
