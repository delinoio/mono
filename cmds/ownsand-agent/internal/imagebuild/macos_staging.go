// SPDX-License-Identifier: FSL-1.1-ALv2

package imagebuild

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/images"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime/vzrt"
)

func cleanupFailedMacOSBuild(run hostRunner, mountPoints func() ([]string, error), build string, logger *slog.Logger, buildErr error) error {
	switch {
	case buildErr == nil:
		return nil
	case errors.Is(buildErr, vzrt.ErrVMMayRun):
		logger.Error("keeping the macOS build directory because the build VM may still run", "build_dir", build)
		return buildErr
	case errors.Is(buildErr, errDiskDetachUnconfirmed):
		return fmt.Errorf("imagebuild: keeping the macOS build directory %q: %w", build, buildErr)
	}
	// The build context may already be canceled. Cleanup still needs a fresh
	// attachment inventory before removing staging.
	if err := removeMacOSBuild(context.Background(), run, mountPoints, build); err != nil {
		return errors.Join(buildErr, err)
	}
	return buildErr
}

// The caller holds the image build lock through both the checks and removal.
func removeMacOSBuild(ctx context.Context, run hostRunner, mountPoints func() ([]string, error), build string) error {
	info, err := os.Lstat(build)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err == nil {
		err = checkMacOSBuildUnused(ctx, run, mountPoints, build, info)
	}
	if err != nil {
		return fmt.Errorf("imagebuild: keeping the macOS build directory %q: %w", build, err)
	}
	return os.RemoveAll(build)
}

func checkMacOSBuildUnused(ctx context.Context, run hostRunner, mountPoints func() ([]string, error), build string, buildInfo os.FileInfo) error {
	if !buildInfo.IsDir() {
		return errors.New("staging path is not a directory")
	}
	mounts, err := mountPoints()
	if err != nil {
		return fmt.Errorf("read the host mount table: %w", err)
	}
	for _, mount := range mounts {
		if !filepath.IsAbs(mount) {
			return fmt.Errorf("invalid host mount point %q", mount)
		}
		// Compare directory identities to recognize symlinks and alternate
		// paths to the same directory on the macOS Data volume.
		for dir := filepath.Clean(mount); ; dir = filepath.Dir(dir) {
			info, err := os.Stat(dir)
			if err != nil {
				return fmt.Errorf("verify host mount point %q: %w", mount, err)
			}
			if os.SameFile(info, buildInfo) {
				return fmt.Errorf("host mount %q is still active; unmount it before retrying", mount)
			}
			if filepath.Dir(dir) == dir {
				break
			}
		}
	}

	disk, err := filepath.Abs(filepath.Join(build, images.MacOSDisk))
	if err != nil {
		return fmt.Errorf("resolve the staged disk path: %w", err)
	}
	diskInfo, err := os.Stat(disk)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect the staged disk: %w", err)
	}
	resolvedBuild, err := filepath.EvalSymlinks(filepath.Dir(disk))
	if err != nil {
		return fmt.Errorf("resolve the staging path: %w", err)
	}
	resolvedDisk := filepath.Join(resolvedBuild, images.MacOSDisk)

	ctx, cancel := context.WithTimeout(ctx, attachmentInfoTimeout)
	defer cancel()
	out, err := run(ctx, hdiutilPath, "info", "-plist")
	if err != nil {
		return fmt.Errorf("read the attached disk inventory: %w", err)
	}
	plist, err := readDiskImagePlist(out)
	if err != nil {
		return fmt.Errorf("read the attached disk inventory: %w", err)
	}
	images := plist.array("images")
	if images == nil {
		return errors.New("attached disk inventory has no images array")
	}
	for _, image := range images {
		path := image.dict.text("image-path")
		if !filepath.IsAbs(path) {
			return fmt.Errorf("attached disk inventory has an invalid image path %q", path)
		}
		path = filepath.Clean(path)
		if strings.EqualFold(path, disk) || strings.EqualFold(path, resolvedDisk) {
			return fmt.Errorf("staged disk %q is still attached; detach it before retrying", path)
		}
		if diskInfo == nil {
			continue
		}
		info, err := os.Stat(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("verify attached disk path %q: %w", path, err)
		}
		if err == nil && os.SameFile(info, diskInfo) {
			return fmt.Errorf("staged disk %q is still attached; detach it before retrying", path)
		}
	}
	return nil
}
