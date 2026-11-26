// SPDX-License-Identifier: Apache-2.0

package sandrun

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"path"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/go/uuidv7"
	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
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
	manifestSuffix = ".sandrun-sync.json"
	lockSuffix     = ".sandrun-sync.lock"
)

// syncManifestLimit bounds the encoded manifest that sync reads and writes.
const syncManifestLimit int64 = 256 << 20

// cleanupTimeout bounds the guest clean-up after a failed sync or download.
const cleanupTimeout = 30 * time.Second

// syncApplyWait bounds how long a sync can wait for apply after losing its stream.
const syncApplyWait = 30 * time.Second

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
// docs/servers-sandrun-api-foundation.md defines it:
//
//  1. The file list is `git ls-files --cached --others --exclude-standard`
//     in LocalDir; submodules and nested repositories are skipped.
//  2. The guest manifest <workspace_path>.sandrun-sync.json lists the synced
//     paths with size, SHA-256 and mode; a missing or unreadable manifest
//     counts as empty unless it exceeds the manifest size limit. If the
//     lock command finds a missing workspace, sync replans against an
//     empty manifest.
//  3. New and changed files are uploaded. Manifest paths that are no longer
//     listed are deleted. Guest files that are not in the manifest stay.
//     With an older manifest that lacks UTF-8 validation, sync leaves an
//     absent path with U+FFFD on the guest and drops its entry with a warning.
//  4. A changed file above 100 MiB or changed files above 2 GiB in total
//     fail the sync before any upload (*LimitError). A guest or candidate
//     manifest above 256 MiB fails with failed_precondition.
//  5. Paths that are not valid UTF-8 (all guests), paths that differ only
//     in case (Windows and macOS guests), paths that collide after canonical
//     normalization and case mapping (macOS guests), and paths that are not
//     valid on Windows (Windows guests) fail before the lock and any upload
//     (*PathRuleError).
//  6. The lock directory <workspace_path>.sandrun-sync.lock is created with one
//     exec; an existing lock fails with ErrSyncLocked unless Force is set.
//     Sync checks the manifest under the lock before returning an unchanged result.
//  7. The changed files go in one tar archive next to the workspace, which
//     the guest tar extracts into the workspace; then the new manifest is
//     written and the archive and the lock are removed.
//
// The deleted files are removed before the archive is extracted, so that a
// path can change between a file and a directory; the directories that
// held deleted files under the path of a new file are removed when they are
// empty. A sync that stops before the new manifest is written leaves the
// old manifest unless apply removed a stale one during workspace recovery.
// The next sync sends the same files again once it can take the lock. If
// the SDK loses the apply stream, it keeps the lock and inputs until it
// observes a terminal result. If the result is unknown after a bounded
// wait, the lock and inputs stay in the guest.
func (c *Client) Sync(ctx context.Context, p SyncParams) (*SyncResult, error) {
	return c.sync(ctx, p, syncManifestLimit)
}

func (c *Client) sync(ctx context.Context, p SyncParams, manifestLimit int64) (*SyncResult, error) {
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

	// Check UTF-8 before comparing names or reading the guest manifest.
	paths := make([]string, 0, len(local.files))
	var invalidUTF8 [][]string
	for _, f := range local.files {
		paths = append(paths, f.rel)
		if !utf8.ValidString(f.rel) {
			invalidUTF8 = append(invalidUTF8, []string{f.rel})
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(invalidUTF8) > 0 {
		return nil, localError(connect.CodeFailedPrecondition, &PathRuleError{Rule: PathRuleInvalidUTF8, Paths: invalidUTF8})
	}

	old, err := c.readManifest(ctx, s, manifestLimit)
	if err != nil {
		return nil, err
	}

	plan, err := planSync(local, old)
	if err != nil {
		return nil, err
	}
	// The remaining path rules depend on the guest OS.
	if osys == sandrunv1.GuestOs_GUEST_OS_WINDOWS || osys == sandrunv1.GuestOs_GUEST_OS_MACOS {
		if groups := pathCollisions(paths, foldCase); len(groups) > 0 {
			return nil, localError(connect.CodeFailedPrecondition, &PathRuleError{Rule: PathRuleCaseCollision, Paths: groups})
		}
	}
	if osys == sandrunv1.GuestOs_GUEST_OS_MACOS {
		if groups := pathCollisions(paths, foldNormalizedCase); len(groups) > 0 {
			return nil, localError(connect.CodeFailedPrecondition, &PathRuleError{Rule: PathRuleNormalizationCollision, Paths: groups})
		}
	}
	if osys == sandrunv1.GuestOs_GUEST_OS_WINDOWS {
		var bad [][]string
		for _, p := range paths {
			if invalidWindowsPath(p) {
				bad = append(bad, []string{p})
			}
		}
		if len(bad) > 0 {
			return nil, localError(connect.CodeFailedPrecondition, &PathRuleError{Rule: PathRuleInvalidWindowsName, Paths: bad})
		}
		// Windows reads one symbolic link per line. A line break in a
		// target would make it interpret the remaining bytes as a new entry.
		for _, f := range local.files {
			if f.entry.Mode == modeSymlink && strings.ContainsAny(f.target, "\r\n") {
				return nil, localError(connect.CodeFailedPrecondition, fmt.Errorf("sandrun: symbolic link %q has a target with a line break, which Windows sync cannot send", f.rel))
			}
		}
	}
	// Check the encoded manifest before taking the lock or uploading anything.
	// Apply can only make it smaller by leaving out failed Windows links.
	next := manifest{Version: manifestVersion, Files: make(map[string]manifestEntry, len(local.files)), PathsValidated: true}
	for _, f := range local.files {
		next.Files[f.rel] = f.entry
	}
	body, err := json.Marshal(next)
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > manifestLimit {
		return nil, localError(connect.CodeFailedPrecondition, fmt.Errorf("sync candidate manifest exceeds the size limit of %d bytes (encoded size %d bytes)", manifestLimit, len(body)))
	}

	// Step 6: the lock.
	lock := guestSibling(s, lockSuffix)
	force := "0"
	if p.Force {
		force = "1"
	}
	lr, err := c.runGuest(ctx, s, opLock, map[string]string{
		guestEnvLock: lock, guestEnvForce: force, guestEnvWorkspace: s.GetWorkspacePath(),
	})
	if err != nil {
		return nil, err
	}
	workspaceMissing := lr.exitCode == exitWorkspaceMissing
	switch lr.exitCode {
	case 0, exitWorkspaceMissing:
	case exitLocked:
		return nil, localError(connect.CodeFailedPrecondition, ErrSyncLocked)
	default:
		return nil, lr.failure(opLock)
	}

	cleanupEnv := map[string]string{guestEnvLock: lock}
	cleanupOnError := true
	defer func() {
		if !cleanupOnError {
			return
		}
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		if r, err := c.runGuest(cctx, s, opCleanup, cleanupEnv); err != nil || r.exitCode != 0 {
			c.logger.DebugContext(ctx, "sandrun sync clean-up failed", "dev_session_id", s.GetDevSessionId(), "code", codeText(err))
		}
	}()

	// Another sync can write the manifest before this sync takes the lock.
	// Reread it under the lock when the workspace exists. When the workspace
	// is missing, plan against an empty manifest to restore all local files.
	var cur manifest
	if !workspaceMissing {
		cur, err = c.readManifest(ctx, s, manifestLimit)
		if err != nil {
			return nil, err
		}
	}
	if workspaceMissing || cur.PathsValidated != old.PathsValidated || !maps.Equal(cur.Files, old.Files) {
		c.logger.DebugContext(ctx, "sandrun sync replanning under the lock", "dev_session_id", s.GetDevSessionId(), "workspace_missing", workspaceMissing)
		if plan, err = planSync(local, cur); err != nil {
			return nil, err
		}
	}
	if plan.empty() && !workspaceMissing {
		cleanupOnError = false
		res := plan.res
		if r, err := c.runGuest(ctx, s, opCleanup, cleanupEnv); err != nil || r.exitCode != 0 {
			res.Warnings = append(res.Warnings, "the guest could not release the sync lock; the next sync may need force")
		}
		report(TransferProgress{Phase: TransferDone})
		return res, nil
	}
	res, changed, deleted := plan.res, plan.changed, plan.deleted

	// Step 7: archive, deletions, manifest and clean-up.
	syncID, err := uuidv7.NewString()
	if err != nil {
		return nil, err
	}
	windows := osys == sandrunv1.GuestOs_GUEST_OS_WINDOWS
	applyEnv := map[string]string{
		guestEnvWorkspace: s.GetWorkspacePath(), guestEnvStaleManifest: "",
		guestEnvArchive: "", guestEnvDeletes: "", guestEnvReplaceFiles: "",
		guestEnvPrunes: "", guestEnvLinks: "",
	}

	if workspaceMissing {
		// Once apply recreates the workspace, a retry can no longer detect
		// its absence. Remove the stale manifest before creating it so a
		// partial restore cannot leave missing files marked as unchanged.
		applyEnv[guestEnvStaleManifest] = guestSibling(s, manifestSuffix)
	}

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
		archive := guestSibling(s, ".sandrun-sync-"+syncID+".tar")
		applyEnv[guestEnvArchive] = archive
		cleanupEnv[guestEnvArchive] = archive
		pr, pw := io.Pipe()
		go func() {
			pw.CloseWithError(writeSyncArchive(ctx, pw, p.LocalDir, changed, windows))
		}()
		n, err := c.writeGuestFile(ctx, s, archive, 0o644, pr, func(done int64) {
			report(TransferProgress{Phase: TransferUploading, BytesDone: done})
		})
		_ = pr.CloseWithError(errors.New("sandrun: upload ended"))
		res.BytesSent += n
		if err != nil {
			return nil, err
		}
	}
	strictDeletes, replacedFiles := splitSyncDeletions(deleted, changed)
	if !windows {
		applyEnv[guestEnvAliasPrunes] = ""
	}
	prunes, aliasPrunes := pruneDirs(deleted, changed, osys == sandrunv1.GuestOs_GUEST_OS_MACOS)
	for _, list := range []struct {
		suffix string
		env    string
		paths  []string
	}{
		{".delete", guestEnvDeletes, strictDeletes},
		{".replace", guestEnvReplaceFiles, replacedFiles},
		{".prune", guestEnvPrunes, prunes},
		{".prune-alias", guestEnvAliasPrunes, aliasPrunes},
	} {
		if len(list.paths) == 0 {
			continue
		}
		listPath := guestSibling(s, ".sandrun-sync-"+syncID+list.suffix)
		applyEnv[list.env] = listPath
		cleanupEnv[list.env] = listPath
		n, err := c.writeGuestFile(ctx, s, listPath, 0o644, bytes.NewReader(pathList(list.paths, windows)), nil)
		res.BytesSent += n
		if err != nil {
			return nil, err
		}
	}
	if len(links) > 0 {
		list := guestSibling(s, ".sandrun-sync-"+syncID+".links")
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
	var failedLinks map[string]bool
	if workspaceMissing || plan.hasFileChanges() {
		ar, err := c.runGuest(ctx, s, opApply, applyEnv)
		if err != nil {
			var streamErr *ExecStreamError
			if errors.As(err, &streamErr) {
				cleanupOnError = c.waitForSyncApply(ctx, s, streamErr)
			}
			return nil, err
		}
		if ar.exitCode != 0 {
			return nil, ar.failure(opApply)
		}
		var warnings []string
		failedLinks, warnings = linkOutcome(links, ar)
		res.Warnings = append(res.Warnings, warnings...)
		if ar.outputLost && len(links) > 0 {
			c.logger.WarnContext(ctx, "sandrun sync apply output incomplete; links left out of the manifest", "dev_session_id", s.GetDevSessionId(), "links", len(links))
		}
	}

	if len(failedLinks) > 0 {
		// Leave out failed links so the next sync tries them again.
		for rel := range failedLinks {
			delete(next.Files, rel)
		}
		body, err = json.Marshal(next)
		if err != nil {
			return nil, err
		}
	}
	if _, err := c.writeGuestFile(ctx, s, guestSibling(s, manifestSuffix), 0o644, bytes.NewReader(body), nil); err != nil {
		return nil, err
	}
	res.Uploaded = len(changed) - len(failedLinks)
	res.Deleted = len(deleted)
	for _, rel := range plan.ambiguousDeletes {
		res.Warnings = append(res.Warnings, fmt.Sprintf("%q: skipped deletion because an older SDK may have replaced invalid UTF-8 bytes when it recorded this path", rel))
	}
	cleanupOnError = false

	cr, err := c.runGuest(ctx, s, opCleanup, cleanupEnv)
	if err != nil || cr.exitCode != 0 {
		res.Warnings = append(res.Warnings, "the guest could not remove the sync archive or release the sync lock; the next sync may need force")
	}
	report(TransferProgress{Phase: TransferDone, BytesDone: res.BytesSent, BytesTotal: res.BytesSent})
	return res, nil
}

// waitForSyncApply observes the original exec because its command can keep
// writing after the stream ends. Cleaning up requires a terminal result.
func (c *Client) waitForSyncApply(ctx context.Context, s *sandrunv1.DevSession, streamErr *ExecStreamError) bool {
	waitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), syncApplyWait)
	defer cancel()
	_, err := c.AttachExec(waitCtx, AttachParams{
		OrganizationID: s.GetOrganizationId(),
		DevSessionID:   s.GetDevSessionId(),
		ExecID:         streamErr.ExecID,
		StdoutOffset:   streamErr.StdoutOffset,
		StderrOffset:   streamErr.StderrOffset,
	})
	if err != nil {
		c.logger.DebugContext(waitCtx, "sandrun sync kept the lock and inputs after losing the apply stream",
			"dev_session_id", s.GetDevSessionId(), "exec_id", streamErr.ExecID, "code", codeText(err))
	}
	return err == nil
}

// readManifest reads the guest manifest. A missing or unreadable manifest
// counts as empty. An oversized manifest, cancellation, and credential,
// permission or session state errors end the sync.
func (c *Client) readManifest(ctx context.Context, s *sandrunv1.DevSession, manifestLimit int64) (manifest, error) {
	var b bytes.Buffer
	_, err := c.readGuestFile(ctx, s, guestSibling(s, manifestSuffix), manifestLimit, &b, nil)
	if err != nil {
		if ctx.Err() != nil {
			return manifest{}, ctx.Err()
		}
		var limitError *LimitError
		if errors.As(err, &limitError) {
			return manifest{}, localError(connect.CodeFailedPrecondition, fmt.Errorf("sync guest manifest exceeds the size limit of %d bytes", manifestLimit))
		}
		switch connect.CodeOf(err) {
		case connect.CodeUnauthenticated, connect.CodePermissionDenied, connect.CodeFailedPrecondition:
			return manifest{}, err
		}
		c.logger.DebugContext(ctx, "sandrun sync manifest counts as empty", "dev_session_id", s.GetDevSessionId(), "code", codeText(err))
		return parseManifest(nil), nil
	}
	return parseManifest(b.Bytes()), nil
}

// linkOutcome returns the symbolic links of a Windows apply command that
// the new manifest leaves out, so that the next sync creates them again,
// and their warnings. These are the links that the guest reported with
// linkFailedPrefix lines. When the apply output is incomplete, the SDK
// cannot tell which links the guest created, so it leaves out every link.
func linkOutcome(links []localFile, r *guestResult) (map[string]bool, []string) {
	sent := map[string]bool{}
	for _, l := range links {
		sent[l.rel] = true
	}
	failed := map[string]bool{}
	var warnings []string
	for _, rel := range r.linkFailures {
		rel = strings.ReplaceAll(rel, `\`, "/")
		if sent[rel] && !failed[rel] {
			failed[rel] = true
			warnings = append(warnings, rel+": the guest could not create the symbolic link")
		}
	}
	if r.outputLost {
		for _, l := range links {
			if !failed[l.rel] {
				failed[l.rel] = true
				warnings = append(warnings, l.rel+": the sync could not confirm the symbolic link because the guest output was incomplete; the next sync creates it again")
			}
		}
	}
	return failed, warnings
}

// syncPlan is what a sync changes in the guest for one manifest.
type syncPlan struct {
	// res holds the skipped counts and the submodules.
	res              *SyncResult
	changed          []localFile
	deleted          []string
	ambiguousDeletes []string
	changedTotal     int64
}

func (p *syncPlan) hasFileChanges() bool { return len(p.changed) > 0 || len(p.deleted) > 0 }

func (p *syncPlan) empty() bool { return !p.hasFileChanges() && len(p.ambiguousDeletes) == 0 }

// ambiguousPath identifies entries from older clients that may have been
// written by encoding/json with invalid bytes replaced by U+FFFD. Keep this
// check as long as sync can read manifests from clients without validation.
func (m manifest) ambiguousPath(rel string) bool {
	return !m.PathsValidated && strings.ContainsRune(rel, utf8.RuneError)
}

// planSync compares the local files with the guest manifest old (steps 3
// and 4): the new and changed files, the deleted manifest paths and the
// size limits of the changed files.
func planSync(local *localList, old manifest) (*syncPlan, error) {
	plan := &syncPlan{res: &SyncResult{Submodules: local.skipped, Skipped: len(local.skipped)}}
	current := map[string]bool{}
	for _, f := range local.files {
		current[f.rel] = true
		// Re-upload an ambiguous entry before claiming it as a validated path.
		if prev, ok := old.Files[f.rel]; ok && prev == f.entry && !old.ambiguousPath(f.rel) {
			plan.res.Skipped++
			continue
		}
		plan.changed = append(plan.changed, f)
		if f.entry.Mode != modeSymlink {
			plan.changedTotal += f.entry.Size
		}
	}
	skippedDirs := map[string]bool{}
	for _, d := range local.skipped {
		skippedDirs[d] = true
	}
	for rel := range old.Files {
		if current[rel] || skippedDirs[rel] {
			continue
		}
		if old.ambiguousPath(rel) {
			plan.ambiguousDeletes = append(plan.ambiguousDeletes, rel)
		} else {
			plan.deleted = append(plan.deleted, rel)
		}
	}
	sort.Strings(plan.deleted)
	sort.Strings(plan.ambiguousDeletes)

	var big []PathSize
	for _, f := range plan.changed {
		if f.entry.Mode != modeSymlink && f.entry.Size > SyncFileLimit {
			big = append(big, PathSize{Path: f.rel, Size: f.entry.Size})
		}
	}
	if len(big) > 0 || plan.changedTotal > SyncTotalLimit {
		le := &LimitError{Operation: "sync", Paths: big, FileLimit: SyncFileLimit, TotalLimit: SyncTotalLimit}
		if plan.changedTotal > SyncTotalLimit {
			le.Total = plan.changedTotal
			if len(big) == 0 {
				le.Paths = largest(plan.changed, 10)
			}
		}
		return nil, localError(connect.CodeFailedPrecondition, le)
	}
	return plan, nil
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

// splitSyncDeletions separates former files that incoming paths need
// as directories. If the manifest write failed after apply, those paths may
// already be directories and must stay in place when the SDK retries.
func splitSyncDeletions(deleted []string, changed []localFile) (strict, replacedFiles []string) {
	if len(deleted) == 0 || len(changed) == 0 {
		return deleted, nil
	}
	parents := map[string]bool{}
	for _, f := range changed {
		for dir := path.Dir(f.rel); dir != "." && !parents[dir]; dir = path.Dir(dir) {
			parents[dir] = true
		}
	}
	for _, rel := range deleted {
		if parents[rel] {
			replacedFiles = append(replacedFiles, rel)
		} else {
			strict = append(strict, rel)
		}
	}
	return strict, replacedFiles
}

// pruneDirs lists the directories to remove after the deletions and before
// extraction, deeper directories first. An alias candidate adds three paths
// to the alias list: the old ancestor, the incoming spelling, and the
// directory to remove. The guest checks that both spellings name the same
// directory before it prunes an alias candidate.
func pruneDirs(deleted []string, changed []localFile, matchAliases bool) (exact, aliases []string) {
	if len(deleted) == 0 || len(changed) == 0 {
		return nil, nil
	}
	key := func(p string) string {
		if matchAliases {
			return foldCase(p)
		}
		return p
	}
	incoming := map[string]string{}
	for _, f := range changed {
		incoming[key(f.rel)] = f.rel
	}
	type alias struct{ old, incoming string }
	dirs := map[string]alias{}
	for _, d := range deleted {
		var chain []string
		for dir := path.Dir(d); dir != "."; dir = path.Dir(dir) {
			chain = append(chain, dir)
			if replacement, ok := incoming[key(dir)]; ok {
				var names alias
				if replacement != dir {
					names = alias{old: dir, incoming: replacement}
				}
				for _, c := range chain {
					dirs[c] = names
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
	for _, d := range out {
		if names := dirs[d]; names.old != "" {
			aliases = append(aliases, names.old, names.incoming, d)
		} else {
			exact = append(exact, d)
		}
	}
	return exact, aliases
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
