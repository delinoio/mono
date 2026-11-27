// SPDX-License-Identifier: FSL-1.1-ALv2

package imagebuild

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/images"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime/vzrt"
)

// withMacOSBuildDir owns the staging directory through publication and cleanup.
func withMacOSBuildDir(dir string, logger *slog.Logger, run func(build string) error) error {
	build := images.BuildDir(dir)
	if err := os.RemoveAll(build); err != nil {
		return err
	}
	if err := os.MkdirAll(build, 0o700); err != nil {
		return err
	}
	err := run(build)
	if errors.Is(err, vzrt.ErrVMMayRun) {
		// The build VM may still use the staged disk, so deleting it
		// could corrupt a running guest. The next build removes it.
		logger.Error("keeping the macOS build directory because the build VM may still run", "build_dir", build)
	} else if err != nil {
		os.RemoveAll(build)
	}
	return err
}

func finishMacOSImage(ctx context.Context, dir string, metadata images.Metadata, downloadedIPSW string, firstBoot func() (string, error)) error {
	helper, err := firstBoot()
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	metadata.BuiltAt = time.Now().UTC()
	metadata.GuestVersion = helper
	if err := images.WriteMetadata(images.BuildDir(dir), metadata); err != nil {
		return err
	}
	if downloadedIPSW != "" {
		os.Remove(downloadedIPSW)
	}
	return images.Install(ctx, dir)
}
