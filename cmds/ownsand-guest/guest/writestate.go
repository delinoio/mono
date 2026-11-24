// SPDX-License-Identifier: FSL-1.1-ALv2

package guest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Durable cleanup records of unfinished writes.
//
// The staging file of a write lives next to its target and is owned by the
// helper process only in memory. When the helper process exits (a crash, a
// kill, or a restart by the init process of a Linux guest) the staging file
// would stay forever, because no sweep knows about it. The helper therefore
// keeps one small record per staging file in "<state dir>/writes". A record
// holds the write_id, the staging path, the identity of the staging file and
// the time of the last accepted chunk. The next helper process loads the
// records at start and lets the regular sweep expire them after
// OperationRetention without a chunk, like the writes of the same process.
//
// Record lifecycle:
//  1. The staging file is created exclusively, then its record is written
//     (temporary file and rename). A crash between the sync and the rename of
//     this first save leaves only the temporary record. The next start
//     attempts to promote a complete, valid record and loads it from the
//     temporary path if promotion fails.
//  2. Each accepted chunk replaces the record to refresh the time of the last
//     chunk. A failure only logs: the next chunk or the sweep tries again.
//  3. A finish removes the record after the rename into place.
//  4. An expired write removes the staging file first and the record second,
//     so an interrupted cleanup keeps the record and the next sweep or the
//     next start tries again.
//
// A write that the helper recovers has no open file and no written ranges, so
// the helper cannot continue it. A chunk or finish for its write_id fails
// with not_found until the write expires, like a write whose state is lost.

const (
	writeRecordVersionUnixNano  = 1
	writeRecordVersionSplitTime = 2
)

var (
	minWriteRecordUnixNanoTime = time.Unix(0, math.MinInt64)
	maxWriteRecordUnixNanoTime = time.Unix(0, math.MaxInt64)
)

// writeRecord is the JSON content of a record file. The file is sandbox
// content (it holds a guest path), so the helper never logs it.
type writeRecord struct {
	Version        int          `json:"version"`
	WriteID        string       `json:"write_id"`
	TmpPath        string       `json:"tmp_path"`
	Identity       fileIdentity `json:"identity"`
	LastNano       *int64       `json:"last_chunk_unix_nano,omitempty"`
	LastSeconds    *int64       `json:"last_chunk_unix_seconds,omitempty"`
	LastNanosecond *int         `json:"last_chunk_nanosecond,omitempty"`

	lastChunk time.Time
}

// writeRecordSuffix and writeRecordTempSuffix name the record files. The
// record name is the SHA-256 hex digest of the write_id, because the helper
// does not validate the format of a write_id.
const (
	writeRecordSuffix     = ".json"
	writeRecordTempSuffix = ".json.tmp"
)

func writeRecordName(writeID string) string {
	sum := sha256.Sum256([]byte(writeID))
	return hex.EncodeToString(sum[:]) + writeRecordSuffix
}

// sanitizedError returns the text of err without the path of the operation.
// Errors of the file system name the path of a guest file, and guest paths
// are sandbox content that the helper never logs.
func sanitizedError(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		return le.Err.Error()
	}
	var se *json.SyntaxError
	if errors.As(err, &se) {
		return "malformed record"
	}
	var ue *json.UnmarshalTypeError
	if errors.As(err, &ue) {
		return "malformed record"
	}
	return err.Error()
}

// recordPath returns the canonical record path of a write. It returns an
// empty string when the manager does not keep records.
func (m *writeManager) recordPath(writeID string) string {
	if m.dir == "" {
		return ""
	}
	return filepath.Join(m.dir, writeRecordName(writeID))
}

// saveWriteRecord writes the record of e atomically. durable also syncs the file;
// a refresh skips the sync, because a stale time after a power loss only
// expires the staging file earlier, and the helper process exiting loses
// nothing that is in the page cache.
func saveWriteRecord(e *writeEntry, durable bool) error {
	path := e.recordPath
	if path == "" || e.identity == nil {
		return nil
	}
	rec := writeRecord{WriteID: e.id, TmpPath: e.tmp, Identity: *e.identity}
	if e.lastChunk.Before(minWriteRecordUnixNanoTime) || e.lastChunk.After(maxWriteRecordUnixNanoTime) {
		rec.Version = writeRecordVersionSplitTime
		seconds, nanosecond := e.lastChunk.Unix(), e.lastChunk.Nanosecond()
		rec.LastSeconds = &seconds
		rec.LastNanosecond = &nanosecond
	} else {
		rec.Version = writeRecordVersionUnixNano
		nano := e.lastChunk.UnixNano()
		rec.LastNano = &nano
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if durable {
		if err := f.Sync(); err != nil {
			f.Close()
			os.Remove(tmp)
			return err
		}
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// removeWriteRecord removes a record if it exists.
func removeWriteRecord(path string) error {
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// persistRecord saves the record and logs a failure. A failed record never
// fails the write: the upload still works, only its cleanup after a helper
// restart is lost.
func (m *writeManager) persistRecord(e *writeEntry, durable bool) {
	if err := saveWriteRecord(e, durable); err != nil {
		m.logger.Warn("save write record failed", "write_id", e.id, "error", sanitizedError(err))
	}
}

// lstatStaging looks up a recovered staging file; tests replace it to fail
// the lookup.
var lstatStaging = os.Lstat

// promoteWriteRecord renames a first record at startup. Tests replace it to
// fail promotion while leaving normal record saves and removals available.
var promoteWriteRecord = os.Rename

// loadRecords registers the writes of an earlier helper process. It loads a
// validated first record from its temporary path if promotion fails. It drops
// unreadable records and records whose staging file is confirmed gone. It
// clamps the last chunk time to now to handle a guest clock set backwards.
func (m *writeManager) loadRecords() error {
	if m.dir == "" {
		return nil
	}
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return fmt.Errorf("guest: create write record directory: %w", err)
	}
	// Settle the temporary records first: ReadDir sorts "<hash>.json" before
	// "<hash>.json.tmp", so a record that this pass promotes would be missed by
	// a single pass over the directory.
	unpromoted := m.settleTempRecords()
	items, err := os.ReadDir(m.dir)
	if err != nil {
		return fmt.Errorf("guest: read write record directory: %w", err)
	}
	now := m.now()
	for _, item := range items {
		name := item.Name()
		path := filepath.Join(m.dir, name)
		var rec writeRecord
		switch {
		case strings.HasSuffix(name, writeRecordTempSuffix):
			var ok bool
			rec, ok = unpromoted[name]
			if !ok {
				continue
			}
		case strings.HasSuffix(name, writeRecordSuffix):
			// Keep canonical precedence if the canonical file appeared since
			// the first pass. ReadDir lists it before its temporary name.
			delete(unpromoted, name+".tmp")
			var err error
			rec, err = readWriteRecord(path)
			if err != nil || rec.WriteID == "" || writeRecordName(rec.WriteID) != name {
				m.logger.Warn("drop unreadable write record", "error", recordLoadError(err))
				if err := os.Remove(path); err != nil {
					m.logger.Warn("remove unreadable write record failed", "error", sanitizedError(err))
				}
				continue
			}
		default:
			continue
		}
		if _, err := lstatStaging(rec.TmpPath); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				m.logger.Info("drop write record without staging file", "write_id", rec.WriteID)
				if err := removeWriteRecord(path); err != nil {
					m.logger.Warn("remove write record failed", "write_id", rec.WriteID, "error", sanitizedError(err))
				}
				continue
			}
			// Any other error (a transient I/O error, a permission denial) does
			// not show that the staging file is gone. Keep the record and
			// register the write, so the sweep retries the cleanup; dropping the
			// record would orphan a staging file that still exists.
			m.logger.Warn("write record: staging file lookup failed, keeping the record", "write_id", rec.WriteID, "error", sanitizedError(err))
		}
		last := rec.lastChunk
		if last.After(now) {
			last = now
		}
		identity := rec.Identity
		m.entries[rec.WriteID] = &writeEntry{
			id:         rec.WriteID,
			tmp:        rec.TmpPath,
			identity:   &identity,
			lastChunk:  last,
			recordPath: path,
			recovered:  true,
		}
		m.logger.Info("write recovered after restart", "write_id", rec.WriteID)
	}
	return nil
}

// settleTempRecords handles the temporary files of interrupted record saves.
// A temporary record exists only between the write of a save and its rename.
// When the canonical record exists, the save replaced an intact record and
// the temporary file is stale: it is removed. When the canonical record is
// absent, the interrupted save was the first one of a write (it is durable:
// the helper syncs it before the rename), so the temporary record is the only
// record of a staging file that exists. A complete, valid temporary record
// whose content belongs to its name is promoted to the canonical name, and the
// normal load checks (staging file lookup, identity at cleanup) apply to it.
// If promotion fails, keep the temporary file and return its validated record
// for loading. Remove invalid temporary records without touching staging files.
func (m *writeManager) settleTempRecords() map[string]writeRecord {
	items, err := os.ReadDir(m.dir)
	if err != nil {
		// loadRecords reports the same failure for its own read.
		return nil
	}
	unpromoted := make(map[string]writeRecord)
	for _, item := range items {
		name := item.Name()
		if !strings.HasSuffix(name, writeRecordTempSuffix) {
			continue
		}
		tempPath := filepath.Join(m.dir, name)
		canonical := strings.TrimSuffix(name, ".tmp")
		canonicalPath := filepath.Join(m.dir, canonical)
		if _, err := os.Lstat(canonicalPath); err == nil {
			// A canonical record keeps precedence over a stale temporary file.
			m.removeTempRecord(tempPath)
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			// The canonical record may exist; keep the temporary file and let
			// the next start decide.
			m.logger.Warn("write record temporary file kept: canonical lookup failed", "error", sanitizedError(err))
			continue
		}
		rec, err := readWriteRecord(tempPath)
		if err != nil || rec.WriteID == "" || writeRecordName(rec.WriteID) != canonical {
			m.logger.Warn("drop unreadable write record temporary file", "error", recordLoadError(err))
			m.removeTempRecord(tempPath)
			continue
		}
		if err := promoteWriteRecord(tempPath, canonicalPath); err != nil {
			m.logger.Warn("promote write record temporary file failed", "write_id", rec.WriteID, "error", sanitizedError(err))
			unpromoted[name] = rec
			continue
		}
		m.logger.Info("promoted write record temporary file", "write_id", rec.WriteID)
	}
	return unpromoted
}

func (m *writeManager) removeTempRecord(path string) {
	if err := removeWriteRecord(path); err != nil {
		m.logger.Warn("remove write record temporary file failed", "error", sanitizedError(err))
	}
}

func recordLoadError(err error) string {
	if err == nil {
		return "record does not match its file name"
	}
	return sanitizedError(err)
}

func readWriteRecord(path string) (writeRecord, error) {
	var rec writeRecord
	data, err := os.ReadFile(path)
	if err != nil {
		return rec, err
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return rec, err
	}
	switch rec.Version {
	case writeRecordVersionUnixNano:
		if rec.LastNano == nil || rec.LastSeconds != nil || rec.LastNanosecond != nil {
			return rec, errors.New("invalid UnixNano timestamp in write record")
		}
		rec.lastChunk = time.Unix(0, *rec.LastNano)
	case writeRecordVersionSplitTime:
		if rec.LastNano != nil || rec.LastSeconds == nil || rec.LastNanosecond == nil ||
			*rec.LastNanosecond < 0 || *rec.LastNanosecond >= int(time.Second) {
			return rec, errors.New("invalid split timestamp in write record")
		}
		rec.lastChunk = time.Unix(*rec.LastSeconds, int64(*rec.LastNanosecond))
		// A large positive Unix second can overflow the internal time.Time seconds.
		if *rec.LastSeconds >= 0 && rec.lastChunk.Before(time.Time{}) {
			return rec, errors.New("split timestamp out of range in write record")
		}
	default:
		return rec, errors.New("unsupported record version")
	}
	return rec, nil
}

// cleanup removes the staging file of an expired write when it still is the
// file that the helper created, then the record. It returns the first error
// and keeps the record when the staging file stays, so a retry has what it
// needs. A finished write has no staging file any more and only loses its
// record.
func (m *writeManager) cleanup(e *writeEntry) error {
	if !e.finished {
		if err := m.removeStaging(e); err != nil {
			return err
		}
	}
	return removeWriteRecord(e.recordPath)
}

// removeStaging removes the staging file of e. A file at the staging path
// that is not the file the helper created (somebody replaced it) is kept.
func (m *writeManager) removeStaging(e *writeEntry) error {
	if e.identity != nil {
		current, err := identityOfPath(e.tmp)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return nil
		case err != nil:
			return err
		case current != *e.identity:
			m.logger.Warn("keep replaced file at the write staging path", "write_id", e.id)
			return nil
		}
	}
	if err := m.removeFile(e.tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
