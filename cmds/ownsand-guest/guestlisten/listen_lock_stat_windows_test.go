// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build windows

package guestlisten

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func isClosedFileStatError(err error) bool {
	// On Windows, Stat can report an invalid handle from GetFileType after Close.
	return errors.Is(err, os.ErrClosed) || errors.Is(err, windows.ERROR_INVALID_HANDLE)
}
