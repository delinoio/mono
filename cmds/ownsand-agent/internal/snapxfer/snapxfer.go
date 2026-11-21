// SPDX-License-Identifier: Apache-2.0

// Package snapxfer exports snapshots to R2 and imports them with signed
// URLs. A snapshot travels as one tar stream of its content directory,
// split into parts of the size that the export command names. The agent
// asks for fresh part URLs right before it uses them, so a transfer may run
// longer than the 15-minute URL lifetime. Signed URLs are never logged.
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

// urlBatch is how many part URLs the agent asks for at a time.
const urlBatch = 8

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
}

func (t *Transfer) urls(ctx context.Context, snapshotID string, op ownsandv1.SignedUrlOperation, parts []int32) (map[int32]string, error) {
	got, err := t.URLs.RequestURLs(ctx, snapshotID, op, parts)
	if err != nil {
		return nil, err
	}
	out := map[int32]string{}
	for _, u := range got {
		out[u.GetPartNumber()] = u.GetUrl()
	}
	for _, p := range parts {
		if out[p] == "" {
			return nil, fmt.Errorf("snapxfer: no signed URL for part %d", p)
		}
	}
	return out, nil
}

// Export packs contentDir, uploads it in parts of partSize bytes and
// returns the parts with their SHA-256 and the total size.
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
	if err := fsutil.WriteTar(tmp, contentDir); err != nil {
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
	var parts []*ownsandv1.SnapshotPart
	for first := int32(1); first <= count; first += urlBatch {
		var batch []int32
		for p := first; p < first+urlBatch && p <= count; p++ {
			batch = append(batch, p)
		}
		urls, err := t.urls(ctx, snapshotID, ownsandv1.SignedUrlOperation_SIGNED_URL_OPERATION_UPLOAD, batch)
		if err != nil {
			return nil, 0, err
		}
		for _, p := range batch {
			off := int64(p-1) * partSize
			n := min(partSize, size-off)
			buf := make([]byte, n)
			if _, err := tmp.ReadAt(buf, off); err != nil && !errors.Is(err, io.EOF) {
				return nil, 0, err
			}
			sum := sha256.Sum256(buf)
			if err := t.put(ctx, urls[p], buf); err != nil {
				return nil, 0, fmt.Errorf("snapxfer: upload part %d: %w", p, err)
			}
			parts = append(parts, &ownsandv1.SnapshotPart{PartNumber: p, SizeBytes: n, Sha256: sum[:]})
			t.Logger.Info("snapshot export progress", "snapshot_id", snapshotID, "part_number", p, "part_count", count, "size_bytes", n)
		}
	}
	return parts, size, nil
}

func (t *Transfer) put(ctx context.Context, url string, data []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(data))
	if err != nil {
		return errors.New("invalid signed URL")
	}
	req.ContentLength = int64(len(data))
	resp, err := t.HTTP.Do(req)
	if err != nil {
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
// snapshot into contentDir, which must not exist yet.
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
	for first := 0; first < len(parts); first += urlBatch {
		batchParts := parts[first:min(first+urlBatch, len(parts))]
		var numbers []int32
		for _, p := range batchParts {
			numbers = append(numbers, p.GetPartNumber())
		}
		urls, err := t.urls(ctx, snapshotID, ownsandv1.SignedUrlOperation_SIGNED_URL_OPERATION_DOWNLOAD, numbers)
		if err != nil {
			return err
		}
		for _, p := range batchParts {
			data, err := t.get(ctx, urls[p.GetPartNumber()], p.GetSizeBytes())
			if err != nil {
				return fmt.Errorf("snapxfer: download part %d: %w", p.GetPartNumber(), err)
			}
			sum := sha256.Sum256(data)
			if int64(len(data)) != p.GetSizeBytes() || !bytes.Equal(sum[:], p.GetSha256()) {
				return fmt.Errorf("snapxfer: part %d does not match its size and SHA-256", p.GetPartNumber())
			}
			if _, err := tmp.Write(data); err != nil {
				return err
			}
			t.Logger.Info("snapshot import progress", "snapshot_id", snapshotID, "part_number", p.GetPartNumber(), "part_count", len(parts))
		}
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}
	staging := contentDir + ".importing"
	os.RemoveAll(staging)
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return err
	}
	if err := fsutil.ReadTar(tmp, staging); err != nil {
		os.RemoveAll(staging)
		return fmt.Errorf("snapxfer: extract snapshot: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(contentDir), 0o700); err != nil {
		return err
	}
	return os.Rename(staging, contentDir)
}

func (t *Transfer) get(ctx context.Context, url string, size int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, errors.New("invalid signed URL")
	}
	resp, err := t.HTTP.Do(req)
	if err != nil {
		return nil, errors.New("download request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, size+1))
	if err != nil {
		return nil, errors.New("download interrupted")
	}
	return data, nil
}
