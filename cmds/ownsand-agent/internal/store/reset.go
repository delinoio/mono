// SPDX-License-Identifier: FSL-1.1-ALv2

package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ResetDir deletes the whole content of the state directory dir, including the
// identity, without opening a Store: unlike Open it creates no runtime
// subdirectory and needs no healthy layout, so a reset also works when an
// entry has the wrong type (for example a regular file at images) or the
// layout is incomplete. The directory itself stays.
//
// An absent directory counts as empty and is not an error. A path that is not
// a directory, an unreadable directory and an empty dir are errors. A failure
// to delete one entry does not stop the deletion of the others: an entry that
// stays (for example, a log file that another process holds open) must not
// keep the identity on disk. ResetDir returns every failure joined into one
// error.
func ResetDir(dir string) error {
	if dir == "" {
		return errors.New("store: state directory is required")
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("store: read state directory: %w", err)
	}
	var errs []error
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
