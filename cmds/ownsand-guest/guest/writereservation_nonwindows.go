// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build !windows

package guest

import "os"

func mkdirWriteReservation(path string) error {
	return os.Mkdir(path, 0o700)
}

func createWriteSeed(path, _ string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
}
