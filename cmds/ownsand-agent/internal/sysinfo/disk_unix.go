// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build linux || darwin

package sysinfo

import "golang.org/x/sys/unix"

func freeDiskBytes(dir string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}
