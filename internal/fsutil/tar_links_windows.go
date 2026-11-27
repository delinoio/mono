// SPDX-License-Identifier: Apache-2.0

package fsutil

import (
	"context"
	"fmt"
	"io/fs"
	"syscall"
)

type tarDirectoryLinkRoot interface {
	Lstat(string) (fs.FileInfo, error)
	Readlink(string) (string, error)
	Remove(string) error
	Symlink(string, string) error
}

// repairTarDirectoryLinks fixes file-form links to directories. Windows sets
// the link's directory attribute at creation time. os.Root.Symlink infers it
// from the target, which may have been missing when the link was first created.
func repairTarDirectoryLinks(ctx context.Context, root tarDirectoryLinkRoot, names []string) (bool, error) {
	repaired := false
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		isDirectory, err := tarLinkDirectoryAttribute(root, name)
		if err != nil {
			return false, err
		}
		if isDirectory {
			continue
		}
		target, err := root.Readlink(name)
		if err != nil {
			return false, err
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if err := root.Remove(name); err != nil {
			return false, err
		}
		if err := root.Symlink(target, name); err != nil {
			return false, err
		}
		isDirectory, err = tarLinkDirectoryAttribute(root, name)
		if err != nil {
			return false, err
		}
		if !isDirectory {
			return false, fmt.Errorf("fsutil: tar symbolic link %q still lacks the directory attribute after recreation", name)
		}
		repaired = true
	}
	return repaired, nil
}

func tarLinkDirectoryAttribute(root tarDirectoryLinkRoot, name string) (bool, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return false, err
	}
	attrs := info.Sys().(*syscall.Win32FileAttributeData)
	return attrs.FileAttributes&syscall.FILE_ATTRIBUTE_DIRECTORY != 0, nil
}
