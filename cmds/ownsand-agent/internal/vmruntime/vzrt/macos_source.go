// SPDX-License-Identifier: FSL-1.1-ALv2

package vzrt

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/images"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime"
)

func checkBaseDiskCapacity(sourceBytes, requestedBytes int64) error {
	if sourceBytes > requestedBytes {
		return fmt.Errorf("%w: the base disk requires %d bytes, but the request allocates %d bytes", vmruntime.ErrUnsupported, sourceBytes, requestedBytes)
	}
	return nil
}

func checkMacOSBaseImage(dir string, spec vmruntime.Spec) error {
	meta, err := images.ReadMetadata(dir)
	if err != nil {
		return fmt.Errorf("%w: the macOS base image is not built; run `ownsand-agent image build`: %w", vmruntime.ErrUnsupported, err)
	}
	if spec.Resources.GetVcpuCount() < meta.MinimumCPUCount || spec.Resources.GetMemoryMib() < meta.MinimumMemoryMiB {
		return fmt.Errorf("%w: macOS guests need at least %d vCPUs and %d MiB of memory", vmruntime.ErrUnsupported, meta.MinimumCPUCount, meta.MinimumMemoryMiB)
	}
	disk, err := os.Stat(filepath.Join(dir, images.MacOSDisk))
	if err != nil {
		return fmt.Errorf("%w: the macOS base image is incomplete; build it again: %w", vmruntime.ErrUnsupported, err)
	}
	aux, err := os.Stat(filepath.Join(dir, images.MacOSAuxStorage))
	if err != nil {
		return fmt.Errorf("%w: the macOS base image is incomplete; build it again: %w", vmruntime.ErrUnsupported, err)
	}
	if !disk.Mode().IsRegular() || !aux.Mode().IsRegular() {
		return fmt.Errorf("%w: the macOS base image is incomplete; build it again", vmruntime.ErrUnsupported)
	}
	return checkBaseDiskCapacity(disk.Size(), spec.Resources.GetDiskGib()<<30)
}

// prepareMacOSSource validates the source and makes the sandbox's own copies.
// A base image stays locked until the model is in memory and both copies are
// ready. Snapshot preparation reads only the files in the snapshot directory.
func prepareMacOSSource(ctx context.Context, spec vmruntime.Spec, baseDir string, supportsModel func([]byte) bool, copyFile func(context.Context, string, string) error) ([]byte, error) {
	prepare := func(src string) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		model, err := os.ReadFile(filepath.Join(src, images.MacOSHardwareModel))
		if err != nil {
			if spec.Snapshot == nil {
				return nil, fmt.Errorf("%w: the macOS base image has no hardware model; build it again: %w", vmruntime.ErrUnsupported, err)
			}
			return nil, fmt.Errorf("vzrt: read the macOS hardware model: %w", err)
		}
		if !supportsModel(model) {
			return nil, fmt.Errorf("%w: this Mac does not support the hardware model of the macOS guest", vmruntime.ErrUnsupported)
		}
		if err := os.RemoveAll(spec.Dir); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(spec.Dir, 0o700); err != nil {
			return nil, err
		}
		if err := copyMacOSSource(ctx, src, spec.Dir, copyFile); err != nil {
			return nil, errors.Join(err, os.RemoveAll(spec.Dir))
		}
		return model, nil
	}
	if spec.Snapshot != nil {
		return prepare(spec.Snapshot.Dir)
	}
	if baseDir == "" {
		return nil, fmt.Errorf("%w: no macOS image directory is configured", vmruntime.ErrUnsupported)
	}
	var model []byte
	err := images.WithBaseImage(ctx, baseDir, func() error {
		if err := checkMacOSBaseImage(baseDir, spec); err != nil {
			return err
		}
		var err error
		model, err = prepare(baseDir)
		return err
	})
	return model, err
}

func copyMacOSSource(ctx context.Context, src, dst string, copyFile func(context.Context, string, string) error) error {
	if err := copyFile(ctx, filepath.Join(src, images.MacOSDisk), filepath.Join(dst, diskFile)); err != nil {
		return fmt.Errorf("vzrt: prepare disk: %w", err)
	}
	if err := copyFile(ctx, filepath.Join(src, images.MacOSAuxStorage), filepath.Join(dst, auxFile)); err != nil {
		return fmt.Errorf("vzrt: prepare auxiliary storage: %w", err)
	}
	return nil
}
