// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build !unix && !windows

package guest

import "os"

// tryLockFile is not implemented on this platform: it always reports the
// lock as taken, so two helpers on one state directory are not detected.
// Implement it with the first guest image for this platform.
func tryLockFile(*os.File) (bool, error) { return true, nil }
