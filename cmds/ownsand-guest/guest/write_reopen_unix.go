// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build unix

package guest

import (
	"os"

	"golang.org/x/sys/unix"
)

// The path can change after the identity check. Do not follow a final
// symlink or block if a FIFO appears before the open.
const stagingReopenFlags = os.O_RDWR | unix.O_NOFOLLOW | unix.O_NONBLOCK

// duplicateStagingFile keeps an independent handle for restoring access if
// closing or renaming the staging file fails. The caller owns f and keeps
// it open during duplication.
func duplicateStagingFile(f *os.File) (*os.File, error) {
	fd, err := unix.FcntlInt(f.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), f.Name()), nil
}
