// SPDX-License-Identifier: Apache-2.0

package sandrun

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
	"syscall"

	"connectrpc.com/connect"
	"golang.org/x/text/unicode/norm"
)

// Manifest modes. They use the Git file modes, so a manifest entry tells a
// regular file, an executable file and a symbolic link apart.
const (
	modeFile       uint32 = 0o100644
	modeExecutable uint32 = 0o100755
	modeSymlink    uint32 = 0o120000
	modeGitlink           = "160000"
	indexSymlink          = "120000"
)

// Bound reads of placeholder files while allowing targets as long as the
// extended Windows path limit, even when encoded as UTF-8.
const maxSymlinkPlaceholderBytes = 128 << 10

// manifestVersion is the version of the guest manifest format.
const manifestVersion = 1

// manifest is the guest sync manifest <workspace_path>.sandrun-sync.json.
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

type gitIndexEntry struct {
	mode      string
	stageZero bool
}

func (e gitIndexEntry) isSymlink() bool {
	return e.stageZero && e.mode == indexSymlink
}

// listLocalFiles selects the local files of a sync: the output of
// `git ls-files --cached --others --exclude-standard` in dir (tracked
// files and untracked files that are not ignored), without submodules.
// It reads each file and hashes its content.
func listLocalFiles(ctx context.Context, dir string) (*localList, error) {
	if _, err := exec.LookPath("git"); err != nil {
		return nil, localError(connect.CodeFailedPrecondition, errors.New("sandrun: sync needs git on the local machine"))
	}
	st, err := os.Stat(dir)
	if err != nil {
		return nil, localError(connect.CodeInvalidArgument, fmt.Errorf("sandrun: local directory: %w", err))
	}
	if !st.IsDir() {
		return nil, localError(connect.CodeInvalidArgument, fmt.Errorf("sandrun: %s is not a directory", dir))
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
	// Read Git's file types and the executable bit, which Windows does not
	// keep. Only a stage-zero link entry can identify a text placeholder.
	index := map[string]gitIndexEntry{}
	hasIndexedSymlink := false
	for _, rec := range splitNUL(staged) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		meta, p, ok := strings.Cut(rec, "\t")
		if !ok {
			continue
		}
		if fields := strings.Fields(meta); len(fields) == 3 {
			entry := gitIndexEntry{mode: fields[0], stageZero: fields[2] == "0"}
			index[p] = entry
			hasIndexedSymlink = hasIndexedSymlink || entry.isSymlink()
		}
	}
	symlinkPlaceholders := false
	if hasIndexedSymlink {
		value, err := runGit(ctx, dir, "config", "--type=bool", "--default", "true", "--get", "core.symlinks")
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err != nil {
			return nil, err
		}
		symlinkPlaceholders = strings.TrimSpace(string(value)) == "false"
	}
	out := &localList{}
	seen := map[string]bool{}
	for _, p := range splitNUL(listed) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
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
		entry := index[p]
		if entry.mode == modeGitlink {
			out.skipped = append(out.skipped, p)
			continue
		}
		if !validRelPath(p) {
			return nil, localError(connect.CodeFailedPrecondition, fmt.Errorf("sandrun: git listed a path that is not a local relative path: %q", p))
		}
		f, ok, err := readLocalFile(ctx, dir, p, entry.mode, symlinkPlaceholders && entry.isSymlink())
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

// readLocalFile hashes one listed path. Missing paths, directories, and paths
// below a file or symbolic link are absent from the local file list.
func readLocalFile(ctx context.Context, dir, rel, indexMode string, symlinkPlaceholder bool) (localFile, bool, error) {
	// Lstat follows symbolic links in parent components, so check each
	// parent from the root outwards before looking up the final component.
	for i := range rel {
		if rel[i] != '/' {
			continue
		}
		parent := filepath.Join(dir, filepath.FromSlash(rel[:i]))
		info, err := os.Lstat(parent)
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return localFile{}, false, nil
		}
		if err != nil {
			return localFile{}, false, err
		}
		if info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return localFile{}, false, nil
		}
	}

	full := filepath.Join(dir, filepath.FromSlash(rel))
	info, err := os.Lstat(full)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
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
		return localSymlink(rel, filepath.ToSlash(target)), true, nil
	case info.Mode().IsRegular():
		file, err := os.Open(full)
		if err != nil {
			return localFile{}, false, err
		}
		defer file.Close()
		if symlinkPlaceholder {
			target, err := io.ReadAll(io.LimitReader(localContextReader{ctx: ctx, src: file}, maxSymlinkPlaceholderBytes+1))
			if err := ctx.Err(); err != nil {
				return localFile{}, false, err
			}
			if err != nil {
				return localFile{}, false, err
			}
			if len(target) == 0 || len(target) > maxSymlinkPlaceholderBytes || bytes.IndexByte(target, 0) >= 0 {
				return localFile{}, false, localError(connect.CodeFailedPrecondition, fmt.Errorf("sandrun: invalid Git symlink placeholder target for %q (must be 1 to %d bytes without NUL)", rel, maxSymlinkPlaceholderBytes))
			}
			// Git wrote the target bytes. Keep literal backslashes and whitespace.
			return localSymlink(rel, string(target)), true, nil
		}
		size, digest, err := hashLocalFile(ctx, file)
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
		f.entry = manifestEntry{Size: size, SHA256: digest, Mode: mode}
	default:
		return localFile{}, false, nil
	}
	return f, true, nil
}

func localSymlink(rel, target string) localFile {
	sum := sha256.Sum256([]byte(target))
	return localFile{
		rel:    rel,
		target: target,
		entry:  manifestEntry{Size: int64(len(target)), SHA256: hex.EncodeToString(sum[:]), Mode: modeSymlink},
	}
}

// localContextReader checks the context before each read. It exposes only Read
// so io.Copy cannot call the underlying file's WriterTo method.
type localContextReader struct {
	ctx context.Context
	src io.Reader
}

func (r localContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.src.Read(p)
}

func hashLocalFile(ctx context.Context, src io.Reader) (int64, string, error) {
	h := sha256.New()
	size, err := io.Copy(h, localContextReader{ctx: ctx, src: src})
	if ctxErr := ctx.Err(); ctxErr != nil {
		return 0, "", ctxErr
	}
	if err != nil {
		return 0, "", err
	}
	return size, hex.EncodeToString(h.Sum(nil)), nil
}

func runGit(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir, "-c", "core.quotepath=off"}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		return nil, localError(connect.CodeFailedPrecondition, fmt.Errorf("sandrun: git %s: %v: %s", args[0], err, msg))
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

// pathCollisions groups distinct file and parent directory paths by their
// comparison key. Each group contains the original paths.
func pathCollisions(paths []string, key func(string) string) [][]string {
	all := map[string]bool{}
	for _, p := range paths {
		all[p] = true
		for d := path.Dir(p); d != "."; d = path.Dir(d) {
			all[d] = true
		}
	}
	groups := map[string][]string{}
	for p := range all {
		k := key(p)
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

// foldNormalizedCase makes canonically equivalent paths equal before applying
// the case mapping. Decompose again to compare the results of the case mapping.
func foldNormalizedCase(p string) string {
	return canonicalDecomposition(foldCase(canonicalDecomposition(p)))
}

func canonicalDecomposition(p string) string {
	decomposed := norm.NFD.String(p)
	if !strings.Contains(decomposed, norm.GraphemeJoiner) {
		return decomposed
	}

	// The stream-safe form inserts a combining grapheme joiner at the 31st
	// non-starter, which can prevent reordering across that boundary. Decompose
	// each rune and order the marks without inserting any joiners of our own.
	type mark struct {
		value rune
		class uint8
	}
	var result strings.Builder
	var marks []mark
	flush := func() {
		sort.SliceStable(marks, func(i, j int) bool { return marks[i].class < marks[j].class })
		for _, m := range marks {
			result.WriteRune(m.value)
		}
		marks = marks[:0]
	}
	for _, r := range p {
		part := norm.NFD.String(string(r))
		for i, value := range part {
			class := norm.NFD.PropertiesString(part[i:]).CCC()
			if class == 0 {
				flush()
				result.WriteRune(value)
			} else {
				marks = append(marks, mark{value, class})
			}
		}
	}
	flush()
	return result.String()
}
