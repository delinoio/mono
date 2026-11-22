// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build !windows

package guest

import (
	"errors"
	"syscall"
)

// isDiskFull reports whether err means that the guest disk has no space left.
func isDiskFull(err error) bool {
	return errors.Is(err, syscall.ENOSPC)
}

// isNotExecutable reports whether err means that the program file is not in an
// executable format.
func isNotExecutable(err error) bool {
	return errors.Is(err, syscall.ENOEXEC)
}

// isOutOfMemory reports whether err means that the guest has no resources left
// to start a process.
func isOutOfMemory(err error) bool {
	return errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.ENOMEM)
}
