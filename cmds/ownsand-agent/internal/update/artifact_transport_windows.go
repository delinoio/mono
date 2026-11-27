// SPDX-License-Identifier: FSL-1.1-ALv2

package update

import (
	"errors"

	"golang.org/x/sys/windows"
)

func transientSocketError(err error) bool {
	return errors.Is(err, windows.WSAECONNRESET) || errors.Is(err, windows.WSAECONNABORTED) ||
		errors.Is(err, windows.ERROR_NETNAME_DELETED) || errors.Is(err, windows.WSAETIMEDOUT)
}
