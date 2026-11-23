// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build !unix && !windows

package guestlisten

import "os"

// tryLockFile is not implemented on this platform: it always reports the
// lock as taken, so concurrent starts on one path are not serialized.
// Implement it with the first guest image for this platform.
func tryLockFile(*os.File) (bool, error) { return true, nil }

// unlockFile does nothing, because tryLockFile takes no lock.
func unlockFile(*os.File) {}
