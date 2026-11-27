// SPDX-License-Identifier: FSL-1.1-ALv2

package guest

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

// snapshotCopyChunk is how many bytes one copy step moves before the worker
// checks whether the manager was closed. The step is a LimitedReader copy, so
// the kernel copy offload (copy_file_range) stays in use.
const snapshotCopyChunk = 16 << 20

// readCloseGrace is how long Close waits for in-flight snapshot work.
// Filesystem calls can outlive the wait, with snapshot paths fenced by the
// unique directory (see readManager.close). Tests shorten it.
var readCloseGrace = 2 * time.Second

// errReadsClosed is the error of a snapshot that the shutdown of the helper
// ended, or of a read that starts after it.
func errReadsClosed() error {
	return errorf(ownsandv1.ErrorCode_ERROR_CODE_UNAVAILABLE, "read: the helper is shutting down")
}

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

// readRemoval reserves a snapshot path until removal succeeds or finds it
// absent. Its state is guarded by readManager.mu.
type readRemoval struct {
	path   string
	active bool
	failed bool
}

// readManager keeps one private snapshot per read_id.
type readManager struct {
	mu sync.Mutex
	// dir is the snapshot directory of this manager: a directory with a
	// unique name inside the reads directory of the state directory (see
	// newReadManager).
	dir     string
	entries map[string]*readEntry
	expired map[string]time.Time
	// pending tracks queued, active and failed removals independently of
	// expired. After the read entry is gone, each sweep retries failed
	// removals. Guarded by mu.
	pending   map[string]*readRemoval
	now       func() time.Time
	retention time.Duration
	logger    *slog.Logger
	// closed fences new reads, creates, and publication under mu. Closing
	// stop also ends running copies. wg counts preparation workers and
	// removals started by admission or sweep.
	closed bool
	stop   chan struct{}
	wg     sync.WaitGroup
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
	// Tests replace these filesystem calls to stall or fail snapshot I/O.
	// Nil uses os.OpenFile or os.Remove.
	createFile func(path string, flag int, perm os.FileMode) (*os.File, error)
	removeFile func(path string) error
}

// open opens the snapshot file at path for one read call.
func (m *readManager) open(path string) (snapshotFile, error) {
	if m.openSnapshot != nil {
		return m.openSnapshot(path)
	}
	return os.Open(path)
}

// newReadManager clears root and creates the snapshot directory of the new
// manager in it with a unique name. The unique name is the fence against a
// worker of an earlier manager that outlived its close: the worker keeps
// absolute paths into its own snapshot directory, which this function's
// RemoveAll deleted and never recreates under that name, so the worker
// cannot create or remove anything in the new manager's directory.
func newReadManager(root string, now func() time.Time, logger *slog.Logger) (*readManager, error) {
	// Snapshots of an earlier helper process are unreachable: no read_id
	// state survives a restart.
	if err := clearReadRoot(root, logger); err != nil {
		return nil, fmt.Errorf("guest: clear read snapshots: %w", err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("guest: create read snapshot directory: %w", err)
	}
	dir, err := os.MkdirTemp(root, snapshotDirPrefix)
	if err != nil {
		return nil, fmt.Errorf("guest: create read snapshot directory: %w", err)
	}
	return &readManager{
		stop:      make(chan struct{}),
		dir:       dir,
		entries:   map[string]*readEntry{},
		expired:   map[string]time.Time{},
		pending:   map[string]*readRemoval{},
		now:       now,
		retention: OperationRetention,
		logger:    logger,
	}, nil
}

// snapshotDirPrefix is the name prefix of the unique snapshot directory of one
// read manager.
const snapshotDirPrefix = "snapshots-"

// clearReadRoot removes every earlier snapshot below root.
//
// Workaround (Windows only): a snapshot worker of an earlier manager that
// outlived its close (see readManager.close) can still hold an open handle on
// a file in its own snapshots-* directory, and Windows refuses to delete a
// directory tree with an open handle. Failing here would keep the replacement
// helper from starting. On Windows, a snapshots-* directory that cannot be
// removed is therefore left in place with a warning: nothing refers to it, so
// it is unreachable, and the next start removes it once the handle is closed.
// Remove this tolerance if workers can no longer outlive close, or if the
// state directory is no longer shared by a replacement in the same process
// lifetime. Every other removal error still fails the start.
func clearReadRoot(root string, logger *slog.Logger) error {
	err := os.RemoveAll(root)
	if err == nil || runtime.GOOS != "windows" {
		return err
	}
	entries, readErr := os.ReadDir(root)
	if readErr != nil {
		return err
	}
	for _, entry := range entries {
		path := filepath.Join(root, entry.Name())
		rmErr := os.RemoveAll(path)
		if rmErr == nil {
			continue
		}
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), snapshotDirPrefix) {
			return rmErr
		}
		logger.Warn("old read snapshot directory is still in use and stays until the next start",
			"path", path, "error", rmErr)
	}
	return nil
}

// begin registers one call for readID. The first call starts the snapshot.
func (m *readManager) begin(readID, path string) (*readEntry, error) {
	if err := checkGuestPath(path); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, errReadsClosed()
	}
	if _, gone := m.expired[readID]; gone {
		return nil, errorf(ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND, "read expired")
	}
	removal := m.pending[readID]
	if removal != nil && removal.active {
		return nil, errReadRemovalPending()
	}
	if e, ok := m.entries[readID]; ok {
		if e.path != nativePath(path) {
			return nil, errorf(ownsandv1.ErrorCode_ERROR_CODE_ALREADY_EXISTS, "read_id is bound to another path")
		}
		e.open++
		return e, nil
	}
	// The new snapshot uses the same path as an earlier failed or expired
	// snapshot. Settle its pending removal before reusing the path.
	if removal != nil {
		m.wg.Add(1)
		m.removeSnapshot(readID, removal)
		m.wg.Done()
		if m.closed {
			return nil, errReadsClosed()
		}
		if removal.failed {
			return nil, errReadRemovalPending()
		}
	}
	e := &readEntry{
		path:     nativePath(path),
		snapshot: filepath.Join(m.dir, readID),
		ready:    make(chan struct{}),
		open:     1,
	}
	m.entries[readID] = e
	// Add under mu, so that close never waits while a worker is added.
	m.wg.Add(1)
	go m.prepare(readID, e)
	return e, nil
}

func errReadRemovalPending() error {
	return errorf(ownsandv1.ErrorCode_ERROR_CODE_UNAVAILABLE,
		"read: the earlier snapshot of this read_id could not be removed yet")
}

// close fences read admission and snapshot publication. It waits for
// snapshot work for at most grace, so that the caller can release the state
// directory lock afterwards. It reports whether all snapshot work ended.
//
// Filesystem calls can outlive close. A worker can finish a create reserved
// before the fence and remove its partial file. These operations address
// only the worker's unique snapshot directory. On Windows open handles can
// keep that directory until they close. A waiting handler is released by its
// own cancellation or the preparation result. close is safe to call more
// than once.
func (m *readManager) close(grace time.Duration) bool {
	m.mu.Lock()
	if !m.closed {
		m.closed = true
		close(m.stop)
	}
	m.mu.Unlock()
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		m.logger.Warn("read snapshot workers still running after close", "grace_ms", grace.Milliseconds())
		return false
	}
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
// m.mu so that cleanup is recorded before the last call can forget the
// failed entry. If the last call already ended, prepare forgets the entry.
func (m *readManager) prepare(readID string, e *readEntry) {
	defer m.wg.Done()
	kind, size, created, err := m.makeSnapshot(e.path, e.snapshot)
	m.mu.Lock()
	if m.closed {
		err = errReadsClosed()
	} else if err == nil && m.entries[readID] != e {
		// Defense in depth: sweep skips an entry whose snapshot is still
		// being prepared, so no path forgets the entry before this point.
		// Treat a snapshot with no entry as a preparation failure.
		err = errorf(ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND, "read expired")
	}
	if err != nil {
		// makeSnapshot has closed its handles. Only remove a file that it
		// created, and track a failed removal before publishing the error.
		if created {
			removal := &readRemoval{path: e.snapshot}
			m.pending[readID] = removal
			m.removeSnapshot(readID, removal)
			if m.closed {
				err = errReadsClosed()
			}
		}
		e.err = err
		// No call is left to receive the failure, and sweep keeps no
		// failed entry. Forget it so the next call can retry the read_id.
		if e.open == 0 && m.entries[readID] == e {
			delete(m.entries, readID)
		}
		close(e.ready)
		m.mu.Unlock()
		m.logger.Info("read snapshot failed", "read_id", readID, "error", err.Error())
		return
	}
	e.kind, e.size = kind, size
	if e.open == 0 {
		// No call is left, and the last call may have ended long before the
		// snapshot became ready. Start the retention time now, so that a
		// snapshot that succeeds after the last call ended stays available
		// for a later call of the same read_id for the whole retention time.
		e.lastEnd = m.now()
	}
	close(e.ready)
	m.mu.Unlock()
	m.logger.Info("read snapshot ready", "read_id", readID, "kind", kind.String(), "size_bytes", size)
}

// makeSnapshot reports whether it created dst, even if preparation failed.
func (m *readManager) makeSnapshot(src, dst string) (kind SnapshotKind, size int64, created bool, err error) {
	// statSource captures the identity of the file together with its size
	// and modification time, so that the comparisons below detect a path
	// that was swapped after this stat on every platform.
	before, err := statSource(src)
	if err != nil {
		return 0, 0, false, fsError("read", err)
	}
	if !before.Mode().IsRegular() {
		return 0, 0, false, errorf(ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION, "read: path is not a regular file")
	}
	if m.openHook != nil {
		m.openHook()
	}
	in, err := openSource(src)
	if err != nil {
		return 0, 0, false, fsError("read", err)
	}
	defer in.Close()
	// The pathname may have been swapped between the stat and the open, so
	// the descriptor that is copied must itself be the validated file.
	if inInfo, err := in.Stat(); err != nil || !inInfo.Mode().IsRegular() || !os.SameFile(before, inInfo) {
		return 0, 0, false, errorf(ownsandv1.ErrorCode_ERROR_CODE_ABORTED, "read: file changed during the snapshot copy")
	}
	// Reserve creation under mu. The registered worker owns dst and the
	// wait group accounts for its I/O. Close can fence publication while
	// the create is in progress.
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return 0, 0, false, errReadsClosed()
	}
	m.mu.Unlock()
	create := m.createFile
	if create == nil {
		create = os.OpenFile
	}
	out, err := create(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, 0, false, fsError("read snapshot", err)
	}
	defer out.Close()
	select {
	case <-m.stop:
		return 0, 0, true, errReadsClosed()
	default:
	}
	if !m.forceCopy && reflink(out, in) == nil {
		st, err := out.Stat()
		if err != nil {
			return 0, 0, true, fsError("read snapshot", err)
		}
		if !sourceUnchanged(before, in, src) {
			return 0, 0, true, errorf(ownsandv1.ErrorCode_ERROR_CODE_ABORTED, "read: file changed during the snapshot copy")
		}
		return SnapshotKindReflink, st.Size(), true, nil
	}
	// Check for close between copy steps. prepare removes partial output.
	var n int64
	for {
		select {
		case <-m.stop:
			return 0, 0, true, errReadsClosed()
		default:
		}
		c, err := io.CopyN(out, in, snapshotCopyChunk)
		n += c
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, 0, true, fsError("read snapshot", err)
		}
	}
	if m.copyHook != nil {
		m.copyHook()
	}
	if !sourceUnchanged(before, in, src) {
		return 0, 0, true, errorf(ownsandv1.ErrorCode_ERROR_CODE_ABORTED, "read: file changed during the snapshot copy")
	}
	if n != before.Size() && !allowsSnapshotSizeMismatch(in, before.Size()) {
		return 0, 0, true, errorf(ownsandv1.ErrorCode_ERROR_CODE_ABORTED, "read: file changed during the snapshot copy")
	}
	return SnapshotKindCopy, n, true, nil
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

// removeSnapshot starts an idle pending removal. The caller holds m.mu on
// entry and return and accounts for the work in m.wg. Keep the path reserved
// while the mutex is unlocked for I/O.
func (m *readManager) removeSnapshot(readID string, removal *readRemoval) {
	retry := removal.failed
	removal.active = true
	m.mu.Unlock()
	remove := m.removeFile
	if remove == nil {
		remove = os.Remove
	}
	err := remove(removal.path)
	removed := err == nil || errors.Is(err, os.ErrNotExist)
	if removed && retry {
		m.logger.Info("read snapshot removed after retry", "read_id", readID)
	}
	if !removed && !retry {
		// The error is reduced to its reason: the helper never logs paths.
		m.logger.Warn("remove read snapshot failed", "read_id", readID, "error", sanitizedError(err))
	}
	m.mu.Lock()
	removal.active = false
	removal.failed = !removed
	if removed {
		delete(m.pending, readID)
	}
}

// sweep deletes snapshots whose retention time has passed since the later of
// the last call end and the moment the snapshot became ready, and forgets
// expired IDs after ExpiredMemory. It never deletes an entry whose snapshot is
// still being prepared: the worker has not created the snapshot file yet or
// still writes it, so deleting the entry would leave a file that nothing
// tracks.
func (m *readManager) sweep(now time.Time) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	var expired []string
	for id, e := range m.entries {
		if e.open > 0 || now.Sub(e.lastEnd) < m.retention {
			continue
		}
		select {
		case <-e.ready:
		default:
			continue
		}
		delete(m.entries, id)
		m.pending[id] = &readRemoval{path: e.snapshot}
		m.expired[id] = now
		expired = append(expired, id)
	}
	for id, at := range m.expired {
		if now.Sub(at) >= ExpiredMemory {
			delete(m.expired, id)
		}
	}
	removals := maps.Clone(m.pending)
	m.mu.Unlock()
	for _, id := range expired {
		m.logger.Info("read expired", "read_id", id)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	// Recheck each reservation after unlocking. A reader or another sweep
	// can have removed it, and a failed read can still have callers.
	for id, removal := range removals {
		if m.closed {
			return
		}
		if m.pending[id] != removal || removal.active || m.entries[id] != nil {
			continue
		}
		m.wg.Add(1)
		m.removeSnapshot(id, removal)
		m.wg.Done()
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
