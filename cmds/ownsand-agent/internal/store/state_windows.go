// SPDX-License-Identifier: FSL-1.1-ALv2

package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

const stateRootSDDL = "O:BAD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"

// PrepareDir creates and verifies the root before the service opens its log.
func PrepareDir(dir string) error {
	root, _, err := prepareWindowsRoot(dir, true)
	if err != nil {
		return err
	}
	return windows.CloseHandle(root)
}

func prepareStateLayout(dir string) error {
	root, p, err := prepareWindowsRoot(dir, true)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(root)
	for _, sub := range stateSubdirs {
		if err := prepareStateSubdir(filepath.Join(dir, sub)); err != nil {
			return err
		}
	}
	return resetChildren(dir, p)
}

func prepareResetDir(dir string) (func(), error) {
	root, _, err := prepareWindowsRoot(dir, false)
	if err != nil {
		return nil, err
	}
	return func() { _ = windows.CloseHandle(root) }, nil
}

func prepareWindowsRoot(dir string, create bool) (windows.Handle, *principals, error) {
	if dir == "" {
		return 0, nil, errors.New("store: state directory is required")
	}
	p, err := loadPrincipals()
	if err != nil {
		return 0, nil, err
	}
	if create {
		if err := createStateRoot(filepath.Clean(dir)); err != nil {
			return 0, nil, fmt.Errorf("store: create %s: %w", dir, err)
		}
	}
	h, info, err := openStateEntry(dir, windows.READ_CONTROL)
	if err != nil {
		return 0, nil, err
	}
	err = verifyStateDirectory(dir, info.FileAttributes)
	if err == nil {
		err = checkHandleOwner(dir, h, p.rootOwners)
	}
	if err == nil {
		err = protectRoot(dir, h, p)
	}
	if err != nil {
		windows.CloseHandle(h)
		return 0, nil, err
	}
	return h, p, nil
}

func createStateRoot(dir string) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return err
	}
	descriptor, err := windows.SecurityDescriptorFromString(stateRootSDDL)
	if err != nil {
		return err
	}
	name, err := statePathName(dir)
	if err != nil {
		return err
	}
	attrs := windows.SecurityAttributes{
		Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: descriptor,
	}
	err = windows.CreateDirectory(name, &attrs)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	return err
}

func prepareStateSubdir(path string) error {
	if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	h, info, err := openStateEntry(path, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	return verifyStateDirectory(path, info.FileAttributes)
}

func verifyStateDirectory(path string, attrs uint32) error {
	switch {
	case attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0:
		return &StatePathError{Path: path, Reason: StatePathReparsePoint}
	case attrs&windows.FILE_ATTRIBUTE_DIRECTORY == 0:
		return &StatePathError{Path: path, Reason: StatePathNotDirectory}
	default:
		return nil
	}
}

// Open the entry itself and keep it from being renamed while it is checked.
func openStateEntry(path string, access uint32) (windows.Handle, windows.ByHandleFileInformation, error) {
	var info windows.ByHandleFileInformation
	name, err := statePathName(path)
	if err != nil {
		return 0, info, err
	}
	h, err := windows.CreateFile(name, access|windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return 0, info, &os.PathError{Op: "open state path", Path: path, Err: err}
	}
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		windows.CloseHandle(h)
		return 0, info, &os.PathError{Op: "inspect state path", Path: path, Err: err}
	}
	return h, info, nil
}

// Win32 calls made directly do not get the extended-length paths that os uses.
func statePathName(path string) (*uint16, error) {
	full, err := windows.FullPath(path)
	if err != nil {
		return nil, err
	}
	if len(full) >= 248 &&
		!strings.HasPrefix(full, `\\?\`) && !strings.HasPrefix(full, `\??\`) && !strings.HasPrefix(full, `\\.\`) {
		if share, ok := strings.CutPrefix(full, `\\`); ok {
			full = `\\?\UNC\` + share
		} else {
			full = `\\?\` + full
		}
	}
	return windows.UTF16PtrFromString(full)
}
