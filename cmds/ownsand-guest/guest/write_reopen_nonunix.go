// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build !unix

package guest

import "os"

const stagingReopenFlags = os.O_RDWR

// Windows leaves the staging file writable, so reopening needs no
// descriptor for permission changes.
func duplicateStagingFile(*os.File) (*os.File, error) {
	return nil, nil
}
