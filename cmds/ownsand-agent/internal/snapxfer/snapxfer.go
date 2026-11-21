// SPDX-License-Identifier: FSL-1.1-ALv2

// Package snapxfer exports snapshots to R2 and imports them with signed
// URLs. A snapshot travels as one tar stream of its content directory,
// split into parts of the size that the export command names. The agent
// asks for the signed URL of one part right before it transfers that part,
// so a transfer may run longer than the 15-minute URL lifetime. It streams
// every part between the temporary archive on disk and the network, so its
// memory use does not depend on the part size. Every step of a transfer
// stops once the context is done. Signed URLs are never logged.
package snapxfer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"

	"github.com/delinoio/mono/internal/fsutil"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// maxParts bounds the part count of an export and of an import. It matches
// the control plane's per-snapshot part limit.
const maxParts = 10000

// ErrTooManyParts reports that a snapshot needs more parts than maxParts.
var ErrTooManyParts = errors.New("snapxfer: snapshot needs more parts than the limit")

// TooManyPartsError carries the part count and limit of a rejected export.
type TooManyPartsError struct {
	PartCount int64
	Limit     int64
}

func (e *TooManyPartsError) Error() string {
	return fmt.Sprintf("snapxfer: snapshot needs %d parts; the limit is %d", e.PartCount, e.Limit)
}

func (e *TooManyPartsError) Is(target error) bool { return target == ErrTooManyParts }

// partCount returns how many parts of partSize bytes hold size bytes, at
// least 1. It computes in int64, so callers narrow only after checking the
// limit.
func partCount(size, partSize int64) int64 {
	n := size / partSize
	if size%partSize != 0 {
		n++
	}
	return max(n, 1)
}

// URLSource asks the control plane for signed part URLs.
type URLSource interface {
	RequestURLs(ctx context.Context, snapshotID string, op ownsandv1.SignedUrlOperation, parts []int32) ([]*ownsandv1.SignedPartUrl, error)
}

// Transfer runs exports and imports.
type Transfer struct {
	HTTP   *http.Client
	URLs   URLSource
	Logger *slog.Logger
	// TempDir holds the packed tar stream during a transfer.
	TempDir string

	// afterExtract runs in tests after Import extracted the staging
	// directory and before the final cancellation check and the rename.
	afterExtract func()
}

// partURL asks the control plane for the signed URL of one part. Callers
// ask right before they use the URL, because it expires after 15 minutes.
func (t *Transfer) partURL(ctx context.Context, snapshotID string, op ownsandv1.SignedUrlOperation, part int32) (string, error) {
	got, err := t.URLs.RequestURLs(ctx, snapshotID, op, []int32{part})
	if err != nil {
		return "", err
	}
	for _, u := range got {
		if u.GetPartNumber() == part && u.GetUrl() != "" {
			return u.GetUrl(), nil
		}
	}
	return "", fmt.Errorf("snapxfer: no signed URL for part %d", part)
}

// Export packs contentDir, uploads it in parts of partSize bytes and
// returns the parts with their SHA-256 and the total size. For each part it
// first hashes the part on disk, then asks for the part's upload URL and
// streams the part from the temporary archive, so it holds no part in
// memory. It returns the context error, and removes the temporary archive,
// when ctx is done at any step.
func (t *Transfer) Export(ctx context.Context, snapshotID, contentDir string, partSize int64) ([]*ownsandv1.SnapshotPart, int64, error) {
	if partSize <= 0 {
		return nil, 0, errors.New("snapxfer: part size must be positive")
	}
	tmp, err := os.CreateTemp(t.TempDir, ".export-"+snapshotID+"-*.tar")
	if err != nil {
		return nil, 0, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if err := fsutil.WriteTarContext(ctx, tmp, contentDir); err != nil {
		return nil, 0, fmt.Errorf("snapxfer: pack snapshot: %w", err)
	}
	size, err := tmp.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, 0, err
	}
	count64 := partCount(size, partSize)
	if count64 > maxParts {
		// Reject before the first URL request so that no part is uploaded.
		return nil, 0, &TooManyPartsError{PartCount: count64, Limit: maxParts}
	}
	count := int32(count64)
	parts := make([]*ownsandv1.SnapshotPart, 0, count)
	for p := int32(1); p <= count; p++ {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		off := int64(p-1) * partSize
		n := min(partSize, size-off)
		// Hash first: the URL clock starts only when the URL is issued.
		hash := sha256.New()
		if _, err := fsutil.CopyContext(ctx, hash, io.NewSectionReader(tmp, off, n)); err != nil {
			return nil, 0, fmt.Errorf("snapxfer: hash part %d: %w", p, err)
		}
		url, err := t.partURL(ctx, snapshotID, ownsandv1.SignedUrlOperation_SIGNED_URL_OPERATION_UPLOAD, p)
		if err != nil {
			return nil, 0, err
		}
		if err := t.put(ctx, url, tmp, off, n); err != nil {
			return nil, 0, fmt.Errorf("snapxfer: upload part %d: %w", p, err)
		}
		parts = append(parts, &ownsandv1.SnapshotPart{PartNumber: p, SizeBytes: n, Sha256: hash.Sum(nil)})
		t.Logger.Info("snapshot export progress", "snapshot_id", snapshotID, "part_number", p, "part_count", count, "size_bytes", n)
	}
	return parts, size, nil
}

// put uploads the n bytes of f at off with an exact Content-Length. The
// body is a section reader, so no copy of the part is held in memory, and
// GetBody hands out a fresh section so that the transport can replay the
// body after a redirect or a retry.
func (t *Transfer) put(ctx context.Context, url string, f *os.File, off, n int64) error {
	body := func() io.ReadCloser {
		if n == 0 {
			return http.NoBody
		}
		return io.NopCloser(io.NewSectionReader(f, off, n))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, body())
	if err != nil {
		return errors.New("invalid signed URL")
	}
	req.ContentLength = n
	req.GetBody = func() (io.ReadCloser, error) { return body(), nil }
	resp, err := t.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// The error text of net/http contains the URL; do not wrap it.
		return errors.New("upload request failed")
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("upload returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// Import downloads the parts, checks each size and SHA-256 against the
// import command (never against hashes stored in R2) and extracts the
// snapshot into contentDir, which must not exist yet. It asks for the
// download URL of a part right before it downloads that part and streams
// the part to the temporary archive while it hashes it, so it holds no part
// in memory. A part that is short, longer than its size or different from
// its hash fails the import before any extraction. When ctx is done, it
// returns the context error and publishes nothing: it removes the temporary
// archive and the staging directory, and it checks ctx once more right
// before the rename that publishes contentDir.
func (t *Transfer) Import(ctx context.Context, snapshotID string, parts []*ownsandv1.SnapshotPart, contentDir string) error {
	if len(parts) == 0 || len(parts) > maxParts {
		return errors.New("snapxfer: import needs between 1 and 10000 parts")
	}
	for i, p := range parts {
		if p.GetPartNumber() != int32(i+1) || p.GetSizeBytes() < 0 || len(p.GetSha256()) != sha256.Size {
			return errors.New("snapxfer: import parts must be numbered from 1 with a size and a SHA-256")
		}
	}
	tmp, err := os.CreateTemp(t.TempDir, ".import-"+snapshotID+"-*.tar")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	for _, p := range parts {
		if err := ctx.Err(); err != nil {
			return err
		}
		url, err := t.partURL(ctx, snapshotID, ownsandv1.SignedUrlOperation_SIGNED_URL_OPERATION_DOWNLOAD, p.GetPartNumber())
		if err != nil {
			return err
		}
		if err := t.get(ctx, url, tmp, p); err != nil {
			return fmt.Errorf("snapxfer: download part %d: %w", p.GetPartNumber(), err)
		}
		t.Logger.Info("snapshot import progress", "snapshot_id", snapshotID, "part_number", p.GetPartNumber(), "part_count", len(parts))
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}
	staging := contentDir + ".importing"
	os.RemoveAll(staging)
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return err
	}
	if err := fsutil.ReadTarContext(ctx, tmp, staging); err != nil {
		os.RemoveAll(staging)
		return fmt.Errorf("snapxfer: extract snapshot: %w", err)
	}
	if t.afterExtract != nil {
		t.afterExtract()
	}
	// The last checkpoint before the content becomes visible.
	if err := ctx.Err(); err != nil {
		os.RemoveAll(staging)
		return err
	}
	if err := os.MkdirAll(filepath.Dir(contentDir), 0o700); err != nil {
		os.RemoveAll(staging)
		return err
	}
	if err := os.Rename(staging, contentDir); err != nil {
		os.RemoveAll(staging)
		return err
	}
	return nil
}

// get downloads one part into the end of dst. It streams at most size+1
// bytes through a SHA-256, so a part that is longer than its size is
// noticed without being stored whole, and then it checks the byte count and
// the digest against part. Bytes of a failed part stay in dst; the caller
// discards the whole archive on error.
func (t *Transfer) get(ctx context.Context, url string, dst *os.File, part *ownsandv1.SnapshotPart) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return errors.New("invalid signed URL")
	}
	resp, err := t.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("download request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned HTTP %d", resp.StatusCode)
	}
	size := part.GetSizeBytes()
	if resp.ContentLength >= 0 && resp.ContentLength != size {
		return fmt.Errorf("part %d does not match its size and SHA-256", part.GetPartNumber())
	}
	hash := sha256.New()
	body := &readErrorTracker{r: io.LimitReader(resp.Body, size+1)}
	n, err := fsutil.CopyContext(ctx, io.MultiWriter(dst, hash), body)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if body.err != nil {
			// The error text of net/http may contain the URL; do not wrap it.
			return errors.New("download interrupted")
		}
		return err
	}
	if n != size || !bytes.Equal(hash.Sum(nil), part.GetSha256()) {
		return fmt.Errorf("part %d does not match its size and SHA-256", part.GetPartNumber())
	}
	return nil
}

// readErrorTracker remembers a read error of the network body, so a failed
// copy tells a download error (whose text is not passed on) from an error
// of the local file.
type readErrorTracker struct {
	r   io.Reader
	err error
}

func (t *readErrorTracker) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if err != nil && err != io.EOF {
		t.err = err
	}
	return n, err
}
