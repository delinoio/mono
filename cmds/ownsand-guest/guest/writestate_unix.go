// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build unix

package guest

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
)

// fileIdentity names a file independent of its path: the device and the inode
// on Unix guests. The helper records it with the staging file of a write, so
// that a later helper process deletes the path only while it still holds the
// file that the helper created.
//
// Limit: a file system may give a freed inode number to the next new file, so
// a replacement that reuses the inode number of the removed staging file is
// not told apart from it.
type fileIdentity struct {
	Device uint64 `json:"device"`
	Index  uint64 `json:"index"`
}

func identityOfInfo(info fs.FileInfo) (fileIdentity, error) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fileIdentity{}, errors.ErrUnsupported
	}
	return fileIdentity{Device: uint64(st.Dev), Index: uint64(st.Ino)}, nil //nolint:unconvert // the field types differ between Unix systems
}

// identityOfFile returns the identity of an open file.
func identityOfFile(f *os.File) (fileIdentity, error) {
	info, err := f.Stat()
	if err != nil {
		return fileIdentity{}, err
	}
	return identityOfInfo(info)
}

// identityOfPath returns the identity of the file at path without following
// a symbolic link at the final element.
func identityOfPath(path string) (fileIdentity, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return fileIdentity{}, err
	}
	return identityOfInfo(info)
}
