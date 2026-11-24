// SPDX-License-Identifier: Apache-2.0

package tide

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"connectrpc.com/connect"
)

// Manifest modes. They use the Git file modes, so a manifest entry tells a
// regular file, an executable file and a symbolic link apart.
const (
	modeFile       uint32 = 0o100644
	modeExecutable uint32 = 0o100755
	modeSymlink    uint32 = 0o120000
	modeGitlink           = "160000"
)

// manifestVersion is the version of the guest manifest format.
const manifestVersion = 1

// manifest is the guest sync manifest <workspace_path>.tide-sync.json.
type manifest struct {
	Version int                      `json:"version"`
	Files   map[string]manifestEntry `json:"files"`
	// PathsValidated records that the SDK validated the UTF-8 of each path.
	PathsValidated bool `json:"paths_validated,omitempty"`
}

// manifestEntry is one synced path. SHA256 is the hash of the file content,
// or of the link target of a symbolic link.
type manifestEntry struct {
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	Mode   uint32 `json:"mode"`
}

// parseManifest returns the manifest in b, or an empty manifest when b is
// not a valid manifest: an unreadable manifest counts as empty.
func parseManifest(b []byte) manifest {
	var m manifest
	if err := json.Unmarshal(b, &m); err != nil || m.Version != manifestVersion || m.Files == nil {
		return manifest{Version: manifestVersion, Files: map[string]manifestEntry{}}
	}
	for p := range m.Files {
		if !validRelPath(p) {
			delete(m.Files, p)
		}
	}
	return m
}

// localFile is one file of the local file list.
type localFile struct {
	rel    string // slash-separated path relative to the sync root
	entry  manifestEntry
	target string // link target of a symbolic link, slash-separated
}

// localList is the result of the local file selection.
type localList struct {
	files []localFile
	// skipped lists submodules and nested repositories, which sync does
	// not enter.
	skipped []string
}

// listLocalFiles selects the local files of a sync: the output of
// `git ls-files --cached --others --exclude-standard` in dir (tracked
// files and untracked files that are not ignored), without submodules.
// It reads each file and hashes its content.
func listLocalFiles(ctx context.Context, dir string) (*localList, error) {
	if _, err := exec.LookPath("git"); err != nil {
		return nil, localError(connect.CodeFailedPrecondition, errors.New("tide: sync needs git on the local machine"))
	}
	st, err := os.Stat(dir)
	if err != nil {
		return nil, localError(connect.CodeInvalidArgument, fmt.Errorf("tide: local directory: %w", err))
	}
	if !st.IsDir() {
		return nil, localError(connect.CodeInvalidArgument, fmt.Errorf("tide: %s is not a directory", dir))
	}
	inside, err := runGit(ctx, dir, "rev-parse", "--is-inside-work-tree")
	if err != nil || strings.TrimSpace(string(inside)) != "true" {
		return nil, localError(connect.CodeFailedPrecondition, ErrNotGitWorkTree)
	}
	listed, err := runGit(ctx, dir, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	staged, err := runGit(ctx, dir, "ls-files", "-z", "--stage")
	if err != nil {
		return nil, err
	}
	// The index modes: gitlinks (submodules) and the executable bit, which
	// a Windows file system does not keep.
	indexMode := map[string]string{}
	for _, rec := range splitNUL(staged) {
		meta, p, ok := strings.Cut(rec, "\t")
		if !ok {
			continue
		}
		if f := strings.Fields(meta); len(f) > 0 {
			indexMode[p] = f[0]
		}
	}
	out := &localList{}
	seen := map[string]bool{}
	for _, p := range splitNUL(listed) {
		if seen[p] {
			continue // a conflicted path is listed once per stage
		}
		seen[p] = true
		if strings.HasSuffix(p, "/") {
			// An untracked directory that git does not enter: a nested
			// repository.
			out.skipped = append(out.skipped, strings.TrimSuffix(p, "/"))
			continue
		}
		if indexMode[p] == modeGitlink {
			out.skipped = append(out.skipped, p)
			continue
		}
		if !validRelPath(p) {
			return nil, localError(connect.CodeFailedPrecondition, fmt.Errorf("tide: git listed a path that is not a local relative path: %q", p))
		}
		f, ok, err := readLocalFile(dir, p, indexMode[p])
		if err != nil {
			return nil, err
		}
		if ok {
			out.files = append(out.files, f)
		}
	}
	sort.Slice(out.files, func(i, j int) bool { return out.files[i].rel < out.files[j].rel })
	sort.Strings(out.skipped)
	return out, nil
}

// readLocalFile hashes one listed path. Missing paths, paths below symbolic
// links, and directories are absent from the local file list.
func readLocalFile(dir, rel, indexMode string) (localFile, bool, error) {
	// Lstat follows symbolic links in parent components, so check each
	// parent from the root outwards before looking up the final component.
	for i := range rel {
		if rel[i] != '/' {
			continue
		}
		parent := filepath.Join(dir, filepath.FromSlash(rel[:i]))
		info, err := os.Lstat(parent)
		if errors.Is(err, os.ErrNotExist) {
			return localFile{}, false, nil
		}
		if err != nil {
			return localFile{}, false, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return localFile{}, false, nil
		}
	}

	full := filepath.Join(dir, filepath.FromSlash(rel))
	info, err := os.Lstat(full)
	if errors.Is(err, os.ErrNotExist) {
		return localFile{}, false, nil
	}
	if err != nil {
		return localFile{}, false, err
	}
	f := localFile{rel: rel}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(full)
		if err != nil {
			return localFile{}, false, err
		}
		target = filepath.ToSlash(target)
		sum := sha256.Sum256([]byte(target))
		f.target = target
		f.entry = manifestEntry{Size: int64(len(target)), SHA256: hex.EncodeToString(sum[:]), Mode: modeSymlink}
	case info.Mode().IsRegular():
		h := sha256.New()
		file, err := os.Open(full)
		if err != nil {
			return localFile{}, false, err
		}
		n, err := io.Copy(h, file)
		file.Close()
		if err != nil {
			return localFile{}, false, err
		}
		mode := modeFile
		executable := info.Mode().Perm()&0o111 != 0
		if runtime.GOOS == "windows" {
			// Windows keeps no executable bit; the index mode has it.
			executable = indexMode == "100755"
		}
		if executable {
			mode = modeExecutable
		}
		f.entry = manifestEntry{Size: n, SHA256: hex.EncodeToString(h.Sum(nil)), Mode: mode}
	default:
		return localFile{}, false, nil
	}
	return f, true, nil
}

func runGit(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir, "-c", "core.quotepath=off"}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		return nil, localError(connect.CodeFailedPrecondition, fmt.Errorf("tide: git %s: %v: %s", args[0], err, msg))
	}
	return out, nil
}

func splitNUL(b []byte) []string {
	var out []string
	for _, p := range bytes.Split(b, []byte{0}) {
		if len(p) > 0 {
			out = append(out, string(p))
		}
	}
	return out
}

// validRelPath reports whether p is a clean slash-separated relative path
// without "." or ".." elements.
func validRelPath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\x00") {
		return false
	}
	if path.Clean(p) != p {
		return false
	}
	for _, el := range strings.Split(p, "/") {
		if el == "." || el == ".." || el == "" {
			return false
		}
	}
	return true
}

// Path rules.

// caseCollisions returns the groups of paths that differ only in case. It
// checks the files and every directory above them, because two directories
// that differ only in case merge into one on such a file system.
func caseCollisions(paths []string) [][]string {
	all := map[string]bool{}
	for _, p := range paths {
		all[p] = true
		for d := path.Dir(p); d != "."; d = path.Dir(d) {
			all[d] = true
		}
	}
	groups := map[string][]string{}
	for p := range all {
		k := foldCase(p)
		groups[k] = append(groups[k], p)
	}
	var out [][]string
	for _, g := range groups {
		if len(g) > 1 {
			sort.Strings(g)
			out = append(out, g)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}

// windowsReserved are the reserved device names of Windows. A name is
// reserved with or without an extension.
var windowsReserved = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true, "CONIN$": true, "CONOUT$": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"COM¹": true, "COM²": true, "COM³": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
	"LPT¹": true, "LPT²": true, "LPT³": true,
}

// invalidWindowsPath reports whether a slash-separated relative path is not
// valid on a Windows guest: an element is a reserved device name (also with
// an extension), contains a character in <>:"\|?* or a control character,
// or ends with a space or a dot.
func invalidWindowsPath(p string) bool {
	for _, el := range strings.Split(p, "/") {
		if el == "" {
			return true
		}
		if strings.HasSuffix(el, " ") || strings.HasSuffix(el, ".") {
			return true
		}
		for _, r := range el {
			if r < 0x20 || strings.ContainsRune(`<>:"\|?*`, r) {
				return true
			}
		}
		base := el
		if i := strings.IndexByte(base, '.'); i >= 0 {
			base = base[:i]
		}
		if windowsReserved[strings.ToUpper(strings.TrimRight(base, " "))] {
			return true
		}
	}
	return false
}

// foldCase folds p for the case collision check of Windows and macOS
// guests, whose default file systems ignore case. Upper-then-lower mapping
// folds the simple Unicode case pairs that those file systems treat as
// equal.
func foldCase(p string) string { return strings.ToLower(strings.ToUpper(p)) }
