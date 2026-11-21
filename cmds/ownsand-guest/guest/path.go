// SPDX-License-Identifier: FSL-1.1-ALv2

package guest

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// nativePath returns the path string that the guest hands to the operating
// system and compares for read_id and write_id bindings.
//
// POSIX guests pass the absolute path exactly as received. A lexical clean
// would turn "<dir>/link/../file" into "<dir>/file", but the operating
// system resolves ".." after it follows the symlink "link", so the clean
// path names a different file.
//
// Workaround scope: Windows guests keep the lexical clean that the helper
// always applied. Windows path semantics are outside the scope of the
// exact-path rule; remove this guard when the Windows guest path contract
// defines its own rules.
func nativePath(path string) string {
	if runtime.GOOS == "windows" {
		return filepath.Clean(path)
	}
	return path
}

// splitTarget splits a write target into its directory part and its final
// element without a lexical clean, so that a ".." in the directory part is
// still resolved by the operating system. The directory part keeps its
// trailing separator on POSIX guests (so "/file" gives "/"), and trailing
// separators of the target are not part of the final element. The final
// element is empty, "." or ".." when the path names a directory.
func splitTarget(path string) (dir, base string) {
	path = nativePath(path)
	if runtime.GOOS == "windows" {
		return filepath.Dir(path), filepath.Base(path)
	}
	trimmed := strings.TrimRight(path, "/")
	i := strings.LastIndexByte(trimmed, '/')
	return trimmed[:i+1], trimmed[i+1:]
}

// siblingPath returns the path of a file called name in dir, built as text so
// that a ".." in dir is not cleaned away.
func siblingPath(dir, name string) string {
	if dir == "" || os.IsPathSeparator(dir[len(dir)-1]) {
		return dir + name
	}
	return dir + string(os.PathSeparator) + name
}

// isDirectoryName reports whether the final element of a path names a
// directory and so can never name a file.
func isDirectoryName(base string) bool {
	return base == "" || base == "." || base == ".."
}
