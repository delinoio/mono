// SPDX-License-Identifier: FSL-1.1-ALv2

package guest

import (
	"os"
	"syscall"
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

func prepareStagingPermissions(path string, identity fileIdentity) error {
	name, err := windows.UTF16PtrFromString(extendedPath(path))
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(name, windows.WRITE_DAC,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	current, err := identityOfHandle(syscall.Handle(handle))
	if err != nil {
		return err
	}
	if current != identity {
		return os.ErrNotExist
	}
	// A hard link shares the seed's ACL. Reset it at the staging name so the
	// file inherits from the destination directory as a directly created
	// staging file would.
	descriptor, err := windows.SecurityDescriptorFromString("D:")
	if err != nil {
		return err
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	return windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.UNPROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}
