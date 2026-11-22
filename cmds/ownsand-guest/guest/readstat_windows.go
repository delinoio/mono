// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build windows

package guest

import (
	"os"
	"syscall"
)

// statSource returns the file information of the file at path, following a
// reparse point, read from one handle.
//
// os.Stat is not enough on Windows: it reads the size and modification time
// with GetFileAttributesEx and keeps only the path. os.SameFile then reads the
// volume serial number and file index by opening that path again when it
// compares. A path swapped to another file after the stat therefore reports
// the identity of the new file, and the read snapshot would copy the new file
// without noticing the swap. A FileInfo from File.Stat holds the identity of
// the handle, read together with the size and modification time.
//
// The handle asks for no access, so it does not read the file and works
// where os.Stat works. It shares every mode, so it neither fails on nor
// blocks a file that is open elsewhere.
func statSource(path string) (os.FileInfo, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, &os.PathError{Op: "stat", Path: path, Err: err}
	}
	h, err := syscall.CreateFile(p, 0,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return nil, &os.PathError{Op: "stat", Path: path, Err: err}
	}
	f := os.NewFile(uintptr(h), path)
	defer f.Close()
	return f.Stat()
}
