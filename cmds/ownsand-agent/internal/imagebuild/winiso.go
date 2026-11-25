// SPDX-License-Identifier: FSL-1.1-ALv2

package imagebuild

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"
)

// This file holds the attach and detach logic of the Windows installation ISO
// without a build tag, so that unit tests exercise it on every host. Only the
// Windows build (winiso_windows.go) wires it to PowerShell.

// isoMountTimeout bounds the Mount-DiskImage step. The step runs on a context
// that the build cancellation does not reach: a canceled Mount-DiskImage may
// still attach the image after the process was killed, and the build would
// not know to detach it.
const isoMountTimeout = 2 * time.Minute

// isoProbeTimeout bounds each re-read of the attach state while the build
// reconciles an ambiguous mount or detaches after a failure. These reads run
// on a fresh context, because the build context is often canceled already.
const isoProbeTimeout = 30 * time.Second

// isoDismountTimeout bounds each detach attempt.
const isoDismountTimeout = time.Minute

// Scripts of the three mount steps. Each reads the ISO path from the
// environment variable OWNSAND_ISO.
const (
	// isoQueryScript reads whether the ISO file is attached.
	isoQueryScript = `[pscustomobject]@{ attached = [bool](Get-DiskImage -ImagePath $env:OWNSAND_ISO).Attached } | ConvertTo-Json -Compress`
	// isoMountScript attaches the ISO read-only.
	isoMountScript = `Mount-DiskImage -ImagePath $env:OWNSAND_ISO -StorageType ISO -Access ReadOnly | Out-Null`
	// isoVolumeScript reads the drive letter of the attached ISO.
	isoVolumeScript = `$v = @(Get-DiskImage -ImagePath $env:OWNSAND_ISO | Get-Volume | Where-Object { $_.DriveLetter } | Select-Object -First 1)
[pscustomobject]@{ letter = if ($v.Count -gt 0) { [string]$v[0].DriveLetter } else { '' } } | ConvertTo-Json -Compress`
	// isoDismountScript detaches the ISO.
	isoDismountScript = `Dismount-DiskImage -ImagePath $env:OWNSAND_ISO | Out-Null`
)

// isoRunner runs a PowerShell script with env added to the environment and
// returns its standard output. Production uses powershell; tests replace it.
type isoRunner func(ctx context.Context, script string, env ...string) ([]byte, error)

type isoLocker func(ctx context.Context, path string) (func(), error)

// tail returns the last n bytes of s without surrounding space.
func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		s = "..." + s[len(s)-n:]
	}
	return s
}

// mountedISO is an ISO file attached as a CD-ROM volume. The build holds the
// lock of the ISO path from the attach until the attachment is released, so
// that two builds of the same ISO never share one attachment.
type mountedISO struct {
	path string
	// Root is the volume root, for example `E:\`.
	Root string
	// ours is set when the build attached the ISO, or may have attached it,
	// and must detach it. A successful dismount clears it; a failed one keeps
	// it, so a later dismount retries.
	ours bool
	// run runs the PowerShell steps: powershell in production, a fake in
	// tests, so they need no Windows image mount.
	run isoRunner
	// unlock releases the lock of the ISO path; it is nil once released.
	unlock func()
}

// releaseLock releases the lock of the ISO path, once.
func (m *mountedISO) releaseLock() {
	if m.unlock != nil {
		m.unlock()
		m.unlock = nil
	}
}

// queryAttached reads whether the ISO file is attached.
func (m *mountedISO) queryAttached(ctx context.Context) (bool, error) {
	out, err := m.run(ctx, isoQueryScript, "OWNSAND_ISO="+m.path)
	if err != nil {
		return false, err
	}
	var r struct {
		Attached *bool `json:"attached"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &r); err != nil || r.Attached == nil {
		return false, fmt.Errorf("unexpected attach state %q", tail(string(out), 256))
	}
	return *r.Attached, nil
}

// driveLetter reads the drive letter of the attached ISO.
func (m *mountedISO) driveLetter(ctx context.Context) (string, error) {
	out, err := m.run(ctx, isoVolumeScript, "OWNSAND_ISO="+m.path)
	if err != nil {
		return "", err
	}
	var r struct {
		Letter string `json:"letter"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &r); err != nil {
		return "", fmt.Errorf("unexpected output %q", tail(string(out), 256))
	}
	if len(r.Letter) != 1 || !(r.Letter[0] >= 'A' && r.Letter[0] <= 'Z' || r.Letter[0] >= 'a' && r.Letter[0] <= 'z') {
		return "", errors.New("the mounted ISO has no drive letter (is automount disabled?)")
	}
	return r.Letter, nil
}

// mountISOWith attaches the ISO read-only, or uses it where it is attached
// already, through run.
//
// The build owns the attachment (ours) from before the mount command is
// issued, so no failure after that point can leak it: when the mount command
// fails in an ambiguous way, the attach state is read again, and when drive
// letter discovery fails, is canceled or reads a bad letter, the build
// detaches the ISO on a fresh bounded context. The error of such a failure
// joins the primary error with the error of the detach, if any. The caller
// receives the attachment, and the duty to release it, only after the drive
// letter is known.
func mountISOWith(ctx context.Context, path string, run isoRunner, lock isoLocker) (*mountedISO, error) {
	unlock, err := lock(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("imagebuild: wait for another build of the ISO: %w", err)
	}
	if err := ctx.Err(); err != nil {
		unlock()
		return nil, err
	}
	m := &mountedISO{path: path, run: run, unlock: unlock}

	attached, err := m.queryAttached(ctx)
	if err != nil {
		m.releaseLock()
		return nil, fmt.Errorf("imagebuild: read the attach state of the ISO: %w", err)
	}
	if !attached {
		// Mark the attachment as ours before the command can attach it.
		m.ours = true
		mountCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), isoMountTimeout)
		_, err := run(mountCtx, isoMountScript, "OWNSAND_ISO="+path)
		cancel()
		if err != nil {
			err = fmt.Errorf("imagebuild: mount the ISO: %w", err)
			// The command may have attached the image before it failed. Keep
			// ours unless the attach state is read and shows a detached image.
			probeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), isoProbeTimeout)
			now, qerr := m.queryAttached(probeCtx)
			cancel()
			if qerr == nil && !now {
				m.ours = false
			}
			return nil, m.abort(err)
		}
	}

	letter, err := m.driveLetter(ctx)
	if err != nil {
		return nil, m.abort(fmt.Errorf("imagebuild: mount the ISO: %w", err))
	}
	m.Root = letter + `:\`
	return m, nil
}

// abort detaches an ISO that the build owns after a failed mount, on a fresh
// bounded context, releases the path lock and returns primary joined with the
// detach error. The caller has no mountedISO to retry the detach, so a failed
// detach is part of the returned error.
func (m *mountedISO) abort(primary error) error {
	ctx, cancel := context.WithTimeout(context.Background(), isoDismountTimeout)
	defer cancel()
	cleanupErr := m.dismount(ctx)
	m.releaseLock()
	if cleanupErr != nil {
		return errors.Join(primary, fmt.Errorf("imagebuild: detach the ISO after the failure: %w", cleanupErr))
	}
	return primary
}

// dismount detaches an ISO that the build attached. It clears m.ours only
// when the detach succeeded, so a dismount that failed, for example with a
// canceled context, can run again. The path lock is released once the build
// owns no attachment any more.
func (m *mountedISO) dismount(ctx context.Context) error {
	if !m.ours {
		m.releaseLock()
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, isoDismountTimeout)
	defer cancel()
	if _, err := m.run(ctx, isoDismountScript, "OWNSAND_ISO="+m.path); err != nil {
		return err
	}
	m.ours = false
	m.releaseLock()
	return nil
}

// release dismounts the ISO and logs a failure without failing the build: an
// attached ISO leaks a volume only, and the caller may call release again.
func (m *mountedISO) release(ctx context.Context, logger *slog.Logger) {
	if err := m.dismount(ctx); err != nil {
		logger.Warn("dismount the ISO failed", "error", err)
	}
}

// finish makes the final detach attempt and releases the lock even if it fails.
func (m *mountedISO) finish(logger *slog.Logger) {
	defer m.releaseLock()
	m.release(context.Background(), logger)
}

// isoLockKey normalizes path: Windows file names are case-insensitive.
func isoLockKey(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	return strings.ToLower(filepath.Clean(path))
}
