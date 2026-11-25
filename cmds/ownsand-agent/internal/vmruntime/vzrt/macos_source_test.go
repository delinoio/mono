// SPDX-License-Identifier: FSL-1.1-ALv2

package vzrt

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/images"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func writeMacOSGeneration(t *testing.T, dir, marker string, minCPU int32, minMemory, diskGiB int64) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{images.MacOSHardwareModel, images.MacOSDisk, images.MacOSAuxStorage} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(marker), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Truncate(filepath.Join(dir, images.MacOSDisk), diskGiB<<30); err != nil {
		t.Fatal(err)
	}
	if err := images.WriteMetadata(dir, images.Metadata{
		GuestOS: ownsandv1.GuestOs_GUEST_OS_MACOS, Source: marker,
		MinimumCPUCount: minCPU, MinimumMemoryMiB: minMemory,
	}); err != nil {
		t.Fatal(err)
	}
}

// Copy the generation marker only. Disk sizes still exercise the production
// capacity check against real sparse files.
func copyMacOSMarker(ctx context.Context, src, dst string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	marker := make([]byte, 1)
	if _, err := f.Read(marker); err != nil {
		return err
	}
	return os.WriteFile(dst, marker, 0o600)
}

func checkMacOSGeneration(t *testing.T, spec vmruntime.Spec, model []byte, want string) {
	t.Helper()
	if string(model) != want {
		t.Fatalf("model = %q, want %q", model, want)
	}
	for _, file := range []string{diskFile, auxFile} {
		f, err := os.Open(filepath.Join(spec.Dir, file))
		if err != nil {
			t.Fatal(err)
		}
		got := make([]byte, len(want))
		_, err = f.Read(got)
		f.Close()
		if err != nil || string(got) != want {
			t.Fatalf("%s = %q, %v, want %q", file, got, err, want)
		}
	}
}

func TestMacOSBaseImageGeneration(t *testing.T) {
	testMacOSBaseImageGeneration(t, copyMacOSMarker)
}

func testMacOSBaseImageGeneration(t *testing.T, copySource func(context.Context, string, string) error) {
	t.Helper()
	root := t.TempDir()
	base := filepath.Join(root, "images", images.MacOSDir)
	spec := vmruntime.Spec{
		Dir:       filepath.Join(root, "sandbox"),
		Resources: &ownsandv1.Resources{VcpuCount: 1, MemoryMib: 2048, DiskGib: 1},
	}
	writeMacOSGeneration(t, base, "A", 1, 2048, 1)
	supportsModel := func(model []byte) bool { return string(model) == "A" || string(model) == "B" }
	var build *images.Lock
	model, err := prepareMacOSSource(t.Context(), spec, base, supportsModel, func(ctx context.Context, src, dst string) error {
		if err := copySource(ctx, src, dst); err != nil {
			return err
		}
		if filepath.Base(src) == images.MacOSDisk {
			var err error
			build, err = images.TryLock(base)
			if err != nil {
				return err
			}
			t.Cleanup(func() {
				if err := build.Release(); err != nil {
					t.Error(err)
				}
			})
			writeMacOSGeneration(t, images.BuildDir(base), "B", 2, 4096, 2)
			installCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
			defer cancel()
			if err := images.Install(installCtx, base); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("install during source copy = %v, want deadline exceeded", err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	checkMacOSGeneration(t, spec, model, "A")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := images.Install(ctx, base); err != nil {
		t.Fatal(err)
	}
	_, err = prepareMacOSSource(t.Context(), spec, base, supportsModel, copySource)
	if !errors.Is(err, vmruntime.ErrUnsupported) || !strings.Contains(err.Error(), "2 vCPUs and 4096 MiB") {
		t.Fatalf("create with A's resource minimums after publication = %v", err)
	}
	spec.Resources.VcpuCount = 2
	spec.Resources.MemoryMib = 4096
	_, err = prepareMacOSSource(t.Context(), spec, base, supportsModel, copySource)
	if !errors.Is(err, vmruntime.ErrUnsupported) || !strings.Contains(err.Error(), "2147483648 bytes") {
		t.Fatalf("create with A's disk capacity after publication = %v", err)
	}
	spec.Resources.DiskGib = 2
	model, err = prepareMacOSSource(t.Context(), spec, base, supportsModel, copySource)
	if err != nil {
		t.Fatal(err)
	}
	checkMacOSGeneration(t, spec, model, "B")
}

func TestMacOSBaseImageGenerationRecoveryAndFailedCopy(t *testing.T) {
	for _, tc := range []string{"failed", "canceled"} {
		t.Run(tc, func(t *testing.T) {
			root := t.TempDir()
			base := filepath.Join(root, images.MacOSDir)
			writeMacOSGeneration(t, images.OldDir(base), "A", 1, 2048, 1)
			spec := vmruntime.Spec{
				Dir:       filepath.Join(root, "sandbox"),
				Resources: &ownsandv1.Resources{VcpuCount: 1, MemoryMib: 2048, DiskGib: 1},
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			copyErr := errors.New("failed to copy source")
			if tc == "canceled" {
				copyErr = context.Canceled
			}
			_, err := prepareMacOSSource(ctx, spec, base, func(model []byte) bool { return string(model) == "A" },
				func(ctx context.Context, src, dst string) error {
					if filepath.Base(src) == images.MacOSAuxStorage {
						if tc == "canceled" {
							cancel()
							return copyMacOSMarker(ctx, src, dst)
						}
						return copyErr
					}
					return copyMacOSMarker(ctx, src, dst)
				})
			if !errors.Is(err, copyErr) {
				t.Fatalf("prepare = %v, want %v", err, copyErr)
			}
			if _, err := os.Stat(spec.Dir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("sandbox directory after failed copy: %v", err)
			}
			meta, err := images.ReadMetadata(base)
			if err != nil || meta.Source != "A" {
				t.Fatalf("recovered metadata = %v, %v", meta, err)
			}
			build, err := images.TryLock(base)
			if err != nil {
				t.Fatal(err)
			}
			defer build.Release()
			writeMacOSGeneration(t, images.BuildDir(base), "B", 2, 4096, 2)
			installCtx, stop := context.WithTimeout(t.Context(), 5*time.Second)
			defer stop()
			if err := images.Install(installCtx, base); err != nil {
				t.Fatalf("install after failed copy: %v", err)
			}
		})
	}
}

func TestMacOSDiskPreparationUsesOnlySnapshotInputs(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, images.MacOSDir)
	snapshot := filepath.Join(root, "snapshot")
	writeMacOSGeneration(t, base, "B", 2, 4096, 2)
	writeMacOSGeneration(t, snapshot, "A", 1, 2048, 1)
	if err := os.Remove(filepath.Join(snapshot, images.MetadataFile)); err != nil {
		t.Fatal(err)
	}
	spec := vmruntime.Spec{
		Dir:       filepath.Join(root, "sandbox"),
		Resources: &ownsandv1.Resources{VcpuCount: 1, MemoryMib: 2048, DiskGib: 1},
		Snapshot:  &vmruntime.SnapshotSource{Dir: snapshot},
	}
	model, err := prepareMacOSSource(t.Context(), spec, base, func(model []byte) bool { return string(model) == "A" },
		func(ctx context.Context, src, dst string) error {
			if filepath.Dir(src) != snapshot {
				t.Fatalf("copy source %q is outside snapshot %q", src, snapshot)
			}
			return copyMacOSMarker(ctx, src, dst)
		})
	if err != nil {
		t.Fatal(err)
	}
	checkMacOSGeneration(t, spec, model, "A")
}
