// SPDX-License-Identifier: Apache-2.0

//go:build unix

package sandrun

import (
	"os"
	"syscall"
)

// A FIFO can replace a regular file after lookup. Open it without waiting
// for a writer so the handle check can reject it.
const syncFileOpenFlags = os.O_RDONLY | syscall.O_NONBLOCK
