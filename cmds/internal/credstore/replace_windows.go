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

// Load reads the stored credentials under the store lock. Windows can
// deny replacement while a reader has the destination open, so the reader
// must close its handle before releasing the lock.
func (s Store) Load() (*Credentials, error) {
	return s.loadWithLock(readFile)
}

func (s Store) loadWithLock(read func(string) ([]byte, error)) (*Credentials, error) {
	unlock, err := s.lock()
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	defer unlock()
	return s.load(read)
}

// The caller holds the store lock. Another program can still hold an
// incompatible handle, so retry sharing violations for a bounded interval.
func readFile(path string) ([]byte, error) {
	return readFileWithRetry(path, os.ReadFile)
}

func readFileWithRetry(path string, read func(string) ([]byte, error)) ([]byte, error) {
	deadline := time.Now().Add(replaceRetryTimeout)
	for {
		data, err := read(path)
		if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) || time.Now().After(deadline) {
			return data, err
		}
		time.Sleep(replaceRetryInterval)
	}
}

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

// removeRetryTimeout bounds the retries of a removal that another program
// blocks for a short time. It is a variable so that tests can shorten it.
var removeRetryTimeout = replaceRetryTimeout

// removeRetrySleep pauses between two removal attempts. Tests replace it.
var removeRetrySleep = time.Sleep

// removeFile deletes path and retries for up to removeRetryTimeout while the
// removal fails with access denied or a sharing violation. A missing file is
// success, any other error returns at once, and after the timeout the last
// removal error is returned.
//
// Workaround: on Windows, DeleteFile fails with ERROR_SHARING_VIOLATION or
// ERROR_ACCESS_DENIED while another program holds the file open without
// FILE_SHARE_DELETE, for the same antivirus and indexer reasons as in
// replaceFile. The retry applies only to the credential file removal on
// Windows. Remove it together with the replaceFile retry.
func removeFile(path string) error {
	deadline := time.Now().Add(removeRetryTimeout)
	for {
		err := os.Remove(path)
		if err == nil || errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if !isTransientReplaceError(err) || time.Now().After(deadline) {
			return err
		}
		removeRetrySleep(replaceRetryInterval)
	}
}

func isTransientReplaceError(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}
