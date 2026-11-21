// SPDX-License-Identifier: Apache-2.0

// Package fsutil copies directory trees and packs them into tar streams
// for snapshots.
package fsutil

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// CopyTree copies the regular files, directories and symbolic links of src
// into dst, which must not exist yet.
func CopyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
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
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		case info.Mode()&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case info.Mode().IsRegular():
			return CopyFile(path, target, info.Mode().Perm())
		}
		// Sockets, devices and pipes are not part of a snapshot.
		return nil
	})
}

// CopyFile copies one regular file.
func CopyFile(src, dst string, perm fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
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

// ReadTar extracts a tar stream into dir. It rejects names that leave dir
// and link targets that are absolute or leave dir. Every file system
// operation goes through an os.Root opened on dir, so no entry can read or
// write outside dir, including through symbolic links that the same archive
// created earlier; such an entry fails with an error.
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
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
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
			if err := f.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if filepath.IsAbs(hdr.Linkname) || !filepath.IsLocal(filepath.Join(filepath.Dir(name), hdr.Linkname)) || strings.Contains(hdr.Linkname, "\x00") {
				return fmt.Errorf("fsutil: tar symbolic link leaves the target directory")
			}
			if err := root.Symlink(hdr.Linkname, name); err != nil {
				return err
			}
		default:
			return fmt.Errorf("fsutil: unsupported tar entry type %d", hdr.Typeflag)
		}
	}
}
