// SPDX-License-Identifier: Apache-2.0

package sandrun

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"syscall"
)

func finishArchiveLinks(ctx context.Context, root *os.Root, st *extractStats) error {
	return repairArchiveDirectoryLinks(ctx, root, st, (*os.Root).Symlink)
}

// Windows chooses the link type when it creates the link. A missing directory
// target can therefore leave a file symlink after the archive has been read.
func repairArchiveDirectoryLinks(ctx context.Context, root *os.Root, st *extractStats, symlink func(*os.Root, string, string) error) error {
	type repair struct {
		name string
		hops int
	}
	var repairs []repair
	err := verifyLinks(ctx, root.Name(), func(name string, target resolvedArchiveLink) error {
		if target.path == "" {
			return nil
		}
		info, err := root.Lstat(name)
		if err != nil {
			return err
		}
		if isWindowsDirectoryLink(info) {
			return nil
		}
		info, err = root.Lstat(target.path)
		if err != nil {
			return err
		}
		if info.IsDir() {
			repairs = append(repairs, repair{name: name, hops: target.hops})
		}
		return nil
	})
	if err != nil || len(repairs) == 0 {
		return err
	}

	// A link's dependencies have fewer hops. Fix them first so Symlink
	// can see the directory through the original relative target.
	slices.SortStableFunc(repairs, func(a, b repair) int { return cmp.Compare(a.hops, b.hops) })
	for _, r := range repairs {
		if err := ctx.Err(); err != nil {
			return err
		}
		target, err := root.Readlink(r.name)
		if err != nil {
			return err
		}
		if err := root.Remove(r.name); err != nil {
			return fmt.Errorf("sandrun: remove symbolic link %s: %w", filepath.ToSlash(r.name), err)
		}
		if err := symlink(root, target, r.name); err == nil {
			info, err := root.Lstat(r.name)
			if err == nil && isWindowsDirectoryLink(info) {
				continue
			}
		}
		// Creation can fail or still create a file link, for example if
		// a target link had to be omitted. Remove any entry it left behind.
		if err := root.Remove(r.name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("sandrun: remove unusable symbolic link %s: %w", filepath.ToSlash(r.name), err)
		}
		st.files--
		st.warnSymlink(r.name)
	}
	return verifyLinks(ctx, root.Name(), nil)
}

func isWindowsDirectoryLink(info fs.FileInfo) bool {
	// Go's FileInfo.IsDir is false for a directory symlink.
	data, ok := info.Sys().(*syscall.Win32FileAttributeData)
	return info.Mode()&fs.ModeSymlink != 0 && ok && data.FileAttributes&syscall.FILE_ATTRIBUTE_DIRECTORY != 0
}
