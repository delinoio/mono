// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build linux

package sysinfo

import "golang.org/x/sys/unix"

func freeDiskBytes(dir string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, err
	}
	// statfs(2) block counts are in units of f_frsize on Linux, not f_bsize.
	return availBytes(st.Bavail, int64(st.Bsize), int64(st.Frsize)), nil
}
