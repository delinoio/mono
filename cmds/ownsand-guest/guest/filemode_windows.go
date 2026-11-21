// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build windows

package guest

import "os"

// applyFileMode ignores the finish mode on Windows guests, as the
// WriteFileFinish contract says. Go's File.Chmod on Windows maps a mode
// without the owner write bit (for example 0444) to the read-only file
// attribute, which would make the target unwritable for the guest user; the
// POSIX permission, setuid, setgid and sticky bits have no Windows meaning.
// The mode stays part of the recorded finish fields, so a repeated finish with
// another mode still fails with failed_precondition.
func applyFileMode(_ *os.File, _ uint32) error {
	return nil
}
