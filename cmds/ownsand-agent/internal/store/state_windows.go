// SPDX-License-Identifier: FSL-1.1-ALv2

package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf16"
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
	return windows.UTF16PtrFromString(Win32StatePath(path))
}

// Win32StatePath adds the extended-length prefix when a state path needs it.
func Win32StatePath(path string) string {
	if len(path) >= 4 {
		switch filepath.ToSlash(path[:4]) {
		case "//?/", "/??/", "//./":
			return path
		}
	}
	if path == "" {
		return path
	}

	absolute := path
	if !filepath.IsAbs(path) {
		// Resolve just the base with Win32 so even a long relative tail
		// works. The base can be the current directory, a drive's current
		// directory, or the root of the current volume.
		base := filepath.VolumeName(path)
		tail := path[len(base):]
		if len(tail) > 0 && os.IsPathSeparator(tail[0]) {
			base += `\`
			tail = tail[1:]
		}
		base, err := filepath.Abs(base)
		if err != nil {
			return path
		}
		absolute = filepath.Join(base, tail)
	}
	// Without Win32 long-path opt-in, directory creation must leave space
	// for a 12-character 8.3 name. Use the same threshold for the log open.
	const longPathLimit = windows.MAX_PATH - 12
	if len(utf16.Encode([]rune(absolute))) < longPathLimit &&
		len(utf16.Encode([]rune(path))) < longPathLimit {
		return path
	}

	// Win32 does not resolve slashes, "." or ".." after the extended prefix.
	absolute = filepath.Clean(absolute)
	if tail, ok := strings.CutPrefix(absolute, `\\`); ok {
		return `\\?\UNC\` + tail
	}
	return `\\?\` + absolute
}
