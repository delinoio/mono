// SPDX-License-Identifier: Apache-2.0

package guest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/delinoio/mono/go/ownsand/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// defaultFileMode applies when a finish carries mode 0.
const defaultFileMode = 0o644

// span is a written byte range [start, end).
type span struct{ start, end int64 }

// finishFields are the fields that a repeated finish must repeat.
type finishFields struct {
	size        int64
	mode        uint32
	chunkOffset int64
	chunkDigest [sha256.Size]byte
}

type writeEntry struct {
	mu         sync.Mutex
	path       string // exact path string of the binding
	target     string // path that the finish renames to
	tmp        string
	file       *os.File
	ranges     []span
	lastChunk  time.Time
	finished   bool
	finish     finishFields
	finishedAt time.Time
	result     *ownsandv1.WriteFileResponse
	gone       bool
}

// writeManager keeps the temporary file and written ranges of each
// write_id and renames the file into place on finish.
type writeManager struct {
	mu        sync.Mutex
	entries   map[string]*writeEntry
	expired   map[string]time.Time
	now       func() time.Time
	retention time.Duration
	logger    *slog.Logger
}

func newWriteManager(now func() time.Time, logger *slog.Logger) *writeManager {
	return &writeManager{
		entries:   map[string]*writeEntry{},
		expired:   map[string]time.Time{},
		now:       now,
		retention: OperationRetention,
		logger:    logger,
	}
}

// tempPath returns the temporary file of a write. It lives next to the
// target so that the final rename stays on one file system.
// The directory part is taken from the target as received, without a
// lexical clean, so the operating system resolves it like the target.
//
// The temporary name does not contain the target name, so its length does
// not depend on the length of the target name (a 255-byte target name must
// still be writable). When the name equals the target name, a second prefix
// is used so the staging file never is the target. The comparison folds
// letter case, because the staging file also is the target on a
// case-insensitive file system (macOS APFS, Windows NTFS) when the target
// differs from the name only in letter case.
func tempPath(target, writeID string) string {
	dir, base := splitTarget(target)
	id := boundedWriteID(writeID)
	name := ".ownsand-" + id + ".tmp"
	if strings.EqualFold(name, base) {
		name = ".ownsand-stage-" + id + ".tmp"
	}
	return siblingPath(dir, name)
}

// maxTempIDLen bounds the write_id part of a temporary name. The agent sends
// UUIDs (36 bytes), but the guest does not validate the format, so a longer
// id is replaced by its SHA-256 hex digest (64 bytes) to stay below the
// 255-byte file name limit of common file systems.
const maxTempIDLen = 128

func boundedWriteID(id string) string {
	if len(id) <= maxTempIDLen {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
}

func (m *writeManager) entry(req *ownsandv1.WriteFileRequest) (*writeEntry, error) {
	id := req.GetWriteId()
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, gone := m.expired[id]; gone {
		return nil, errorf(ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND, "write expired")
	}
	if e, ok := m.entries[id]; ok {
		if req.GetPath() != "" && nativePath(req.GetPath()) != e.path {
			return nil, errorf(ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION, "write_id is bound to another path")
		}
		return e, nil
	}
	if req.GetPath() == "" {
		return nil, errorf(ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND, "unknown write; the first call must carry the path")
	}
	if err := checkGuestPath(req.GetPath()); err != nil {
		return nil, err
	}
	// bound is the exact path string of the binding; target is the same
	// path without trailing separators. Neither is cleaned: the operating
	// system resolves "..", which follows symlinks first.
	bound := nativePath(req.GetPath())
	dir, base := splitTarget(bound)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return nil, errorf(ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION, "write: parent directory does not exist")
	}
	if isDirectoryName(base) {
		return nil, errorf(ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION, "write: target is a directory")
	}
	target := siblingPath(dir, base)
	if st, err := os.Lstat(target); err == nil && st.IsDir() {
		return nil, errorf(ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION, "write: target is a directory")
	}
	tmp := tempPath(target, id)
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fsError("write", err)
	}
	e := &writeEntry{path: bound, target: target, tmp: tmp, file: f, lastChunk: m.now()}
	m.entries[id] = e
	m.logger.Info("write started", "write_id", id)
	return e, nil
}

// apply handles one WriteFile call.
func (m *writeManager) apply(req *ownsandv1.WriteFileRequest) (*ownsandv1.WriteFileResponse, error) {
	if len(req.GetData()) > datapath.MaxFileChunkSize {
		return nil, errorf(ownsandv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "chunk larger than 1 MiB")
	}
	if req.GetOffset() < 0 {
		return nil, errorf(ownsandv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "offset must not be negative")
	}
	e, err := m.entry(req)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.gone {
		return nil, errorf(ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND, "write expired")
	}
	finish := req.GetFinish()
	if e.finished {
		if finish == nil {
			return nil, errorf(ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION, "write already finished")
		}
		if fieldsOf(req) != e.finish {
			return nil, errorf(ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION, "write already finished with other finish fields")
		}
		return e.result, nil
	}
	e.lastChunk = m.now()
	if err := e.writeChunk(req.GetOffset(), req.GetData()); err != nil {
		return nil, err
	}
	resp := &ownsandv1.WriteFileResponse{WriteId: req.GetWriteId(), Offset: req.GetOffset(), Length: int64(len(req.GetData()))}
	if finish == nil {
		return resp, nil
	}
	if err := e.complete(finish); err != nil {
		return nil, err
	}
	e.finished = true
	e.finish = fieldsOf(req)
	e.finishedAt = m.now()
	resp.Finished = true
	resp.SizeBytes = finish.GetSizeBytes()
	e.result = resp
	m.logger.Info("write finished", "write_id", req.GetWriteId(), "size_bytes", finish.GetSizeBytes())
	return resp, nil
}

func fieldsOf(req *ownsandv1.WriteFileRequest) finishFields {
	return finishFields{
		size:        req.GetFinish().GetSizeBytes(),
		mode:        req.GetFinish().GetMode(),
		chunkOffset: req.GetOffset(),
		chunkDigest: sha256.Sum256(req.GetData()),
	}
}

// writeChunk writes data at off. Bytes that overlap written ranges must be
// equal to the written bytes.
func (e *writeEntry) writeChunk(off int64, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	end := off + int64(len(data))
	for _, r := range e.ranges {
		lo, hi := max(r.start, off), min(r.end, end)
		if lo >= hi {
			continue
		}
		existing := make([]byte, hi-lo)
		if _, err := e.file.ReadAt(existing, lo); err != nil {
			return fsError("write", err)
		}
		if !bytes.Equal(existing, data[lo-off:hi-off]) {
			return errorf(ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION, "chunk overlaps written bytes with different content")
		}
	}
	if _, err := e.file.WriteAt(data, off); err != nil {
		return fsError("write", err)
	}
	e.ranges = mergeSpan(e.ranges, span{off, end})
	return nil
}

// mergeSpan adds s to the sorted, disjoint ranges and merges touching ones.
func mergeSpan(ranges []span, s span) []span {
	ranges = append(ranges, s)
	sort.Slice(ranges, func(i, j int) bool { return ranges[i].start < ranges[j].start })
	out := ranges[:1]
	for _, r := range ranges[1:] {
		last := &out[len(out)-1]
		if r.start <= last.end {
			last.end = max(last.end, r.end)
			continue
		}
		out = append(out, r)
	}
	return out
}

// complete checks that the written ranges cover exactly [0, size), applies
// the mode and renames the temporary file into place.
func (e *writeEntry) complete(finish *ownsandv1.WriteFileFinish) error {
	size := finish.GetSizeBytes()
	covered := (size == 0 && len(e.ranges) == 0) ||
		(len(e.ranges) == 1 && e.ranges[0].start == 0 && e.ranges[0].end == size)
	if !covered {
		return errorf(ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION, "written ranges do not cover exactly [0, size_bytes)")
	}
	mode := fs.FileMode(finish.GetMode() & 0o7777)
	if mode == 0 {
		mode = defaultFileMode
	}
	if err := e.file.Truncate(size); err != nil {
		return fsError("write", err)
	}
	if err := e.file.Chmod(mode); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		return fsError("write", err)
	}
	if err := e.file.Sync(); err != nil {
		return fsError("write", err)
	}
	if err := e.file.Close(); err != nil {
		return fsError("write", err)
	}
	if err := os.Rename(e.tmp, e.target); err != nil {
		// Reopen so a retry of the finish can try again.
		if f, openErr := os.OpenFile(e.tmp, os.O_RDWR, 0); openErr == nil {
			e.file = f
		}
		return fsError("write", err)
	}
	e.file = nil
	return nil
}

// sweep deletes the temporary files of writes without a chunk for the
// retention time and forgets finished writes after the retention time.
func (m *writeManager) sweep(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, e := range m.entries {
		e.mu.Lock()
		expired := (e.finished && now.Sub(e.finishedAt) >= m.retention) ||
			(!e.finished && now.Sub(e.lastChunk) >= m.retention)
		if expired {
			e.gone = true
			if e.file != nil {
				e.file.Close()
				e.file = nil
			}
			if !e.finished {
				if err := os.Remove(e.tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
					m.logger.Warn("remove write temporary file failed", "write_id", id, "error", err)
				}
			}
			delete(m.entries, id)
			m.expired[id] = now
			m.logger.Info("write expired", "write_id", id, "finished", e.finished)
		}
		e.mu.Unlock()
	}
	for id, at := range m.expired {
		if now.Sub(at) >= ExpiredMemory {
			delete(m.expired, id)
		}
	}
}
