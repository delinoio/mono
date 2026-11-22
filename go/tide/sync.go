// SPDX-License-Identifier: Apache-2.0

package tide

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path"
	"sort"
	"strings"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/go/uuidv7"
	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
)

// Sync limits. They protect the bandwidth of the shared control plane host.
const (
	// SyncFileLimit is the largest changed file of a sync: 100 MiB.
	SyncFileLimit int64 = 100 << 20
	// SyncTotalLimit is the largest sum of the changed files of a sync:
	// 2 GiB.
	SyncTotalLimit int64 = 2 << 30
)

// Guest file names next to the workspace path.
const (
	manifestSuffix = ".tide-sync.json"
	lockSuffix     = ".tide-sync.lock"
)

// manifestReadLimit bounds the manifest that sync reads.
const manifestReadLimit int64 = 256 << 20

// cleanupTimeout bounds the guest clean-up after a failed sync or download.
const cleanupTimeout = 30 * time.Second

// SyncParams describes a sync of a local working tree into the workspace
// path of a dev session.
type SyncParams struct {
	OrganizationID string
	DevSessionID   string
	// LocalDir is a directory in a Git working tree.
	LocalDir string
	// Force removes a sync lock that a stopped sync left.
	Force bool
	// Progress receives progress reports. Nil reports nothing.
	Progress func(TransferProgress)
}

// SyncResult is the result of a sync.
type SyncResult struct {
	// Uploaded counts the new and changed files, Deleted the files that
	// the sync removed from the guest, and Skipped the unchanged files and
	// the submodules and nested repositories that sync does not enter.
	Uploaded int `json:"uploaded"`
	Deleted  int `json:"deleted"`
	Skipped  int `json:"skipped"`
	// Submodules lists the skipped submodules and nested repositories.
	Submodules []string `json:"submodules,omitempty"`
	// BytesSent counts the bytes of the uploaded archive and lists.
	BytesSent int64 `json:"bytes_sent"`
	// Warnings lists paths that the sync could not apply, for example
	// symbolic links that a Windows guest could not create.
	Warnings []string `json:"warnings,omitempty"`
}

// Sync copies a local working tree into the workspace path of a running
// dev session, as the "Dev session sync" section of
// docs/servers-tide-api-foundation.md defines it:
//
//  1. The file list is `git ls-files --cached --others --exclude-standard`
//     in LocalDir; submodules and nested repositories are skipped.
//  2. The guest manifest <workspace_path>.tide-sync.json lists the synced
//     paths with size, SHA-256 and mode; a missing or unreadable manifest
//     counts as empty.
//  3. New and changed files are uploaded; manifest paths that are no longer
//     listed are deleted. Guest files that are not in the manifest stay.
//  4. A changed file above 100 MiB or changed files above 2 GiB in total
//     fail the sync before any upload (*LimitError).
//  5. Paths that differ only in case (Windows and macOS guests) and paths
//     that are not valid on Windows (Windows guests) fail the sync before
//     any upload (*PathRuleError).
//  6. The lock directory <workspace_path>.tide-sync.lock is created with one
//     exec; an existing lock fails with ErrSyncLocked unless Force is set.
//  7. The changed files go in one tar archive next to the workspace, which
//     the guest tar extracts into the workspace; then the new manifest is
//     written and the archive and the lock are removed.
//
// The deleted files are removed before the archive is extracted, so that a
// path can change between a file and a directory; the directories that
// held deleted files under the path of a new file are removed when they are
// empty. A sync that stops before the new manifest is written leaves the
// old manifest, so the next sync sends the same files again.
func (c *Client) Sync(ctx context.Context, p SyncParams) (*SyncResult, error) {
	report := func(tp TransferProgress) {
		if p.Progress != nil {
			p.Progress(tp)
		}
	}
	report(TransferProgress{Phase: TransferPreparing})
	s, err := c.runningSession(ctx, p.OrganizationID, p.DevSessionID)
	if err != nil {
		return nil, err
	}
	osys := s.GetGuestOs()
	if _, err := guestCommand(osys, opLock); err != nil {
		return nil, err
	}
	local, err := listLocalFiles(ctx, p.LocalDir)
	if err != nil {
		return nil, err
	}
	old, err := c.readManifest(ctx, s)
	if err != nil {
		return nil, err
	}

	res := &SyncResult{Submodules: local.skipped, Skipped: len(local.skipped)}
	current := map[string]bool{}
	var changed []localFile
	var changedTotal int64
	for _, f := range local.files {
		current[f.rel] = true
		if prev, ok := old.Files[f.rel]; ok && prev == f.entry {
			res.Skipped++
			continue
		}
		changed = append(changed, f)
		if f.entry.Mode != modeSymlink {
			changedTotal += f.entry.Size
		}
	}
	skippedDirs := map[string]bool{}
	for _, d := range local.skipped {
		skippedDirs[d] = true
	}
	var deleted []string
	for rel := range old.Files {
		if !current[rel] && !skippedDirs[rel] {
			deleted = append(deleted, rel)
		}
	}
	sort.Strings(deleted)

	// Step 4: limits.
	var big []PathSize
	for _, f := range changed {
		if f.entry.Mode != modeSymlink && f.entry.Size > SyncFileLimit {
			big = append(big, PathSize{Path: f.rel, Size: f.entry.Size})
		}
	}
	if len(big) > 0 || changedTotal > SyncTotalLimit {
		le := &LimitError{Operation: "sync", Paths: big, FileLimit: SyncFileLimit, TotalLimit: SyncTotalLimit}
		if changedTotal > SyncTotalLimit {
			le.Total = changedTotal
			if len(big) == 0 {
				le.Paths = largest(changed, 10)
			}
		}
		return nil, localError(connect.CodeFailedPrecondition, le)
	}
	// Step 5: path rules for every local path.
	paths := make([]string, 0, len(local.files))
	for _, f := range local.files {
		paths = append(paths, f.rel)
	}
	if osys == tidev1.GuestOs_GUEST_OS_WINDOWS || osys == tidev1.GuestOs_GUEST_OS_MACOS {
		if groups := caseCollisions(paths); len(groups) > 0 {
			return nil, localError(connect.CodeFailedPrecondition, &PathRuleError{Rule: PathRuleCaseCollision, Paths: groups})
		}
	}
	if osys == tidev1.GuestOs_GUEST_OS_WINDOWS {
		var bad [][]string
		for _, p := range paths {
			if invalidWindowsPath(p) {
				bad = append(bad, []string{p})
			}
		}
		if len(bad) > 0 {
			return nil, localError(connect.CodeFailedPrecondition, &PathRuleError{Rule: PathRuleInvalidWindowsName, Paths: bad})
		}
	}
	if len(changed) == 0 && len(deleted) == 0 {
		report(TransferProgress{Phase: TransferDone})
		return res, nil
	}

	// Step 6: the lock.
	lock := guestSibling(s, lockSuffix)
	force := "0"
	if p.Force {
		force = "1"
	}
	lr, err := c.runGuest(ctx, s, opLock, map[string]string{guestEnvLock: lock, guestEnvForce: force})
	if err != nil {
		return nil, err
	}
	switch lr.exitCode {
	case 0:
	case exitLocked:
		return nil, localError(connect.CodeFailedPrecondition, ErrSyncLocked)
	default:
		return nil, lr.failure(opLock)
	}

	// Step 7: archive, deletions, manifest and clean-up.
	syncID, err := uuidv7.NewString()
	if err != nil {
		return nil, err
	}
	cleanupEnv := map[string]string{guestEnvLock: lock}
	succeeded := false
	defer func() {
		if succeeded {
			return
		}
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		if r, err := c.runGuest(cctx, s, opCleanup, cleanupEnv); err != nil || r.exitCode != 0 {
			c.logger.DebugContext(ctx, "tide sync clean-up failed", "dev_session_id", s.GetDevSessionId(), "code", codeText(err))
		}
	}()
	windows := osys == tidev1.GuestOs_GUEST_OS_WINDOWS
	applyEnv := map[string]string{guestEnvWorkspace: s.GetWorkspacePath(), guestEnvArchive: "", guestEnvDeletes: "", guestEnvPrunes: "", guestEnvLinks: ""}

	var links []localFile
	var archived int
	for _, f := range changed {
		if windows && f.entry.Mode == modeSymlink {
			links = append(links, f)
		} else {
			archived++
		}
	}
	if archived > 0 {
		archive := guestSibling(s, ".tide-sync-"+syncID+".tar")
		applyEnv[guestEnvArchive] = archive
		cleanupEnv[guestEnvArchive] = archive
		pr, pw := io.Pipe()
		go func() {
			pw.CloseWithError(writeSyncArchive(ctx, pw, p.LocalDir, changed, windows))
		}()
		sent := res.BytesSent
		n, err := c.writeGuestFile(ctx, s, archive, 0o644, pr, func(done int64) {
			report(TransferProgress{Phase: TransferUploading, BytesDone: sent + done, BytesTotal: changedTotal})
		})
		_ = pr.CloseWithError(errors.New("tide: upload ended"))
		res.BytesSent += n
		if err != nil {
			return nil, err
		}
	}
	if len(deleted) > 0 {
		list := guestSibling(s, ".tide-sync-"+syncID+".delete")
		applyEnv[guestEnvDeletes] = list
		cleanupEnv[guestEnvDeletes] = list
		n, err := c.writeGuestFile(ctx, s, list, 0o644, bytes.NewReader(pathList(deleted, windows)), nil)
		res.BytesSent += n
		if err != nil {
			return nil, err
		}
	}
	if prunes := pruneDirs(deleted, changed); len(prunes) > 0 {
		list := guestSibling(s, ".tide-sync-"+syncID+".prune")
		applyEnv[guestEnvPrunes] = list
		cleanupEnv[guestEnvPrunes] = list
		n, err := c.writeGuestFile(ctx, s, list, 0o644, bytes.NewReader(pathList(prunes, windows)), nil)
		res.BytesSent += n
		if err != nil {
			return nil, err
		}
	}
	if len(links) > 0 {
		list := guestSibling(s, ".tide-sync-"+syncID+".links")
		applyEnv[guestEnvLinks] = list
		cleanupEnv[guestEnvLinks] = list
		var b bytes.Buffer
		for _, l := range links {
			b.WriteString(strings.ReplaceAll(l.rel, "/", `\`))
			b.WriteByte('\t')
			b.WriteString(strings.ReplaceAll(l.target, "/", `\`))
			b.WriteByte('\n')
		}
		n, err := c.writeGuestFile(ctx, s, list, 0o644, &b, nil)
		res.BytesSent += n
		if err != nil {
			return nil, err
		}
	}

	report(TransferProgress{Phase: TransferApplying, BytesDone: res.BytesSent, BytesTotal: res.BytesSent})
	ar, err := c.runGuest(ctx, s, opApply, applyEnv)
	if err != nil {
		return nil, err
	}
	if ar.exitCode != 0 {
		return nil, ar.failure(opApply)
	}
	failedLinks := map[string]bool{}
	for _, line := range strings.Split(string(ar.stdout), "\n") {
		line = strings.TrimRight(line, "\r")
		if rel, ok := strings.CutPrefix(line, linkFailedPrefix); ok {
			rel = strings.ReplaceAll(rel, `\`, "/")
			failedLinks[rel] = true
			res.Warnings = append(res.Warnings, rel+": the guest could not create the symbolic link")
		}
	}

	next := manifest{Version: manifestVersion, Files: map[string]manifestEntry{}}
	for _, f := range local.files {
		if failedLinks[f.rel] {
			// Left out, so the next sync tries the link again.
			continue
		}
		next.Files[f.rel] = f.entry
	}
	body, err := json.Marshal(next)
	if err != nil {
		return nil, err
	}
	if _, err := c.writeGuestFile(ctx, s, guestSibling(s, manifestSuffix), 0o644, bytes.NewReader(body), nil); err != nil {
		return nil, err
	}
	res.Uploaded = len(changed) - len(failedLinks)
	res.Deleted = len(deleted)
	succeeded = true

	cr, err := c.runGuest(ctx, s, opCleanup, cleanupEnv)
	if err != nil || cr.exitCode != 0 {
		res.Warnings = append(res.Warnings, "the guest could not remove the sync archive or release the sync lock; the next sync may need force")
	}
	report(TransferProgress{Phase: TransferDone, BytesDone: res.BytesSent, BytesTotal: res.BytesSent})
	return res, nil
}

// readManifest reads the guest manifest. A missing or unreadable manifest
// counts as empty; only a cancelled caller and errors that also stop every
// later step of the sync (credential, permission and session state errors)
// end the sync.
func (c *Client) readManifest(ctx context.Context, s *tidev1.DevSession) (manifest, error) {
	var b bytes.Buffer
	_, err := c.readGuestFile(ctx, s, guestSibling(s, manifestSuffix), manifestReadLimit, &b, nil)
	if err != nil {
		if ctx.Err() != nil {
			return manifest{}, ctx.Err()
		}
		switch connect.CodeOf(err) {
		case connect.CodeUnauthenticated, connect.CodePermissionDenied, connect.CodeFailedPrecondition:
			return manifest{}, err
		}
		c.logger.DebugContext(ctx, "tide sync manifest counts as empty", "dev_session_id", s.GetDevSessionId(), "code", codeText(err))
		return parseManifest(nil), nil
	}
	return parseManifest(b.Bytes()), nil
}

// pathList returns the guest list file of relative paths: NUL-separated
// for Linux and macOS guests, newline-separated with backslashes for
// Windows guests.
func pathList(paths []string, windows bool) []byte {
	var b bytes.Buffer
	for _, p := range paths {
		if windows {
			b.WriteString(strings.ReplaceAll(p, "/", `\`))
			b.WriteByte('\n')
		} else {
			b.WriteString(p)
			b.WriteByte(0)
		}
	}
	return b.Bytes()
}

// pruneDirs returns the guest directories that the apply command removes
// after the deletions and before the extraction. The deletions remove only
// files, so a deleted entry a/b/old leaves the directories a/b and a, and
// the guest tar cannot then extract a new file or link at a. For each
// deleted path below the path of a changed file or link, the result holds
// the directories from the parent of the deleted path up to that path.
// The guest removes only empty directories, so files that the guest created
// there stay and make the apply fail. Deeper directories come first.
func pruneDirs(deleted []string, changed []localFile) []string {
	incoming := map[string]bool{}
	for _, f := range changed {
		incoming[f.rel] = true
	}
	dirs := map[string]bool{}
	for _, d := range deleted {
		var chain []string
		for dir := path.Dir(d); dir != "."; dir = path.Dir(dir) {
			chain = append(chain, dir)
			if incoming[dir] {
				for _, c := range chain {
					dirs[c] = true
				}
				break
			}
		}
	}
	out := make([]string, 0, len(dirs))
	for d := range dirs {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool {
		di, dj := strings.Count(out[i], "/"), strings.Count(out[j], "/")
		if di != dj {
			return di > dj
		}
		return out[i] < out[j]
	})
	return out
}

// largest returns the n largest regular files of files.
func largest(files []localFile, n int) []PathSize {
	var out []PathSize
	for _, f := range files {
		if f.entry.Mode != modeSymlink {
			out = append(out, PathSize{Path: f.rel, Size: f.entry.Size})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Size > out[j].Size })
	if len(out) > n {
		out = out[:n]
	}
	return out
}
