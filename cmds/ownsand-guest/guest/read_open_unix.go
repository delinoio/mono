// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build unix

package guest

import (
	"os"
	"syscall"
)

// openSource opens the source of a read snapshot. The path was a regular file
// at the first stat, but it can be replaced by a FIFO or a device before this
// open, and a plain open of a FIFO without a writer blocks until a writer
// appears: the worker would never end and the entry would never be reclaimed.
// O_NONBLOCK makes that open return at once. The kernel ignores it for a
// regular file, so ordinary reads are unchanged, and the caller then rejects
// the descriptor because it is not the regular file that it validated. A
// symlink is still followed, because a symlink source is legitimate.
func openSource(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
