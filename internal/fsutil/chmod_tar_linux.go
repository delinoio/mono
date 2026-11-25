// SPDX-License-Identifier: Apache-2.0

package fsutil

import (
	"errors"
	"io/fs"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

func chmodNewTarDir(root *os.Root, name string, mode fs.FileMode) error {
	// O_PATH opens the directory even if the umask removed read access.
	dir, err := root.OpenFile(name, unix.O_PATH|unix.O_DIRECTORY, 0)
	if err != nil {
		return err
	}
	defer dir.Close()
	fd := int(dir.Fd())
	// Linux before 6.6 lacks fchmodat2 with AT_EMPTY_PATH. Use procfs to
	// address the same open directory on those kernels. Remove the fallback
	// when the minimum supported kernel provides fchmodat2.
	err = unix.Fchmodat(fd, "", uint32(mode.Perm()), unix.AT_EMPTY_PATH)
	if errors.Is(err, unix.EOPNOTSUPP) {
		return os.Chmod("/proc/self/fd/"+strconv.Itoa(fd), mode)
	}
	if err != nil {
		return &fs.PathError{Op: "chmod", Path: name, Err: err}
	}
	return nil
}
