// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package credstore

import "os"

// replaceFile renames src over dst. On Linux and macOS a rename replaces an
// open destination atomically, so no retry is needed.
func replaceFile(src, dst string) error {
	return os.Rename(src, dst)
}

// removeFile deletes path once. On Linux and macOS, unlinking a file works
// while another program has it open, so no retry is needed.
func removeFile(path string) error {
	return os.Remove(path)
}
