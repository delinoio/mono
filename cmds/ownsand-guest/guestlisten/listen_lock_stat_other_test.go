// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build !windows

package guestlisten

import (
	"errors"
	"os"
)

func isClosedFileStatError(err error) bool {
	return errors.Is(err, os.ErrClosed)
}
