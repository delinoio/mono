// SPDX-License-Identifier: Apache-2.0

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
