// SPDX-License-Identifier: FSL-1.1-ALv2

package imagebuild

import (
	"errors"

	"golang.org/x/sys/unix"
)

func macOSMountPoints() ([]string, error) {
	for range 3 {
		n, err := unix.Getfsstat(nil, unix.MNT_NOWAIT)
		if err != nil {
			return nil, err
		}
		if n <= 0 {
			break
		}
		// Leave room for new mounts. If the buffer still fills, retry
		// instead of treating a potentially incomplete table as complete.
		stats := make([]unix.Statfs_t, n+8)
		n, err = unix.Getfsstat(stats, unix.MNT_NOWAIT)
		if err != nil {
			return nil, err
		}
		if n <= 0 || n >= len(stats) {
			continue
		}
		mounts := make([]string, 0, n)
		for _, stat := range stats[:n] {
			name := stat.Mntonname[:]
			if name[len(name)-1] != 0 {
				return nil, errors.New("host mount table contains an unterminated mount point")
			}
			mounts = append(mounts, unix.ByteSliceToString(name))
		}
		return mounts, nil
	}
	return nil, errors.New("could not read a complete host mount table")
}
