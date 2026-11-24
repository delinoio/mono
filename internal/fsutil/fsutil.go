// SPDX-License-Identifier: Apache-2.0

// Package fsutil copies directory trees, removes them again and packs them
// into tar streams for snapshots.
package fsutil

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
)

// CopyTree copies the regular files, directories and symbolic links of src
// into dst, which must not exist yet. Every copied file and directory ends
// with the permission bits of its source, whatever the umask of the process
// is; see CopyTreeContext.
func CopyTree(src, dst string) error {
	return CopyTreeContext(context.Background(), src, dst)
}

// dirMode holds the final permission bits to apply to a directory.
type dirMode struct {
	path string
	perm fs.FileMode
}

// CopyTreeContext is CopyTree that stops with the context error before each
// entry and between the chunks of a file once ctx is done. The entries that it
// copied before the stop stay in dst; the caller removes them.
//
// The permission bits of a copied file are those of its source: CopyFile sets
// them explicitly, so the umask does not clear any. A directory is created
// owner-writable, so that its entries can be copied into it, and receives its
// source permission bits only after the whole tree is copied, deepest
// directories first; a read-only source directory therefore copies with its
// content and ends read-only. A copy that fails or stops early skips this
// step and leaves the directories that it created owner-writable, so that the
// caller's removal of the partial copy does not meet a read-only directory.
//
// On Windows a directory keeps the mode that the create call gives it: the
// read-only attribute of a directory has no effect on its entries there.
func CopyTreeContext(ctx context.Context, src, dst string) error {
	var dirs []dirMode
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			if err := os.MkdirAll(target, info.Mode().Perm()|0o700); err != nil {
				return err
			}
			dirs = append(dirs, dirMode{path: target, perm: info.Mode().Perm()})
			return nil
		case info.Mode()&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case info.Mode().IsRegular():
			return copyFile(ctx, path, target, info.Mode().Perm())
		}
		// Sockets, devices and pipes are not part of a snapshot.
		return nil
	})
	if err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		return nil
	}
	// The walk visits a directory before its entries, so the reverse order
	// changes the mode of a directory after the modes of its entries.
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := os.Chmod(dirs[i].path, dirs[i].perm); err != nil {
			return err
		}
	}
	return nil
}

// CopyFile copies one regular file. The copy has the permission bits perm
// whatever the umask of the process is.
func CopyFile(src, dst string, perm fs.FileMode) error {
	return copyFile(context.Background(), src, dst, perm)
}

// copyFile is CopyFile that stops with the context error at the next chunk
// once ctx is done.
func copyFile(ctx context.Context, src, dst string, perm fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	if _, err := CopyContext(ctx, out, in); err != nil {
		out.Close()
		return err
	}
	// OpenFile applies the umask to perm, so set the mode explicitly. A file
	// that is not owner-writable has been written already, so this is the
	// first point at which it can become read-only.
	if err := out.Chmod(perm); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// RemoveAll removes path and the tree below it like os.RemoveAll. A tree that
// CopyTree or ReadTar restored with read-only directory modes can contain
// entries that an unprivileged process cannot delete. When the first attempt
// fails with a permission error, RemoveAll grants the owner read, write and
// search access on every directory of the tree (symbolic links are not followed) and removes
// the tree again. It returns the error of that second attempt, so a tree that
// stays undeletable (for example because its parent directory is read-only)
// is still reported. A missing path is not an error.
//
// Workaround: Go 1.26 os.RemoveAll can return an error that wraps the
// unexported os.errSymlink when it cannot unlink a symbolic link inside a
// read-only directory. The Error method of that value panics ("errSymlink is
// not user-visible"), so printing the error crashes the caller. The cause is
// a permission failure, so RemoveAll treats such an error like one, and it
// never returns an error that cannot be printed. Remove this handling when
// the minimum Go version includes the fix for Go issue 78490.
func RemoveAll(path string) error {
	err := os.RemoveAll(path)
	if err == nil || !(errors.Is(err, fs.ErrPermission) || !printable(err)) {
		return err
	}
	makeDirsWritable(path)
	err = os.RemoveAll(path)
	if err != nil && !printable(err) {
		return &fs.PathError{Op: "remove", Path: path, Err: fs.ErrPermission}
	}
	return err
}

// printable reports whether err.Error() returns instead of panicking.
func printable(err error) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	_ = err.Error()
	return true
}

// makeDirsWritable adds the owner read, write and search bits to every
// directory at or below path. It ignores errors: the removal that follows
// reports whatever still blocks it.
func makeDirsWritable(path string) {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() {
		return
	}
	const need = 0o700
	if info.Mode().Perm()&need != need {
		_ = os.Chmod(path, info.Mode().Perm()|need)
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			makeDirsWritable(filepath.Join(path, e.Name()))
		}
	}
}

// TreeSize returns the total size of the regular files under dir.
func TreeSize(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			total += info.Size()
		}
		return nil
	})
	return total, err
}

// ctxReader fails a read with the context error once ctx is done, so a copy
// through it stops at the next chunk boundary instead of at the end of the
// stream.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// CopyContext copies src to dst like io.Copy but stops with the context
// error at the next chunk once ctx is done.
func CopyContext(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	return io.Copy(dst, ctxReader{ctx: ctx, r: src})
}

// WriteTar writes the tree under dir to w as a tar stream with relative
// names. It keeps regular files, directories and symbolic links.
func WriteTar(w io.Writer, dir string) error {
	return WriteTarContext(context.Background(), w, dir)
}

// WriteTarContext is WriteTar that stops with the context error between
// entries and between the chunks of a file once ctx is done. The stream that
// it wrote before the stop is incomplete and must be discarded.
func WriteTarContext(ctx context.Context, w io.Writer, dir string) error {
	tw := tar.NewWriter(w)
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil || rel == "." {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		var link string
		if info.Mode()&fs.ModeSymlink != 0 {
			if link, err = os.Readlink(path); err != nil {
				return err
			}
		} else if !info.Mode().IsRegular() && !info.IsDir() {
			return nil
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		hdr.Uname, hdr.Gname = "", ""
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			_, err = CopyContext(ctx, tw, f)
			f.Close()
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

// ReadTar extracts a tar stream into dir. It rejects names that are not
// local paths under the host rules and link targets that are absolute,
// start at a root (such as "/etc" on Windows), have a volume name or leave
// dir. Every file system
// operation goes through an os.Root opened on dir, so no entry can read or
// write outside dir, including through symbolic links that the same archive
// created earlier; such an entry fails with an error.
//
// The lexical link target check cannot see a link that resolves outside dir
// only because an earlier or later link of the same archive stands for a
// directory component, such as "a" -> "." followed by "b" -> "a/..". After
// the last entry, ReadTar therefore resolves every symbolic link that it
// created through the os.Root again and fails when one leaves dir. A link
// that resolves to nothing stays allowed.
//
// On Unix, regular files and explicitly archived directories receive the
// ordinary permission bits from their headers, regardless of the umask.
// Directory modes are applied after extraction and link validation.
func ReadTar(r io.Reader, dir string) error {
	return ReadTarContext(context.Background(), r, dir)
}

// ReadTarContext is ReadTar that stops with the context error between
// entries and between the chunks of a file once ctx is done. The files that
// it extracted before the stop stay in dir; the caller removes them.
func ReadTarContext(ctx context.Context, r io.Reader, dir string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	tr := tar.NewReader(ctxReader{ctx: ctx, r: r})
	// links holds the names of the symbolic links that the archive created.
	// A later entry can turn a path component of an earlier link into
	// another link, so the links are resolved once, after the last entry.
	var links []string
	dirs := make(map[string]fs.FileMode)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		name := filepath.FromSlash(hdr.Name)
		if !filepath.IsLocal(name) {
			return fmt.Errorf("fsutil: tar entry leaves the target directory")
		}
		mode := fs.FileMode(hdr.Mode).Perm()
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, mode|0o700); err != nil {
				return err
			}
			if runtime.GOOS != "windows" {
				// A repeated directory header supplies the final mode.
				dirs[strings.TrimRight(name, string(filepath.Separator))] = mode
			}
		case tar.TypeReg:
			if err := root.MkdirAll(filepath.Dir(name), 0o700); err != nil {
				return err
			}
			f, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			if runtime.GOOS != "windows" {
				if err := f.Chmod(mode); err != nil {
					f.Close()
					return err
				}
			}
			if err := f.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if !confinedLinkTarget(name, hdr.Linkname) {
				return fmt.Errorf("fsutil: tar symbolic link leaves the target directory")
			}
			// Workaround: on Windows, os.Root.Symlink in Go 1.26 writes the
			// target unchanged, and Windows does not resolve `/` in link
			// contents, so a tar target such as `../real/f` does not resolve.
			// os.Symlink converts the separators, and Root.Symlink does too
			// from Go 1.27. FromSlash changes nothing on Linux and macOS.
			// Remove the conversion when go.mod requires Go 1.27 or later.
			if err := root.Symlink(filepath.FromSlash(hdr.Linkname), name); err != nil {
				return err
			}
			links = append(links, name)
		default:
			return fmt.Errorf("fsutil: unsupported tar entry type %d", hdr.Typeflag)
		}
	}
	if err := checkLinksConfined(root, links); err != nil {
		return err
	}
	// A tar can list a directory after its children. Apply the deepest
	// modes first so that every parent stays searchable until its children
	// have their final modes.
	names := slices.SortedFunc(maps.Keys(dirs), func(a, b string) int {
		return tarDirectoryDepth(b) - tarDirectoryDepth(a)
	})
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := setTarDirectoryMode(root, name, dirs[name]); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func tarDirectoryDepth(name string) int {
	name = filepath.Clean(name)
	if name == "." {
		return 0
	}
	return 1 + strings.Count(name, string(filepath.Separator))
}

func setTarDirectoryMode(root *os.Root, name string, mode fs.FileMode) error {
	// Open within the extraction root and chmod the handle. A path-based
	// chmod can follow a symlink that replaces the final path component.
	f, err := root.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("fsutil: tar directory %q is no longer a directory", name)
	}
	return f.Chmod(mode)
}

// checkLinksConfined resolves each symbolic link in links through root and
// fails when the resolution leaves root or does not finish.
//
// confinedLinkTarget cleans ".." elements lexically, as if every component
// of the target were a real directory. A component can be a link that the
// same archive created, and the file system then resolves ".." from the
// link's destination, so a target such as "a/.." with "a" -> "." names the
// parent of root. os.Root refuses to resolve a path through such a link
// ("path escapes from parent"), and it reports ELOOP for a chain that is too
// deep. A link that resolves to nothing, or through a file that is not a
// directory, is dangling and stays allowed.
func checkLinksConfined(root *os.Root, links []string) error {
	for _, name := range links {
		err := recoverStat(func() (fs.FileInfo, error) { return root.Stat(name) })
		if err == nil || errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			continue
		}
		if errors.Is(err, errRootStatPanic) {
			return fmt.Errorf("fsutil: tar symbolic link %q cannot be confirmed inside the target directory: %w", name, err)
		}
		return fmt.Errorf("fsutil: tar symbolic link %q resolves outside the target directory: %w", name, err)
	}
	return nil
}

// errRootStatPanic marks a panic of os.Root.Stat that recoverStat turned
// into an error.
var errRootStatPanic = errors.New("os.Root.Stat panicked")

// recoverStat calls stat, which is os.Root.Stat on one link, and returns a
// panic of it as an error that wraps errRootStatPanic.
//
// Workaround: on Windows, Go 1.26.1 os.Root resolves a symbolic link with
// rootCleanPath, which cleans the prefix, the link target and the rest of
// the path lexically. When that result is the path that the walk already
// opened up to the link, such as "d/x" -> "up/.." or "d/self" -> ".", the new
// path has no more elements than the walk index, and doInRoot (os/root_openat.go)
// panics with "index out of range" instead of returning an error. ReadTar
// must not crash on archive content, so the panic becomes an error, and
// checkLinksConfined then fails the archive: it fails closed, which also
// rejects such a link that stays inside the root. The panic happens only on
// Windows; on other systems recoverStat returns the result of stat
// unchanged. Remove recoverStat when go.mod requires a Go version in which
// os.Root.Stat returns a result or an error for such a link on Windows. The
// doInRoot source of Go 1.27.4 still compares len(newparts) < i without the
// equal case.
func recoverStat(stat func() (fs.FileInfo, error)) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%w: %v", errRootStatPanic, r)
		}
	}()
	_, err = stat()
	return err
}

// confinedLinkTarget reports whether the symbolic link target linkname of
// the entry name, which is a local path in host form, stays inside the
// extraction root.
//
// The target must be relative under the host path rules. A joined path
// cannot be used for this check alone: filepath.Join drops the leading
// separator of a root-relative target such as "/etc" or `\etc` on
// Windows, where filepath.IsAbs reports false for it because it has no
// volume name, and Windows resolves it against the root of the current
// drive. A volume name ("C:x", `C:\x`, `\\server\share`) also makes the
// target leave the root. After these checks, the target joined with the
// directory of the entry must be local, so ".." elements cannot climb out
// of the root.
//
// The rules are the host rules: on Linux and macOS, a backslash and a colon
// are ordinary file name characters, so a target such as `\x` or "C:x"
// names a file in the directory of the link there.
func confinedLinkTarget(name, linkname string) bool {
	if linkname == "" || strings.Contains(linkname, "\x00") {
		return false
	}
	target := filepath.FromSlash(linkname)
	if filepath.IsAbs(target) || filepath.VolumeName(target) != "" || os.IsPathSeparator(target[0]) {
		return false
	}
	return filepath.IsLocal(filepath.Join(filepath.Dir(name), target))
}
