// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build !windows

package update

import (
	"errors"
	"syscall"
)

func transientSocketError(err error) bool {
	return errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.EPIPE)
}
