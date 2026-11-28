// SPDX-License-Identifier: FSL-1.1-ALv2

package guest

import (
	"crypto/rand"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

func mkdirWriteReservation(path string) error {
	// Windows ignores Unix permission bits. Create the directory with a
	// protected ACL for the helper's account, inherited by the seed file.
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;" + user.User.Sid.String() + ")")
	if err != nil {
		return err
	}
	attributes := windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: descriptor,
	}
	name, err := windows.UTF16PtrFromString(extendedPath(path))
	if err != nil {
		return err
	}
	if err := windows.CreateDirectory(name, &attributes); err != nil {
		return &os.PathError{Op: "mkdir", Path: path, Err: err}
	}
	return nil
}

func createWriteSeed(path, parent string) (*os.File, error) {
	// Create the seed in the destination directory so NTFS assigns its normal
	// inherited DACL. The caller holds the empty seed open without sharing
	// until its identity is recorded and the staging link exists. Closing the
	// handle removes the temporary source name and keeps the hard links.
	source := siblingPath(parent, ".ownsand-seed-"+rand.Text()+".tmp")
	name, err := windows.UTF16PtrFromString(extendedPath(source))
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(name, windows.FILE_READ_ATTRIBUTES|windows.DELETE, 0, nil,
		windows.CREATE_NEW, windows.FILE_FLAG_DELETE_ON_CLOSE, 0)
	if err != nil {
		return nil, &os.PathError{Op: "create", Path: source, Err: err}
	}
	seed := os.NewFile(uintptr(handle), source)
	if err := os.Link(source, path); err != nil {
		seed.Close()
		return nil, err
	}
	return seed, nil
}
