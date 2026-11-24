// SPDX-License-Identifier: Apache-2.0

package tide

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/go/uuidv7"
)

// Download limits.
const (
	// DownloadFileLimit is the largest file of a download: 500 MiB.
	DownloadFileLimit int64 = 500 << 20
	// DownloadTotalLimit is the largest download in total: 2 GiB.
	DownloadTotalLimit int64 = 2 << 30
)

// DownloadParams describes a download of a guest file or directory.
type DownloadParams struct {
	OrganizationID string
	DevSessionID   string
	// GuestPath is an absolute guest path, or a path relative to the
	// workspace path.
	GuestPath string
	// LocalPath is the local destination. An existing file or directory
	// there is replaced after the download succeeds; missing parent
	// directories are created.
	LocalPath string
	// Progress receives progress reports. Nil reports nothing.
	Progress func(TransferProgress)
}

// DownloadResult is the result of a download.
type DownloadResult struct {
	LocalPath string `json:"local_path"`
	// Directory is true when the guest path is a directory.
	Directory bool `json:"directory"`
	// Files counts the regular files, symbolic links and hard links written.
	Files int `json:"files"`
	// Bytes counts the regular-file bytes written, including hard-link fallback copies.
	Bytes    int64    `json:"bytes"`
	Warnings []string `json:"warnings,omitempty"`
}

// Download copies a guest file or directory to a local path, as the "Dev
// session download" section of docs/servers-tide-api-foundation.md defines
// it. A directory is packed with the guest tar into a temporary archive
// next to the workspace, read with ReadDevSessionFile and extracted
// locally; the guest archive is removed afterwards. A file above 500 MiB or
// a transfer above 2 GiB fails (*LimitError): the SDK checks the size of
// the file or the archive before it reads it and every archive entry during
// extraction. Extraction rejects absolute entries, entries outside the
// destination and symbolic links that point outside it. The result is
// written to a temporary location next to LocalPath and moved into place
// only after the download succeeded.
func (c *Client) Download(ctx context.Context, p DownloadParams) (*DownloadResult, error) {
	return c.download(ctx, p, archiveExtractor{fileLimit: DownloadFileLimit, totalLimit: DownloadTotalLimit, link: (*os.Root).Link})
}

func (c *Client) download(ctx context.Context, p DownloadParams, extractor archiveExtractor) (*DownloadResult, error) {
	report := func(tp TransferProgress) {
		if p.Progress != nil {
			p.Progress(tp)
		}
	}
	if p.GuestPath == "" || p.LocalPath == "" {
		return nil, localError(connect.CodeInvalidArgument, errors.New("tide: download needs a guest path and a local path"))
	}
	report(TransferProgress{Phase: TransferPreparing})
	s, err := c.runningSession(ctx, p.OrganizationID, p.DevSessionID)
	if err != nil {
		return nil, err
	}
	guestPath := guestResolve(s, p.GuestPath)
	id, err := uuidv7.NewString()
	if err != nil {
		return nil, err
	}
	archive := guestSibling(s, ".tide-download-"+id+".tar")
	env := map[string]string{guestEnvPath: guestPath, guestEnvDLArchive: archive}
	pr, err := c.runGuest(ctx, s, opProbe, env)
	// The probe may have created the archive also when it failed later, so
	// the archive is always removed.
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		if r, err := c.runGuest(cctx, s, opRemoveArchive, map[string]string{guestEnvDLArchive: archive}); err != nil || r.exitCode != 0 {
			c.logger.DebugContext(ctx, "tide download clean-up failed", "dev_session_id", s.GetDevSessionId(), "code", codeText(err))
		}
	}()
	if err != nil {
		return nil, err
	}
	switch pr.exitCode {
	case 0:
	case exitNotFound:
		return nil, localError(connect.CodeNotFound, fmt.Errorf("tide: the guest path %s does not exist", guestPath))
	default:
		return nil, pr.failure(opProbe)
	}
	kind, sizeText, _ := strings.Cut(strings.TrimSpace(lastLine(string(pr.stdout))), " ")
	size, perr := strconv.ParseInt(strings.TrimSpace(sizeText), 10, 64)
	if perr != nil || (kind != "file" && kind != "dir") {
		return nil, localError(connect.CodeInternal, errors.New("tide: the guest probe returned an unexpected result"))
	}
	dir := kind == "dir"
	switch {
	case !dir && size > DownloadFileLimit:
		return nil, localError(connect.CodeFailedPrecondition, &LimitError{Operation: "download", FileLimit: DownloadFileLimit, TotalLimit: DownloadTotalLimit, Paths: []PathSize{{Path: guestPath, Size: size}}})
	case dir && size > DownloadTotalLimit:
		return nil, localError(connect.CodeFailedPrecondition, &LimitError{Operation: "download", FileLimit: DownloadFileLimit, TotalLimit: DownloadTotalLimit, Total: size})
	}

	dest, err := filepath.Abs(p.LocalPath)
	if err != nil {
		return nil, err
	}
	parent := filepath.Dir(dest)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp(parent, ".tide-download-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	res := &DownloadResult{LocalPath: dest, Directory: dir}
	progress := func(done int64) {
		report(TransferProgress{Phase: TransferDownloading, BytesDone: done, BytesTotal: size})
	}
	var staged string
	if dir {
		af, err := os.CreateTemp(tmp, "archive-*.tar")
		if err != nil {
			return nil, err
		}
		_, err = c.readGuestFile(ctx, s, archive, DownloadTotalLimit, af, progress)
		if err == nil {
			_, err = af.Seek(0, 0)
		}
		if err != nil {
			af.Close()
			return nil, wrapLimit(err)
		}
		report(TransferProgress{Phase: TransferApplying, BytesDone: size, BytesTotal: size})
		staged = filepath.Join(tmp, "result")
		if err := os.Mkdir(staged, 0o755); err != nil {
			af.Close()
			return nil, err
		}
		st, err := extractor.extractArchive(ctx, af, staged)
		af.Close()
		if err != nil {
			return nil, wrapLimit(err)
		}
		res.Files, res.Bytes, res.Warnings = st.files, st.bytes, st.warnings
	} else {
		staged = filepath.Join(tmp, "result")
		f, err := os.OpenFile(staged, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, err
		}
		n, err := c.readGuestFile(ctx, s, guestPath, DownloadFileLimit, f, progress)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return nil, wrapLimit(err)
		}
		res.Files, res.Bytes = 1, n
	}
	if err := replacePath(staged, dest, os.Rename); err != nil {
		return nil, err
	}
	report(TransferProgress{Phase: TransferDone, BytesDone: size, BytesTotal: size})
	return res, nil
}

// wrapLimit gives a *LimitError the failed_precondition Connect code.
func wrapLimit(err error) error {
	var le *LimitError
	var ce *connect.Error
	if errors.As(err, &le) && !errors.As(err, &ce) {
		return localError(connect.CodeFailedPrecondition, err)
	}
	return err
}

// replacePath moves staged to dest. It keeps the previous destination in a
// short, separate backup path so that staging cleanup cannot delete it.
func replacePath(staged, dest string, rename func(string, string) error) error {
	_, err := os.Lstat(dest)
	if errors.Is(err, os.ErrNotExist) {
		return rename(staged, dest)
	}
	if err != nil {
		return err
	}

	backupDir, err := os.MkdirTemp(filepath.Dir(dest), ".tide-old-*")
	if err != nil {
		return fmt.Errorf("tide: create the destination backup directory: %w", err)
	}
	backup := filepath.Join(backupDir, "previous")
	if err := rename(dest, backup); err != nil {
		return errors.Join(fmt.Errorf("tide: move the existing destination aside: %w", err), os.Remove(backupDir))
	}
	if err := rename(staged, dest); err != nil {
		if restoreErr := rename(backup, dest); restoreErr != nil {
			return fmt.Errorf("tide: publish download: %w (restore failed, backup at %q: %w)", err, backup, restoreErr)
		}
		return errors.Join(err, os.Remove(backupDir))
	}
	return os.RemoveAll(backupDir)
}

func lastLine(s string) string {
	s = strings.TrimRight(s, "\r\n")
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}
