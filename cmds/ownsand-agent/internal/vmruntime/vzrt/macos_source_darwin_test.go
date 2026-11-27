// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build darwin && cgo

package vzrt

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/images"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func TestMacOSBaseImageGenerationWithNativeCopies(t *testing.T) {
	testMacOSBaseImageGeneration(t, copyFile)
}

func TestMacOSBaseImageRecoversLinkedStorageWithNativeCopies(t *testing.T) {
	testMacOSBaseImageRejectsLinkedStorage(t, func(ctx context.Context, src, dst string) error {
		clone := func(src, dst string, flags int) error {
			err := unix.Clonefile(src, dst, flags)
			t.Logf("clonefile %s: %v", filepath.Base(src), err)
			return err
		}
		if err := copyFileWithClone(ctx, src, dst, clone); err != nil {
			return err
		}
		if filepath.Base(dst) == diskFile {
			return growFile(dst, 2<<30)
		}
		return nil
	})
}

func TestMacOSRuntimeRejectsLinkedBaseStorage(t *testing.T) {
	if !macOSGuestsSupported {
		t.Skip("macOS guests need an Apple silicon Mac")
	}
	const incomplete = "the macOS base image is incomplete; build it again"
	for _, name := range []string{images.MacOSDisk, images.MacOSAuxStorage} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			base := filepath.Join(root, images.MacOSDir)
			writeMacOSGeneration(t, base, "A", 1, 2048, 1)
			linkMacOSStorage(t, base, name)
			rt, err := New(Options{MacOSImage: base})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := rt.Close(); err != nil {
					t.Error(err)
				}
			})

			caps := rt.Capabilities(t.Context())
			if slices.Contains(caps.BaseImages, ownsandv1.BaseImage_BASE_IMAGE_MACOS) || len(caps.MacOSHardwareModels) != 0 {
				t.Fatal("capabilities advertised the incomplete base image")
			}
			check := slices.IndexFunc(caps.Checks, func(c *ownsandv1.HypervisorCheck) bool {
				return c.GetCheckId() == checkMacOSImage
			})
			if check < 0 || caps.Checks[check].GetPassed() || caps.Checks[check].GetDetail() != incomplete {
				t.Fatalf("capabilities did not report an incomplete macOS image: %v", caps.Checks)
			}

			vm, err := rt.Create(t.Context(), vmruntime.Spec{
				SandboxID: "linked-base",
				Dir:       filepath.Join(root, "sandbox"),
				GuestOS:   ownsandv1.GuestOs_GUEST_OS_MACOS,
				Resources: &ownsandv1.Resources{VcpuCount: 1, MemoryMib: 2048, DiskGib: 1},
			})
			if vm != nil || !errors.Is(err, vmruntime.ErrUnsupported) || !strings.Contains(err.Error(), incomplete) {
				t.Fatalf("create = %v, %v, want incomplete image precondition", vm, err)
			}
		})
	}
}
