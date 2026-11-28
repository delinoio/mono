// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build !windows

package guest

import "os"

func mkdirWriteReservation(path string) error {
	return os.Mkdir(path, 0o700)
}

func prepareStagingPermissions(string, fileIdentity) error { return nil }
