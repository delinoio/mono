// SPDX-License-Identifier: FSL-1.1-ALv2

package guest

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// Workaround: an earlier read worker can outlive readManager.close and hold a
// file without delete sharing. Retain its unreachable tree until the next
// start, when the handle may have closed. Remove this fallback when workers
// can no longer outlive close.
func clearReadRoot(root string, logger *slog.Logger) error {
	if err := os.RemoveAll(root); err == nil {
		return nil
	}

	state, err := os.OpenRoot(filepath.Dir(root))
	if err != nil {
		return err
	}
	defer state.Close()
	name := filepath.Base(root)
	if err := state.Remove(name); !errors.Is(err, windows.ERROR_DIR_NOT_EMPTY) {
		return ignoreMissingReadCleanupEntry(err)
	}

	retained, err := clearReadRootChildren(state, name)
	if err != nil {
		return err
	}
	// Retry removal even when only retained snapshots remain. A separate
	// failure on reads itself is fatal.
	err = state.Remove(name)
	if len(retained) == 0 || !errors.Is(err, windows.ERROR_DIR_NOT_EMPTY) {
		return ignoreMissingReadCleanupEntry(err)
	}
	for _, sharing := range retained {
		logger.Warn("old read snapshot directory is still in use and stays until the next start",
			"error", sanitizedError(sharing))
	}
	return nil
}

func clearReadRootChildren(state *os.Root, name string) ([]error, error) {
	root, err := openReadCleanupDir(state, name)
	if err != nil {
		return nil, ignoreMissingReadCleanupEntry(err)
	}
	defer root.Close()
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return nil, err
	}

	var retained []error
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), snapshotDirPrefix) {
			if err := root.RemoveAll(entry.Name()); err != nil {
				return nil, err
			}
			continue
		}
		sharing, err := removeOldReadSnapshot(root, entry)
		if err != nil {
			return nil, err
		}
		if sharing != nil {
			retained = append(retained, sharing)
		}
	}
	return retained, nil
}

// removeOldReadSnapshot returns a sharing conflict from a deletion separately
// from a fatal error. RemoveAll can report only one descendant error, so check
// every surviving entry before allowing a sharing conflict to keep the tree.
func removeOldReadSnapshot(parent *os.Root, entry os.DirEntry) (sharing, fatal error) {
	name := entry.Name()
	err := ignoreMissingReadCleanupEntry(parent.Remove(name))
	if err == nil {
		return nil, nil
	}
	if !entry.IsDir() {
		if errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			return err, nil
		}
		return nil, err
	}
	if !errors.Is(err, windows.ERROR_DIR_NOT_EMPTY) && !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		return nil, err
	}

	dir, err := openReadCleanupDir(parent, name)
	if err != nil {
		return nil, ignoreMissingReadCleanupEntry(err)
	}
	defer dir.Close()
	children, err := fs.ReadDir(dir.FS(), ".")
	if err != nil {
		return nil, err
	}
	for _, child := range children {
		blocked, err := removeOldReadSnapshot(dir, child)
		if err != nil {
			return nil, err
		}
		if sharing == nil {
			sharing = blocked
		}
	}
	if err := dir.Close(); err != nil {
		return nil, err
	}
	err = ignoreMissingReadCleanupEntry(parent.Remove(name))
	switch {
	case errors.Is(err, windows.ERROR_SHARING_VIOLATION):
		return err, nil
	case sharing != nil && errors.Is(err, windows.ERROR_DIR_NOT_EMPTY):
		return sharing, nil
	default:
		return nil, err
	}
}

func openReadCleanupDir(parent *os.Root, name string) (*os.Root, error) {
	info, err := parent.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, &os.PathError{Op: "open", Path: name, Err: windows.ERROR_DIRECTORY}
	}
	dir, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	// Root keeps operations inside the opened directory. Verify its identity
	// in case another process replaced the entry with a junction or symlink.
	opened, err := dir.Stat(".")
	if err == nil && !os.SameFile(info, opened) {
		err = &os.PathError{Op: "open", Path: name, Err: windows.ERROR_DIRECTORY}
	}
	if err != nil {
		dir.Close()
		return nil, err
	}
	return dir, nil
}

func ignoreMissingReadCleanupEntry(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
