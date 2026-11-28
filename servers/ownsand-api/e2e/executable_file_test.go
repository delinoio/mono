// SPDX-License-Identifier: FSL-1.1-ALv2

package e2e

import (
	"os"
	"syscall"
	"testing"
)

// writeExecutable writes a program that the test runs later.
//
// The tests of this package run in parallel, and several of them start
// processes. A fork copies every open file descriptor of the test process
// into the child until the child calls exec. If the copy includes the
// descriptor that writes this file, Linux refuses to run the file with
// "text file busy" (ETXTBSY) until that child calls exec. os/exec forks while
// it holds syscall.ForkLock for writing, so holding the read lock while the
// file is open for writing keeps its descriptor out of every child, as the
// syscall.ForkLock documentation describes for new descriptors.
func writeExecutable(t *testing.T, path string, data []byte, perm os.FileMode) {
	t.Helper()
	syscall.ForkLock.RLock()
	err := os.WriteFile(path, data, perm)
	syscall.ForkLock.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
}
