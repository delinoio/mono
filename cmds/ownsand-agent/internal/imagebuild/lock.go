// SPDX-License-Identifier: FSL-1.1-ALv2

package imagebuild

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// ErrBuildInProgress reports that another process builds the image of the
// same guest OS in the same images directory. Match it with errors.Is; the
// error value is a *BuildInProgressError.
var ErrBuildInProgress = errors.New("imagebuild: build in progress")

// BuildInProgressError is returned by Build when another process holds the
// build lock of the image destination. Build returns it before it creates,
// removes or modifies anything below the images directory except the lock
// file itself.
type BuildInProgressError struct {
	// Image is the name of the image directory (macos or windows).
	Image string
	// LockPath is the lock file that the other build holds.
	LockPath string
}

func (e *BuildInProgressError) Error() string {
	return fmt.Sprintf("imagebuild: build in progress for %s image (lock file %s is held by another process)", e.Image, e.LockPath)
}

// Is makes errors.Is(err, ErrBuildInProgress) match.
func (e *BuildInProgressError) Is(target error) bool { return target == ErrBuildInProgress }

// buildLock is an exclusive cross-process lock on the image destination. The
// operating system releases it when the holder closes the file or ends for
// any reason, so a crashed build never blocks a later one.
type buildLock struct {
	f *os.File
}

// buildLockPath returns the lock file of an image directory: the sibling
// `<dir>.build.lock` (for example images/windows.build.lock), next to the
// staging directory `<dir>.build` that it protects. Install and the
// platform builders never remove it, so every process locks the same file.
func buildLockPath(dir string) string { return dir + ".build.lock" }

// acquireBuildLock takes the build lock of the image directory dir without
// waiting. It returns *BuildInProgressError when another process holds it.
func acquireBuildLock(dir string, logger *slog.Logger) (*buildLock, error) {
	path := buildLockPath(dir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("imagebuild: create the images directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("imagebuild: open the build lock: %w", err)
	}
	ok, err := tryLockFile(f)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("imagebuild: lock %s: %w", path, err)
	}
	if !ok {
		_ = f.Close()
		image := strings.TrimSuffix(filepath.Base(path), ".build.lock")
		logger.Warn("image build refused: another build holds the lock", "image", image, "lock_path", path)
		return nil, &BuildInProgressError{Image: image, LockPath: path}
	}
	return &buildLock{f: f}, nil
}

// release unlocks and closes the lock file. It leaves the file in place:
// removing it would let a waiting process lock an unlinked file while a third
// process creates and locks a new one.
func (l *buildLock) release() error {
	unlockErr := unlockFile(l.f)
	closeErr := l.f.Close()
	return errors.Join(unlockErr, closeErr)
}
