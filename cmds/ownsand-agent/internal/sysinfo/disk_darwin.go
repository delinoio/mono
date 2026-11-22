// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build darwin

package sysinfo

import "golang.org/x/sys/unix"

func freeDiskBytes(dir string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, err
	}
	// statfs(2) block counts are in units of f_bsize on macOS, which has no
	// fragment size field, so pass zero.
	return availBytes(st.Bavail, int64(st.Bsize), 0), nil
}
