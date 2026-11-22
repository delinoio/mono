// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build !windows

package guest

import (
	"errors"
	"os"
)

// applyFileMode sets the mode of a finished write on the staging file. A file
// system that cannot hold modes (os.Chmod reports errors.ErrUnsupported) keeps
// the file as it is.
func applyFileMode(f *os.File, wire uint32) error {
	if err := f.Chmod(wireToFileMode(wire)); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		return err
	}
	return nil
}

// restoreStagingMode makes the staging file readable and writable for its
// owner again after a failed rename, so the file can be reopened. A file
// system without modes keeps the file as it is.
func restoreStagingMode(path string) error {
	if err := os.Chmod(path, 0o600); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		return err
	}
	return nil
}
