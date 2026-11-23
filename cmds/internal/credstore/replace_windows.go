// SPDX-License-Identifier: Apache-2.0

//go:build windows

package credstore

import (
	"errors"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// replaceRetryTimeout bounds the retries of a replace that another program
// blocks for a short time.
const replaceRetryTimeout = 2 * time.Second

// replaceRetryInterval is the pause between two replace attempts.
const replaceRetryInterval = 10 * time.Millisecond

// replaceFile renames src over dst and retries for up to
// replaceRetryTimeout while the rename fails with access denied or a
// sharing violation.
//
// Workaround: on Windows, MoveFileEx fails with ERROR_ACCESS_DENIED or
// ERROR_SHARING_VIOLATION while another program holds the destination open
// without FILE_SHARE_DELETE. Antivirus scanners and the search indexer open a
// file that was just written for a short time, so a second save soon after
// the first one can fail although the CLI holds the store lock. The Go
// toolchain retries the same errors for the same reason
// (cmd/go/internal/robustio). The retry applies only to the credential file
// replace on Windows. Remove it if the credential file is no longer replaced
// by a rename, or if Windows rename semantics that ignore open handles
// (FILE_RENAME_FLAG_POSIX_SEMANTICS) are used instead.
func replaceFile(src, dst string) error {
	deadline := time.Now().Add(replaceRetryTimeout)
	for {
		err := os.Rename(src, dst)
		if err == nil || !isTransientReplaceError(err) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(replaceRetryInterval)
	}
}

func isTransientReplaceError(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}
