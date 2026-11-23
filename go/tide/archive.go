// SPDX-License-Identifier: Apache-2.0

package tide

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// writeSyncArchive writes the files to w as one tar archive (PAX format,
// slash-separated relative names, owner 0). Regular files carry their mode
// (0644 or 0755); symbolic links are link entries unless skipLinks is set.
// The content of each file is read again and must still have the hashed
// size and SHA-256, so that the manifest describes what the guest
// receives. A file that changed since it was hashed fails the archive, and
// so the upload, before the guest can extract it.
func writeSyncArchive(ctx context.Context, w io.Writer, root string, files []localFile, skipLinks bool) error {
	tw := tar.NewWriter(w)
	dirs := map[string]bool{}
	now := time.Now()
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if f.entry.Mode == modeSymlink && skipLinks {
			continue
		}
		// Parent directories first, so that the guest tar creates them with
		// a usable mode.
		var parents []string
		for d := path.Dir(f.rel); d != "." && !dirs[d]; d = path.Dir(d) {
			parents = append(parents, d)
		}
		for i := len(parents) - 1; i >= 0; i-- {
			dirs[parents[i]] = true
			if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: parents[i] + "/", Mode: 0o755, ModTime: now, Format: tar.FormatPAX}); err != nil {
				return err
			}
		}
		if f.entry.Mode == modeSymlink {
			if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: f.rel, Linkname: f.target, Mode: 0o777, ModTime: now, Format: tar.FormatPAX}); err != nil {
				return err
			}
			continue
		}
		mode := int64(0o644)
		if f.entry.Mode == modeExecutable {
			mode = 0o755
		}
		file, err := os.Open(filepath.Join(root, filepath.FromSlash(f.rel)))
		if err != nil {
			return err
		}
		err = func() error {
			defer file.Close()
			if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: f.rel, Size: f.entry.Size, Mode: mode, ModTime: now, Format: tar.FormatPAX}); err != nil {
				return err
			}
			h := sha256.New()
			n, err := io.Copy(io.MultiWriter(tw, h), io.LimitReader(file, f.entry.Size))
			if err != nil {
				return err
			}
			changed := n != f.entry.Size || hex.EncodeToString(h.Sum(nil)) != f.entry.SHA256
			if !changed {
				// A file that grew has more bytes after the hashed size.
				var extra [1]byte
				m, err := file.Read(extra[:])
				if err != nil && !errors.Is(err, io.EOF) {
					return err
				}
				changed = m > 0
			}
			if changed {
				return fmt.Errorf("tide: %s changed during the sync; sync again", f.rel)
			}
			return nil
		}()
		if err != nil {
			return err
		}
	}
	return tw.Close()
}

// extractStats counts what an extraction wrote.
type extractStats struct {
	files    int
	bytes    int64
	warnings []string
}

// extractArchive extracts the tar archive r into the directory dir, which
// must exist and be empty. It rejects entries with absolute paths, entries
// that resolve outside dir, symbolic links whose targets point outside dir
// and hard links to paths outside the archive. After the last entry, it
// checks every symbolic link again with the other links of the tree
// resolved (verifyLinks). Each regular file may hold at
// most fileLimit bytes and all files together at most totalLimit bytes.
// Entry types other than directories, regular files, symbolic links and
// hard links are skipped with a warning.
func extractArchive(ctx context.Context, r io.Reader, dir string, fileLimit, totalLimit int64) (*extractStats, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	st := &extractStats{}
	regular := map[string]bool{}
	tr := tar.NewReader(r)
	for {
		if err := ctx.Err(); err != nil {
			return st, err
		}
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return st, verifyLinks(ctx, dir)
		}
		if err != nil {
			return st, err
		}
		name, ok, err := entryName(hdr.Name)
		if err != nil {
			return st, err
		}
		if !ok {
			continue // the root entry "./"
		}
		mode := fs.FileMode(hdr.Mode).Perm()
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, mode|0o700); err != nil {
				return st, err
			}
		case tar.TypeReg:
			if hdr.Size > fileLimit {
				return st, &LimitError{Operation: "download", FileLimit: fileLimit, TotalLimit: totalLimit, Paths: []PathSize{{Path: filepath.ToSlash(name), Size: hdr.Size}}}
			}
			if st.bytes+hdr.Size > totalLimit {
				return st, &LimitError{Operation: "download", FileLimit: fileLimit, TotalLimit: totalLimit, Total: st.bytes + hdr.Size}
			}
			if err := writeEntry(root, name, mode, tr, hdr.Size); err != nil {
				return st, err
			}
			regular[name] = true
			st.files++
			st.bytes += hdr.Size
		case tar.TypeSymlink:
			if !confinedLinkTarget(name, hdr.Linkname) {
				return st, fmt.Errorf("tide: the archive entry %s is a symbolic link that points outside the destination", filepath.ToSlash(name))
			}
			if err := root.MkdirAll(filepath.Dir(name), 0o755); err != nil {
				return st, err
			}
			_ = root.Remove(name)
			if err := root.Symlink(filepath.FromSlash(hdr.Linkname), name); err != nil {
				// A Windows account without the symbolic link privilege
				// cannot create links; the download keeps the rest.
				st.warnings = append(st.warnings, fmt.Sprintf("%s: the symbolic link could not be created", filepath.ToSlash(name)))
				continue
			}
			st.files++
		case tar.TypeLink:
			target, ok, err := entryName(hdr.Linkname)
			if err != nil || !ok || !regular[target] {
				return st, fmt.Errorf("tide: the archive entry %s is a hard link to a path outside the archive", filepath.ToSlash(name))
			}
			if err := root.MkdirAll(filepath.Dir(name), 0o755); err != nil {
				return st, err
			}
			_ = root.Remove(name)
			if err := root.Link(target, name); err != nil {
				if err := copyInRoot(root, target, name); err != nil {
					return st, err
				}
			}
			regular[name] = true
			st.files++
		default:
			st.warnings = append(st.warnings, fmt.Sprintf("%s: skipped an archive entry of an unsupported type", filepath.ToSlash(name)))
		}
	}
}

// entryName returns the local form of a tar entry name. ok is false for the
// root entry. An absolute name or one that leaves the root is an error.
func entryName(n string) (string, bool, error) {
	clean := strings.TrimPrefix(n, "./")
	clean = strings.TrimSuffix(clean, "/")
	if clean == "" || clean == "." {
		return "", false, nil
	}
	if strings.HasPrefix(n, "/") || strings.HasPrefix(n, `\`) || (len(n) >= 2 && n[1] == ':') {
		return "", false, fmt.Errorf("tide: the archive entry %q has an absolute path", n)
	}
	local := filepath.FromSlash(clean)
	if !filepath.IsLocal(local) {
		return "", false, fmt.Errorf("tide: the archive entry %q leaves the destination", n)
	}
	return local, true, nil
}

// writeEntry writes one regular file inside root, replacing an existing
// entry of the same name.
func writeEntry(root *os.Root, name string, mode fs.FileMode, r io.Reader, size int64) error {
	if err := root.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		return err
	}
	_ = root.Remove(name)
	f, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode|0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(r, size))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n != size {
		err = io.ErrUnexpectedEOF
	}
	return err
}

func copyInRoot(root *os.Root, from, to string) error {
	src, err := root.Open(from)
	if err != nil {
		return err
	}
	defer src.Close()
	info, err := src.Stat()
	if err != nil {
		return err
	}
	return writeEntry(root, to, info.Mode().Perm(), src, info.Size())
}

// confinedLinkTarget reports whether the symbolic link target linkname of
// the entry name (a local path) stays inside the extraction root: it must be
// relative under the local path rules, without a volume name or a leading
// separator, and the target joined with the directory of the entry must be
// local, so ".." elements cannot climb out of the root.
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

// maxLinkHops bounds the symbolic links that verifyLinks follows for one
// path. A longer chain is rejected; operating systems stop much earlier
// (Linux after 40 links).
const maxLinkHops = 255

// verifyLinks checks every symbolic link in the extracted tree dir: its
// target, resolved from the directory where the link really is and through
// the other links of the tree, must stay inside dir. confinedLinkTarget
// checks a target only as text, and an entry name or a target that passes
// through another link can point elsewhere: with a/b -> .., the link
// a/b/l -> ../x is the link l in dir, whose target leaves dir.
func verifyLinks(ctx context.Context, dir string) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink == 0 {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		ok, err := resolvesInside(dir, rel)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("tide: the archive entry %s is a symbolic link that resolves outside the destination", filepath.ToSlash(rel))
		}
		return nil
	})
}

// resolvesInside resolves the local path rel inside dir like the operating
// system does: element by element, following each symbolic link. It reports
// whether every step stays inside dir. After an element that does not
// exist, the rest of the path is checked as text.
func resolvesInside(dir, rel string) (bool, error) {
	pending := splitPath(rel)
	var cur []string
	missing := false
	hops := 0
	for len(pending) > 0 {
		el := pending[0]
		pending = pending[1:]
		switch el {
		case "", ".":
			continue
		case "..":
			if len(cur) == 0 {
				return false, nil
			}
			cur = cur[:len(cur)-1]
			continue
		}
		cur = append(cur, el)
		if missing {
			continue
		}
		full := filepath.Join(append([]string{dir}, cur...)...)
		info, err := os.Lstat(full)
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			missing = true
			continue
		}
		if err != nil {
			return false, err
		}
		if info.Mode()&fs.ModeSymlink == 0 {
			continue
		}
		if hops++; hops > maxLinkHops {
			return false, nil
		}
		target, err := os.Readlink(full)
		if err != nil {
			return false, err
		}
		if target == "" || filepath.IsAbs(target) || filepath.VolumeName(target) != "" || os.IsPathSeparator(target[0]) {
			return false, nil
		}
		// The target replaces the link, relative to the link's directory.
		// It is not cleaned: a ".." after a link leaves the link's target,
		// not the link.
		cur = cur[:len(cur)-1]
		pending = append(splitPath(target), pending...)
	}
	return true, nil
}

// splitPath splits a local path at every separator of the local system.
func splitPath(p string) []string {
	return strings.FieldsFunc(p, func(r rune) bool { return r < 0x80 && os.IsPathSeparator(uint8(r)) })
}
