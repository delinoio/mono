// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build windows

package guest

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// longPathLimit is the length from which os.Open (fixLongPath in the standard
// library) switches a path to the extended-length form. It is the Windows
// MAX_PATH of 260 minus the 12 bytes of an 8.3 file name.
const longPathLimit = 248

func isPathSeparator(c byte) bool { return c == '\\' || c == '/' }

// absoluteLongPath returns the absolute form of an ordinary path long enough
// to need an extended-length prefix. Explicit extended-length and device paths
// already specify how Windows should interpret them.
func absoluteLongPath(path string) (string, bool) {
	if len(path) >= 4 {
		switch {
		case path[:4] == `\??\`:
			return "", false
		case isPathSeparator(path[0]) && isPathSeparator(path[1]) && path[2] == '?' && isPathSeparator(path[3]):
			return "", false
		case isPathSeparator(path[0]) && isPathSeparator(path[1]) && path[2] == '.' && isPathSeparator(path[3]):
			return "", false
		}
	}
	abs := path
	if !filepath.IsAbs(abs) {
		a, err := filepath.Abs(abs)
		if err != nil {
			return "", false
		}
		abs = a
	}
	return abs, len(abs) >= longPathLimit
}

// extendedPath preserves the lexical conversion used by write recovery to
// identify a staging path.
func extendedPath(path string) string {
	abs, ok := absoluteLongPath(path)
	if !ok {
		return path
	}
	return prefixLongPath(filepath.Clean(abs))
}

// extendedReadPath uses Win32 normalization before adding a prefix that
// disables it. This matches the name that os.Open resolves, including when
// the last component ends in a period or an ASCII space.
func extendedReadPath(path string) string {
	abs, ok := absoluteLongPath(path)
	if !ok {
		return path
	}
	full, err := syscall.FullPath(abs)
	if err != nil {
		return path
	}
	return prefixLongPath(full)
}

func prefixLongPath(path string) string {
	if strings.HasPrefix(path, `\\`) {
		return `\\?\UNC\` + path[2:]
	}
	return `\\?\` + path
}

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
//
// CreateFile does not convert a long path as os.Open does, so statSource
// converts it with extendedReadPath.
func statSource(path string) (os.FileInfo, error) {
	// The prefix is only for CreateFile. The errors and the file below keep
	// the path that the caller passed.
	p, err := syscall.UTF16PtrFromString(extendedReadPath(path))
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
