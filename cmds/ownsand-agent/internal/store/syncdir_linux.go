// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build linux

package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// SyncPendingIdentityDirectories flushes the directory entries that make an
// existing pending key reachable. An earlier process may have created any
// of the ancestors and exited before flushing them, so start at the resolved
// state directory and flush each parent up to and including the root.
func (s *Store) SyncPendingIdentityDirectories() error {
	dir, err := filepath.Abs(s.dir)
	if err == nil {
		dir, err = filepath.EvalSymlinks(dir)
	}
	if err != nil {
		return fmt.Errorf("store: resolve state directory %s: %w", s.dir, err)
	}
	for {
		if err := syncDir(dir); err != nil {
			return fmt.Errorf("store: sync directory %s: %w", dir, err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil
		}
		dir = parent
	}
}

// syncDirectory flushes the entries of a directory (a file created, renamed
// or removed in it, or a subdirectory created in it) to stable storage. A
// file fsync does not do that: after a power loss, a file that was synced
// but whose directory entry was not can be gone.
//
// A filesystem that cannot synchronize a directory (EINVAL or ENOTSUP)
// has no stronger guarantee to offer, so that answer is not an error. Every
// other failure is returned.
func syncDirectory(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	serr := d.Sync()
	cerr := d.Close()
	if serr != nil && !errors.Is(serr, syscall.EINVAL) && !errors.Is(serr, syscall.ENOTSUP) {
		return serr
	}
	return cerr
}
