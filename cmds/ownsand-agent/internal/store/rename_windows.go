// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build windows

package store

import (
	"errors"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// Retry schedule of replaceFile. The total wait stays near one second.
const (
	replaceRetryDelay    = 10 * time.Millisecond
	replaceRetryAttempts = 100
)

// replaceFile moves the finished temporary file over the destination.
//
// On Windows the replacement fails with a sharing violation or access denied
// while another process has the destination open, for example a concurrent
// `ownsand-agent status` (os.ReadFile), a backup tool or antivirus. The
// replacement fails even if that handle shares delete access, so a reader
// cannot avoid it. Such a handle is short-lived, so the replacement retries
// for a bounded time before it reports the error; the caller keeps its
// previous report meanwhile. Other errors return at once.
//
// Workaround scope: Windows only. Remove it if Windows replaces a destination
// that other processes have open.
func replaceFile(oldPath, newPath string) error {
	var err error
	for attempt := 0; ; attempt++ {
		err = os.Rename(oldPath, newPath)
		if err == nil || !isSharingError(err) || attempt >= replaceRetryAttempts {
			return err
		}
		time.Sleep(replaceRetryDelay)
	}
}

func isSharingError(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}
