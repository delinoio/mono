// SPDX-License-Identifier: Apache-2.0

//go:build windows

package guest

import (
	"os"
	"syscall"
)

// fileIdentity names a file independent of its path: the volume serial number
// (Device) and the 64-bit file index (Index) of GetFileInformationByHandle on
// Windows guests. The helper records it with the staging file of a write, so
// that a later helper process deletes the path only while it still holds the
// file that the helper created. It uses only the standard syscall package.
//
// Limit: a file system may give a freed file index to the next new file, so a
// replacement that reuses the index of the removed staging file is not told
// apart from it.
type fileIdentity struct {
	Device uint64 `json:"device"`
	Index  uint64 `json:"index"`
}

func identityOfHandle(h syscall.Handle) (fileIdentity, error) {
	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(h, &info); err != nil {
		return fileIdentity{}, err
	}
	return fileIdentity{
		Device: uint64(info.VolumeSerialNumber),
		Index:  uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow),
	}, nil
}

// identityOfFile returns the identity of an open file.
func identityOfFile(f *os.File) (fileIdentity, error) {
	return identityOfHandle(syscall.Handle(f.Fd()))
}

// identityOfPath returns the identity of the file at path without following
// a reparse point at the final element. The handle asks for no access and
// shares every mode, so it neither fails on nor blocks a file that is open
// elsewhere.
func identityOfPath(path string) (fileIdentity, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return fileIdentity{}, err
	}
	h, err := syscall.CreateFile(p, 0,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING,
		syscall.FILE_FLAG_BACKUP_SEMANTICS|syscall.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return fileIdentity{}, &os.PathError{Op: "open", Path: path, Err: err}
	}
	defer syscall.CloseHandle(h)
	return identityOfHandle(h)
}
