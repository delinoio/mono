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
	"slices"
	"sort"
	"strings"
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
	manifestSuffix       = ".sandrun-sync.json"
	lockSuffix           = ".sandrun-sync.lock"
	pendingUpdatesSuffix = ".sandrun-sync-pending"
)

// syncMetadataLimit bounds the encoded manifest and pending update list.
const syncMetadataLimit int64 = 256 << 20

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
	// Uploaded counts the new, changed and retried files, Deleted the files that
	// the sync removed from the guest, and Skipped the unchanged files and
	// the submodules and nested repositories that sync does not enter.
	Uploaded int `json:"uploaded"`
	Deleted  int `json:"deleted"`
	Skipped  int `json:"skipped"`
	// Submodules lists the skipped submodules and nested repositories.
	Submodules []string `json:"submodules,omitempty"`
	// BytesSent counts the bytes of the uploaded archive and temporary lists.
	BytesSent int64 `json:"bytes_sent"`
	// Warnings reports incomplete apply or cleanup, for example symbolic
	// links that a Windows guest could not create.
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
//  3. New and changed files and pending regular-file updates are uploaded.
//     Manifest paths no longer listed are deleted. Guest files that are
//     not in the manifest stay.
//     With an older manifest that lacks UTF-8 validation, sync leaves an
//     absent path with U+FFFD on the guest and drops its entry with a warning.
//  4. A changed file above 100 MiB or changed files above 2 GiB in total
//     fail the sync before any upload (*LimitError). A guest or candidate
//     manifest or pending update list above 256 MiB fails with failed_precondition.
//  5. Paths that are not valid UTF-8 (all guests), paths that differ only
//     in case (Windows and macOS guests), paths that collide after canonical
//     normalization and case mapping (macOS guests), and paths that are not
//     valid on Windows (Windows guests) fail before the lock and any upload
//     (*PathRuleError).
//     Windows link targets with invalid UTF-8 or a line break fail at this
//     stage, including links that match the guest manifest.
//  6. The lock directory <workspace_path>.sandrun-sync.lock is created with one
//     exec; an existing lock fails with ErrSyncLocked unless Force is set.
//     Sync checks the manifest and reads the pending update list under the lock.
//  7. The changed files go in one tar archive next to the workspace, which
//     the guest tar extracts into the workspace. Before apply, sync records
//     regular-file overwrites in the pending list. After confirming the new
//     manifest, sync removes the list, archive and lock.
//
// The deleted files are removed before the archive is extracted, so that a
// path can change between a file and a directory; the directories that
// held deleted files under the path of a new file are removed when they are
// empty. A sync that stops before the new manifest is written leaves the
// old manifest unless apply removed a stale one during workspace recovery.
// A later sync uses the pending list to resend selected regular files even
// if they match the old manifest. If the SDK loses the lock acquisition
// stream, it releases the lock only if a recovered result confirms
// acquisition. If the SDK loses the apply stream, it keeps the lock and
// inputs until it observes a terminal result. If the result is unknown
// after a bounded wait, the lock and inputs stay in the guest.
func (c *Client) Sync(ctx context.Context, p SyncParams) (*SyncResult, error) {
	result, err := c.sync(ctx, p, syncMetadataLimit)
	var changed *syncFileChangedError
	if errors.As(err, &changed) {
		c.logger.DebugContext(ctx, "sandrun sync file changed after listing",
			"dev_session_id", p.DevSessionID, "reason", changed.reason)
	}
	return result, err
}

func (c *Client) sync(ctx context.Context, p SyncParams, metadataLimit int64) (*SyncResult, error) {
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
	root, err := openSyncRoot(p.LocalDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	local, err := listLocalFiles(ctx, root)
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

	old, err := c.readManifest(ctx, s, metadataLimit)
	if err != nil {
		return nil, err
	}

	plan, err := planSync(local, old, nil)
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
		// Windows reads the link list as UTF-8 with one entry per line.
		// Check every selected link, including entries the manifest already has.
		for _, f := range local.files {
			if f.entry.Mode != modeSymlink {
				continue
			}
			if strings.ContainsAny(f.target, "\r\n") {
				return nil, localError(connect.CodeFailedPrecondition, fmt.Errorf("sandrun: symbolic link %q has a target with a line break, which Windows sync cannot send", f.rel))
			}
			if !utf8.ValidString(f.target) {
				return nil, localError(connect.CodeFailedPrecondition, fmt.Errorf("sandrun: symbolic link %q has a target with invalid UTF-8, which Windows sync cannot send", f.rel))
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
	if int64(len(body)) > metadataLimit {
		return nil, localError(connect.CodeFailedPrecondition, fmt.Errorf("sync candidate manifest exceeds the size limit of %d bytes (encoded size %d bytes)", metadataLimit, len(body)))
	}

	// Step 6: the lock.
	lock := guestSibling(s, lockSuffix)
	force := "0"
	if p.Force {
		force = "1"
	}
	windows := osys == sandrunv1.GuestOs_GUEST_OS_WINDOWS
	cleanupEnv := map[string]string{guestEnvLock: lock, guestEnvPendingUpdates: ""}
	if !windows {
		cleanupEnv[guestEnvAliasReplace] = ""
	}
	lr, err := c.runGuest(ctx, s, opLock, map[string]string{
		guestEnvLock: lock, guestEnvForce: force, guestEnvWorkspace: s.GetWorkspacePath(),
	})
	if err != nil {
		var streamErr *ExecStreamError
		if errors.As(err, &streamErr) && c.syncLockAcquired(ctx, s, streamErr) {
			c.cleanupFailedSync(ctx, s, cleanupEnv)
		}
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

	cleanupOnError := true
	defer func() {
		if cleanupOnError {
			c.cleanupFailedSync(ctx, s, cleanupEnv)
		}
	}()

	// Another sync can write the manifest before this sync takes the lock.
	// Reread it under the lock when the workspace exists. When the workspace
	// is missing, plan against an empty manifest to restore all local files.
	var cur manifest
	if !workspaceMissing {
		cur, err = c.readManifest(ctx, s, metadataLimit)
		if err != nil {
			return nil, err
		}
	}
	pending, err := c.readPendingUpdates(ctx, s, metadataLimit)
	if err != nil {
		return nil, err
	}
	if workspaceMissing || len(pending) > 0 || cur.PathsValidated != old.PathsValidated || !maps.Equal(cur.Files, old.Files) {
		c.logger.DebugContext(ctx, "sandrun sync replanning under the lock", "dev_session_id", s.GetDevSessionId(), "workspace_missing", workspaceMissing)
		if plan, err = planSync(local, cur, pending); err != nil {
			return nil, err
		}
	}
	if plan.empty() && !workspaceMissing && len(pending) == 0 {
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
	applyEnv := map[string]string{
		guestEnvWorkspace: s.GetWorkspacePath(), guestEnvStaleManifest: "",
		guestEnvArchive: "", guestEnvDeletes: "", guestEnvReplaceFiles: "",
		guestEnvPrunes: "", guestEnvAliasPrunes: "", guestEnvLinks: "",
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
		done := make(chan struct{})
		go func() {
			defer close(done)
			pw.CloseWithError(writeSyncArchive(ctx, pw, root, changed, windows))
		}()
		n, err := c.writeGuestFile(ctx, s, archive, 0o644, pr, func(done int64) {
			report(TransferProgress{Phase: TransferUploading, BytesDone: done})
		})
		_ = pr.CloseWithError(errors.New("sandrun: upload ended"))
		<-done
		res.BytesSent += n
		if err != nil {
			return nil, err
		}
	}
	strictDeletes, replacedFiles, aliasReplacements := splitSyncDeletions(deleted, changed, osys == sandrunv1.GuestOs_GUEST_OS_MACOS)
	if !windows {
		applyEnv[guestEnvAliasReplace] = ""
	}
	prunes, aliasPrunes := pruneDirs(deleted, changed, osys)
	for _, list := range []struct {
		suffix string
		env    string
		paths  []string
	}{
		{".delete", guestEnvDeletes, strictDeletes},
		{".replace-alias", guestEnvAliasReplace, aliasReplacements},
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
		n, err := c.writeGuestFile(ctx, s, list, 0o644, bytes.NewReader(windowsLinkList(links)), nil)
		res.BytesSent += n
		if err != nil {
			return nil, err
		}
	}

	// Persist the union before apply can overwrite files whose manifest
	// entries still describe their previous contents.
	previousCount := len(pending)
	for _, f := range changed {
		if f.entry.Mode == modeSymlink {
			continue
		}
		switch cur.Files[f.rel].Mode {
		case modeFile, modeExecutable:
			pending[f.rel] = struct{}{}
		}
	}
	if len(pending) > previousCount {
		if err := c.writePendingUpdates(ctx, s, pending, metadataLimit); err != nil {
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

	// Only confirmed manifest publication allows cleanup of pending updates.
	if len(pending) > 0 {
		cleanupEnv[guestEnvPendingUpdates] = guestSibling(s, pendingUpdatesSuffix)
	}
	cr, err := c.runGuest(ctx, s, opCleanup, cleanupEnv)
	if err != nil || (cr.exitCode != 0 && cr.exitCode != exitPendingCleanupFailed) {
		res.Warnings = append(res.Warnings, "the guest could not remove the sync archive or release the sync lock; the next sync may need force")
	}
	if len(pending) > 0 && (err != nil || cr.exitCode&exitPendingCleanupFailed != 0) {
		res.Warnings = append(res.Warnings, "the guest could not remove the pending sync updates; the next sync may upload those files again")
	}
	report(TransferProgress{Phase: TransferDone, BytesDone: res.BytesSent, BytesTotal: res.BytesSent})
	return res, nil
}

func (c *Client) cleanupFailedSync(ctx context.Context, s *sandrunv1.DevSession, env map[string]string) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	if r, err := c.runGuest(cleanupCtx, s, opCleanup, env); err != nil || r.exitCode != 0 {
		c.logger.DebugContext(ctx, "sandrun sync clean-up failed", "dev_session_id", s.GetDevSessionId(), "code", codeText(err))
	}
}

// syncLockAcquired requires an explicit exit code that proves ownership.
func (c *Client) syncLockAcquired(ctx context.Context, s *sandrunv1.DevSession, streamErr *ExecStreamError) bool {
	result, err := c.recoverGuestExec(ctx, s, streamErr)
	if err != nil {
		c.logger.DebugContext(ctx, "sandrun sync kept the lock after losing the acquisition stream",
			"dev_session_id", s.GetDevSessionId(), "exec_id", streamErr.ExecID, "code", codeText(err))
		return false
	}
	return result.Kind == ExecResultExited && result.ExitCode != nil &&
		(*result.ExitCode == 0 || *result.ExitCode == exitWorkspaceMissing)
}

// waitForSyncApply observes the original exec because its command can keep
// writing after the stream ends. Cleaning up requires a terminal result.
func (c *Client) waitForSyncApply(ctx context.Context, s *sandrunv1.DevSession, streamErr *ExecStreamError) bool {
	_, err := c.recoverGuestExec(ctx, s, streamErr)
	if err != nil {
		c.logger.DebugContext(ctx, "sandrun sync kept the lock and inputs after losing the apply stream",
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

// Pending updates use NUL-separated workspace-relative paths on every OS.
// The SDK consumes the list itself, so guest shell path conventions do not apply.
func (c *Client) readPendingUpdates(ctx context.Context, s *sandrunv1.DevSession, limit int64) (map[string]struct{}, error) {
	pending := make(map[string]struct{})
	var b bytes.Buffer
	n, err := c.readGuestFile(ctx, s, guestSibling(s, pendingUpdatesSuffix), limit, &b, nil)
	if err != nil {
		if n == 0 && connect.CodeOf(err) == connect.CodeNotFound {
			return pending, nil
		}
		var limitError *LimitError
		if errors.As(err, &limitError) {
			return nil, localError(connect.CodeFailedPrecondition, fmt.Errorf("sync pending updates exceed the size limit of %d bytes", limit))
		}
		return nil, fmt.Errorf("read sync pending updates: %w", err)
	}
	body := b.Bytes()
	if len(body) == 0 || body[len(body)-1] != 0 || !utf8.Valid(body) {
		return nil, localError(connect.CodeFailedPrecondition, errors.New("invalid sync pending update list"))
	}
	for name := range bytes.SplitSeq(body[:len(body)-1], []byte{0}) {
		rel := string(name)
		if !validRelPath(rel) {
			return nil, localError(connect.CodeFailedPrecondition, errors.New("invalid path in sync pending update list"))
		}
		pending[rel] = struct{}{}
	}
	return pending, nil
}

func (c *Client) writePendingUpdates(ctx context.Context, s *sandrunv1.DevSession, pending map[string]struct{}, limit int64) error {
	var size int64
	for rel := range pending {
		size += int64(len(rel)) + 1
	}
	if size > limit {
		return localError(connect.CodeFailedPrecondition, fmt.Errorf("sync pending updates exceed the size limit of %d bytes", limit))
	}
	body := pathList(slices.Sorted(maps.Keys(pending)), false)
	if _, err := c.writeGuestFile(ctx, s, guestSibling(s, pendingUpdatesSuffix), 0o644, bytes.NewReader(body), nil); err != nil {
		return fmt.Errorf("write sync pending updates: %w", err)
	}
	return nil
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

// planSync selects new, changed and pending files for upload, applies the
// changed-file limits and derives deletions from the guest manifest.
func planSync(local *localList, old manifest, pending map[string]struct{}) (*syncPlan, error) {
	plan := &syncPlan{res: &SyncResult{Submodules: local.skipped, Skipped: len(local.skipped)}}
	current := map[string]bool{}
	for _, f := range local.files {
		current[f.rel] = true
		_, retry := pending[f.rel]
		// Re-upload ambiguous paths and selected regular files that a
		// previous apply may have changed without publishing the manifest.
		if prev, ok := old.Files[f.rel]; ok && prev == f.entry && !old.ambiguousPath(f.rel) &&
			!(retry && f.entry.Mode != modeSymlink) {
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

// PowerShell's ReadAllLines consumes one UTF-8 BOM. Write it explicitly
// so it preserves a leading U+FEFF in the first path.
const windowsListPreamble = "\xef\xbb\xbf"

// pathList returns the guest list file of relative paths: NUL-separated
// for Linux and macOS guests, newline-separated with backslashes for
// Windows guests.
func pathList(paths []string, windows bool) []byte {
	var b bytes.Buffer
	if windows {
		b.WriteString(windowsListPreamble)
	}
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

func windowsLinkList(links []localFile) []byte {
	var b bytes.Buffer
	b.WriteString(windowsListPreamble)
	for _, l := range links {
		b.WriteString(strings.ReplaceAll(l.rel, "/", `\`))
		b.WriteByte('\t')
		b.WriteString(strings.ReplaceAll(l.target, "/", `\`))
		b.WriteByte('\n')
	}
	return b.Bytes()
}

// splitSyncDeletions separates former files that incoming paths need as
// directories. Alias records carry both spellings so the guest can check
// whether it is safe to keep a directory created before a failed manifest write.
func splitSyncDeletions(deleted []string, changed []localFile, matchAliases bool) (strict, replacedFiles, aliases []string) {
	if len(deleted) == 0 || len(changed) == 0 {
		return deleted, nil, nil
	}
	key := func(p string) string { return p }
	if matchAliases {
		key = foldCase
	}
	parents := map[string]string{}
	for _, f := range changed {
		for dir := path.Dir(f.rel); dir != "."; dir = path.Dir(dir) {
			k := key(dir)
			if _, ok := parents[k]; ok {
				break
			}
			parents[k] = dir
		}
	}
	for _, rel := range deleted {
		switch incoming := parents[key(rel)]; incoming {
		case "":
			strict = append(strict, rel)
		case rel:
			replacedFiles = append(replacedFiles, rel)
		default:
			aliases = append(aliases, rel, incoming)
		}
	}
	return strict, replacedFiles, aliases
}

// pruneDirs lists the directories to remove after the deletions and before
// extraction, deeper directories first. An alias candidate adds three paths
// to the alias list: the old ancestor, the incoming spelling, and the
// directory to remove. The guest checks that both spellings name the same
// directory before it prunes an alias candidate.
func pruneDirs(deleted []string, changed []localFile, osys sandrunv1.GuestOs) (exact, aliases []string) {
	if len(deleted) == 0 || len(changed) == 0 {
		return nil, nil
	}
	key := func(p string) string {
		return p
	}
	switch osys {
	case sandrunv1.GuestOs_GUEST_OS_MACOS:
		key = foldNormalizedCase
	case sandrunv1.GuestOs_GUEST_OS_WINDOWS:
		key = foldCase
	}
	incoming := make(map[string]*localFile, len(changed))
	for i := range changed {
		f := &changed[i]
		incoming[key(f.rel)] = f
	}
	type alias struct{ old, incoming string }
	dirs := map[string]alias{}
	for _, d := range deleted {
		var chain []string
		for dir := path.Dir(d); dir != "."; dir = path.Dir(dir) {
			chain = append(chain, dir)
			if replacement, ok := incoming[key(dir)]; ok {
				var names alias
				if replacement.rel != dir {
					if osys == sandrunv1.GuestOs_GUEST_OS_WINDOWS && replacement.entry.Mode == modeSymlink {
						continue
					}
					names = alias{old: dir, incoming: replacement.rel}
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
