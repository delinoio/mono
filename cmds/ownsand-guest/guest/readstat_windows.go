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
// MAX_PATH of 260 minus the 12 bytes of an 8.3 file name. statSource uses the
// same limit, so that it and the os.Open that follows it name the file in the
// same way.
const longPathLimit = 248

func isPathSeparator(c byte) bool { return c == '\\' || c == '/' }

// extendedPath returns the form of path that syscall.CreateFile accepts when
// path is longer than the Win32 path limit.
//
// os.Open and os.Stat add the extended-length prefix on their own, but
// syscall.CreateFile does not, so a path of 248 bytes or more that works with
// os.Open fails in statSource. extendedPath cleans a long path, and adds the
// \\?\ prefix to a drive path or the \\?\UNC\ prefix to a UNC path. It
// returns a short path, a path that has the extended-length prefix (\\?\ or
// \??\) and a device path (\\.\) unchanged: the operating system does not
// normalize an extended-length path, and a prefix on a device path changes its
// meaning. A relative path is made absolute first, as os.Open does, and keeps
// its form when the absolute path is short.
//
// The result is only for the call that opens the file. Errors and the
// *os.File keep the path that the caller passed.
func extendedPath(path string) string {
	if len(path) >= 4 {
		switch {
		case path[:4] == `\??\`:
			return path
		case isPathSeparator(path[0]) && isPathSeparator(path[1]) && path[2] == '?' && isPathSeparator(path[3]):
			return path
		case isPathSeparator(path[0]) && isPathSeparator(path[1]) && path[2] == '.' && isPathSeparator(path[3]):
			return path
		}
	}
	abs := path
	if !filepath.IsAbs(abs) {
		a, err := filepath.Abs(abs)
		if err != nil {
			return path
		}
		abs = a
	}
	if len(abs) < longPathLimit {
		return path
	}
	// The operating system does not resolve "." and ".." or a "/" in an
	// extended-length path, so clean the path before it gets the prefix.
	clean := filepath.Clean(abs)
	if strings.HasPrefix(clean, `\\`) {
		return `\\?\UNC\` + clean[2:]
	}
	return `\\?\` + clean
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
// converts it with extendedPath. Without that, the stat of a path that the
// open below accepts would fail.
func statSource(path string) (os.FileInfo, error) {
	// The prefix is only for CreateFile. The errors and the file below keep
	// the path that the caller passed.
	p, err := syscall.UTF16PtrFromString(extendedPath(path))
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
