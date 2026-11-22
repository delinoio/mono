// SPDX-License-Identifier: FSL-1.1-ALv2

package guest

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// Retention rules of the file transfer contract.
const (
	// OperationRetention is how long a read snapshot stays after the last
	// call for its read_id ended, and how long a write stays without a
	// chunk or after its finish.
	OperationRetention = 15 * time.Minute
	// ExpiredMemory is how long the helper remembers an expired read_id or
	// write_id, so a late call fails with not_found instead of starting
	// again. The device agent keeps its own 30-minute memory as well.
	ExpiredMemory = 30 * time.Minute
)

// SnapshotKind says how a read snapshot was made.
type SnapshotKind int

const (
	SnapshotKindUnspecified SnapshotKind = iota
	// SnapshotKindReflink is a copy-on-write clone: a point-in-time view.
	SnapshotKindReflink
	// SnapshotKindCopy is a full copy, checked for identity, size and
	// modification time before and after the copy.
	SnapshotKindCopy
)

func (k SnapshotKind) String() string {
	switch k {
	case SnapshotKindReflink:
		return "reflink"
	case SnapshotKindCopy:
		return "copy"
	}
	return "unspecified"
}

// snapshotFile is the open snapshot that a read call reads its chunks from.
type snapshotFile interface {
	io.ReaderAt
	io.Closer
}

type readEntry struct {
	path     string
	snapshot string
	ready    chan struct{}
	// Set before ready is closed.
	err  error
	size int64
	kind SnapshotKind
	// Guarded by readManager.mu.
	open    int
	lastEnd time.Time
}

// readManager keeps one private snapshot per read_id.
type readManager struct {
	mu        sync.Mutex
	dir       string
	entries   map[string]*readEntry
	expired   map[string]time.Time
	now       func() time.Time
	retention time.Duration
	logger    *slog.Logger
	// copyHook runs between the copy and the second check, and forceCopy
	// skips the reflink attempt; tests use them.
	copyHook  func()
	forceCopy bool
	// openHook runs between the first stat of the source path and the open
	// of it; tests use it to swap the path deterministically.
	openHook func()
	// openSnapshot opens a snapshot file for reading chunks; nil means
	// os.Open. Tests replace it to stall a chunk read.
	openSnapshot func(path string) (snapshotFile, error)
}

// open opens the snapshot file at path for one read call.
func (m *readManager) open(path string) (snapshotFile, error) {
	if m.openSnapshot != nil {
		return m.openSnapshot(path)
	}
	return os.Open(path)
}

func newReadManager(dir string, now func() time.Time, logger *slog.Logger) (*readManager, error) {
	// Snapshots of an earlier helper process are unreachable: no read_id
	// state survives a restart.
	if err := os.RemoveAll(dir); err != nil {
		return nil, fmt.Errorf("guest: clear read snapshots: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("guest: create read snapshot directory: %w", err)
	}
	return &readManager{
		dir:       dir,
		entries:   map[string]*readEntry{},
		expired:   map[string]time.Time{},
		now:       now,
		retention: OperationRetention,
		logger:    logger,
	}, nil
}

// begin registers one call for readID. The first call starts the snapshot.
func (m *readManager) begin(readID, path string) (*readEntry, error) {
	if err := checkGuestPath(path); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, gone := m.expired[readID]; gone {
		return nil, errorf(ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND, "read expired")
	}
	if e, ok := m.entries[readID]; ok {
		if e.path != nativePath(path) {
			return nil, errorf(ownsandv1.ErrorCode_ERROR_CODE_ALREADY_EXISTS, "read_id is bound to another path")
		}
		e.open++
		return e, nil
	}
	e := &readEntry{
		path:     nativePath(path),
		snapshot: filepath.Join(m.dir, readID),
		ready:    make(chan struct{}),
		open:     1,
	}
	m.entries[readID] = e
	go m.prepare(readID, e)
	return e, nil
}

// end ends one call. A failed snapshot is forgotten when its last call
// ends, or by prepare when the failure comes after the last call ended, so a
// retry can try again. A snapshot that succeeded after the last call ended
// stays until the retention time passes.
func (m *readManager) end(readID string, e *readEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e.open--
	e.lastEnd = m.now()
	if e.open == 0 {
		select {
		case <-e.ready:
			if e.err != nil && m.entries[readID] == e {
				delete(m.entries, readID)
			}
		default:
		}
	}
}

// prepare makes the snapshot and publishes the result. It publishes under
// m.mu so that it is ordered against end(): a failure published after the
// last call ended removes the entry here, and a failure published before it
// is removed by end(). Either way a retry of the read_id snapshots again.
func (m *readManager) prepare(readID string, e *readEntry) {
	kind, size, err := m.makeSnapshot(e.path, e.snapshot)
	if err != nil {
		os.Remove(e.snapshot)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	defer close(e.ready)
	if err != nil {
		e.err = err
		// No call is left to receive the failure, and sweep keeps no
		// failed entry: forget it so the next call for the read_id starts
		// a new snapshot instead of returning this stale error.
		if e.open == 0 && m.entries[readID] == e {
			delete(m.entries, readID)
		}
		m.logger.Info("read snapshot failed", "read_id", readID, "error", err.Error())
		return
	}
	e.kind, e.size = kind, size
	m.logger.Info("read snapshot ready", "read_id", readID, "kind", kind.String(), "size_bytes", size)
}

func (m *readManager) makeSnapshot(src, dst string) (SnapshotKind, int64, error) {
	// statSource captures the identity of the file together with its size
	// and modification time, so that the comparisons below detect a path
	// that was swapped after this stat on every platform.
	before, err := statSource(src)
	if err != nil {
		return 0, 0, fsError("read", err)
	}
	if !before.Mode().IsRegular() {
		return 0, 0, errorf(ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION, "read: path is not a regular file")
	}
	if m.openHook != nil {
		m.openHook()
	}
	in, err := os.Open(src)
	if err != nil {
		return 0, 0, fsError("read", err)
	}
	defer in.Close()
	// The pathname may have been swapped between the stat and the open, so
	// the descriptor that is copied must itself be the validated file.
	if inInfo, err := in.Stat(); err != nil || !inInfo.Mode().IsRegular() || !os.SameFile(before, inInfo) {
		return 0, 0, errorf(ownsandv1.ErrorCode_ERROR_CODE_ABORTED, "read: file changed during the snapshot copy")
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, 0, fsError("read snapshot", err)
	}
	defer out.Close()
	if !m.forceCopy && reflink(out, in) == nil {
		st, err := out.Stat()
		if err != nil {
			return 0, 0, fsError("read snapshot", err)
		}
		if !sourceUnchanged(before, in, src) {
			return 0, 0, errorf(ownsandv1.ErrorCode_ERROR_CODE_ABORTED, "read: file changed during the snapshot copy")
		}
		return SnapshotKindReflink, st.Size(), nil
	}
	n, err := io.Copy(out, in)
	if err != nil {
		return 0, 0, fsError("read snapshot", err)
	}
	if m.copyHook != nil {
		m.copyHook()
	}
	if !sourceUnchanged(before, in, src) {
		return 0, 0, errorf(ownsandv1.ErrorCode_ERROR_CODE_ABORTED, "read: file changed during the snapshot copy")
	}
	after := before
	// The copied byte count must equal the stat size, except for a procfs
	// file: it reports st_size 0 although a read returns bytes, so its
	// snapshot size is the byte count of the copy.
	if n != after.Size() && !(before.Size() == 0 && isProcfs(in)) {
		return 0, 0, errorf(ownsandv1.ErrorCode_ERROR_CODE_ABORTED, "read: file changed during the snapshot copy")
	}
	return SnapshotKindCopy, n, nil
}

// sourceUnchanged reports whether both the open descriptor and the pathname
// still identify the file that was validated before the copy, with the same
// size and modification time.
func sourceUnchanged(before os.FileInfo, in *os.File, src string) bool {
	inInfo, err := in.Stat()
	if err != nil {
		return false
	}
	pathInfo, err := statSource(src)
	if err != nil {
		return false
	}
	for _, after := range []os.FileInfo{inInfo, pathInfo} {
		if !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
			return false
		}
	}
	return true
}

// sweep deletes snapshots whose last call ended more than the retention
// time ago and forgets expired IDs after ExpiredMemory.
func (m *readManager) sweep(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, e := range m.entries {
		if e.open > 0 || now.Sub(e.lastEnd) < m.retention {
			continue
		}
		delete(m.entries, id)
		if err := os.Remove(e.snapshot); err != nil && !errors.Is(err, os.ErrNotExist) {
			// The error is reduced to its reason: the helper never logs paths.
			m.logger.Warn("remove read snapshot failed", "read_id", id, "error", sanitizedError(err))
		}
		m.expired[id] = now
		m.logger.Info("read expired", "read_id", id)
	}
	for id, at := range m.expired {
		if now.Sub(at) >= ExpiredMemory {
			delete(m.expired, id)
		}
	}
}

// checkGuestPath checks that path is an absolute path of this guest.
func checkGuestPath(path string) error {
	if path == "" || !filepath.IsAbs(path) {
		return errorf(ownsandv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "path must be an absolute guest path")
	}
	for i := 0; i < len(path); i++ {
		if path[i] == 0 {
			return errorf(ownsandv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "path must not contain NUL bytes")
		}
	}
	return nil
}
