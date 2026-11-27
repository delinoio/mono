// SPDX-License-Identifier: FSL-1.1-ALv2

package imagebuild

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/images"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// withMacOSBuildDir owns the staging directory through publication and cleanup.
func withMacOSBuildDir(ctx context.Context, dir string, logger *slog.Logger, run hostRunner, mountPoints func() ([]string, error), buildImage func(build string) error) error {
	build := images.BuildDir(dir)
	if err := removeMacOSBuild(ctx, run, mountPoints, build); err != nil {
		return err
	}
	if err := os.MkdirAll(build, 0o700); err != nil {
		return err
	}
	return cleanupFailedMacOSBuild(run, mountPoints, build, logger, buildImage(build))
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
	return images.Install(ctx, dir, ownsandv1.GuestOs_GUEST_OS_MACOS)
}
