// SPDX-License-Identifier: Apache-2.0

package sandrun

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
				return fmt.Errorf("sandrun: %s changed during the sync; sync again", f.rel)
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

type archiveExtractor struct {
	fileLimit  int64
	totalLimit int64
	link       func(*os.Root, string, string) error
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
func (e archiveExtractor) extractArchive(ctx context.Context, r io.Reader, dir string) (*extractStats, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	st := &extractStats{}
	regular := map[string]bool{}
	fileNames := map[string][]archiveFileName{}
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
			key := foldCase(name)
			names := fileNames[key]
			if err := e.writeFile(st, name, hdr.Size, func() error {
				if err := checkArchiveCaseCollision(root, name, hdr.Name, names); err != nil {
					return err
				}
				return writeEntry(root, name, mode, tr, hdr.Size)
			}); err != nil {
				return st, err
			}
			fileNames[key] = rememberArchiveFileName(names, name, hdr.Name)
			regular[name] = true
			st.files++
		case tar.TypeSymlink:
			if !confinedLinkTarget(name, hdr.Linkname) {
				return st, fmt.Errorf("sandrun: the archive entry %s is a symbolic link that points outside the destination", filepath.ToSlash(name))
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
				return st, fmt.Errorf("sandrun: the archive entry %s is a hard link to a path outside the archive", filepath.ToSlash(name))
			}
			if err := root.MkdirAll(filepath.Dir(name), 0o755); err != nil {
				return st, err
			}
			_ = root.Remove(name)
			if err := e.link(root, target, name); err != nil {
				if err := e.copyFile(root, st, target, name); err != nil {
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

type archiveFileName struct {
	local   string
	archive string
}

func rememberArchiveFileName(names []archiveFileName, local, archive string) []archiveFileName {
	for i, name := range names {
		if name.local == local {
			names[i].archive = archive
			return names
		}
	}
	return append(names, archiveFileName{local: local, archive: archive})
}

// A case-folded match is only a candidate. The two archive paths must
// resolve to the same entry on the staging filesystem.
func checkArchiveCaseCollision(root *os.Root, name, archivePath string, names []archiveFileName) error {
	if len(names) == 0 {
		return nil
	}
	for _, previous := range names {
		if previous.local == name {
			return nil // Repeated entries with the same name may replace one another.
		}
	}

	info, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return nil
	}

	for _, previous := range names {
		other, err := root.Lstat(previous.local)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !os.SameFile(info, other) {
			continue
		}
		alias, err := sameArchiveDirectoryEntry(root, name, previous.local)
		if err != nil {
			return err
		}
		if alias {
			return &archiveCaseCollision{earlier: previous.archive, later: archivePath}
		}
	}
	return nil
}

// A hard link can share the file identity of a different directory entry.
func sameArchiveDirectoryEntry(root *os.Root, a, b string) (bool, error) {
	dir := filepath.Dir(a)
	if otherDir := filepath.Dir(b); dir != otherDir {
		parent, err := root.Stat(dir)
		if err != nil {
			return false, err
		}
		otherParent, err := root.Stat(otherDir)
		if err != nil {
			return false, err
		}
		if !os.SameFile(parent, otherParent) {
			return false, nil
		}
	}

	first, second := filepath.Base(a), filepath.Base(b)
	if first == second {
		return true, nil
	}
	directory, err := root.Open(dir)
	if err != nil {
		return false, err
	}
	defer directory.Close()
	entries, err := directory.ReadDir(-1)
	if err != nil {
		return false, err
	}
	var firstExists, secondExists bool
	for _, entry := range entries {
		firstExists = firstExists || entry.Name() == first
		secondExists = secondExists || entry.Name() == second
	}
	return !firstExists || !secondExists, nil
}

type archiveCaseCollision struct {
	earlier string
	later   string
}

func (e *archiveCaseCollision) Error() string {
	return fmt.Sprintf("sandrun: the archive entries %q and %q conflict on the destination filesystem", e.earlier, e.later)
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
		return "", false, fmt.Errorf("sandrun: the archive entry %q has an absolute path", n)
	}
	local := filepath.FromSlash(clean)
	if !filepath.IsLocal(local) {
		return "", false, fmt.Errorf("sandrun: the archive entry %q leaves the destination", n)
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

func (e archiveExtractor) writeFile(st *extractStats, name string, size int64, write func() error) error {
	if size > e.fileLimit {
		return &LimitError{Operation: "download", FileLimit: e.fileLimit, TotalLimit: e.totalLimit, Paths: []PathSize{{Path: filepath.ToSlash(name), Size: size}}}
	}
	if st.bytes+size > e.totalLimit {
		return &LimitError{Operation: "download", FileLimit: e.fileLimit, TotalLimit: e.totalLimit, Total: st.bytes + size}
	}
	if err := write(); err != nil {
		return err
	}
	st.bytes += size
	return nil
}

func (e archiveExtractor) copyFile(root *os.Root, st *extractStats, from, to string) error {
	src, err := root.Open(from)
	if err != nil {
		return err
	}
	defer src.Close()
	info, err := src.Stat()
	if err != nil {
		return err
	}
	return e.writeFile(st, to, info.Size(), func() error {
		return writeEntry(root, to, info.Mode().Perm(), src, info.Size())
	})
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
			return fmt.Errorf("sandrun: the archive entry %s is a symbolic link that resolves outside the destination", filepath.ToSlash(rel))
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
