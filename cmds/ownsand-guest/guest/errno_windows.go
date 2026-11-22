// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build windows

package guest

import (
	"errors"

	"golang.org/x/sys/windows"
)

// The syscall package on Windows defines the POSIX names ENOSPC, ENOEXEC,
// EAGAIN and ENOMEM only as invented placeholder values above the range of
// Win32 error codes, so no real Windows error ever matches them. These helpers
// compare the Win32 codes that the same conditions produce.

// isDiskFull reports whether err means that the guest disk has no space left.
func isDiskFull(err error) bool {
	return errors.Is(err, windows.ERROR_DISK_FULL) || errors.Is(err, windows.ERROR_HANDLE_DISK_FULL)
}

// isNotExecutable reports whether err means that the program file is not a
// runnable image for this host. CreateProcess returns ERROR_BAD_EXE_FORMAT for
// a file that is not a valid executable image, and newer Windows versions
// return ERROR_EXE_MACHINE_TYPE_MISMATCH for a file with the .exe extension
// whose content is not an image for this machine (for example a text file).
func isNotExecutable(err error) bool {
	return errors.Is(err, windows.ERROR_BAD_EXE_FORMAT) || errors.Is(err, windows.ERROR_EXE_MACHINE_TYPE_MISMATCH)
}

// isOutOfMemory reports whether err means that the guest has no resources left
// to start a process.
func isOutOfMemory(err error) bool {
	return errors.Is(err, windows.ERROR_NOT_ENOUGH_MEMORY) || errors.Is(err, windows.ERROR_OUTOFMEMORY)
}
