// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build !unix

package guest

import "os"

// openSource opens the source of a read snapshot. Windows has no FIFO that a
// plain open blocks on, and statSource and the identity check after the open
// already detect a swapped path there.
func openSource(path string) (*os.File, error) {
	return os.Open(path)
}
