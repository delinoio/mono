// SPDX-License-Identifier: FSL-1.1-ALv2

package guest

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
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

// readCloseGrace is how long Close waits for in-flight snapshot workers. A
// blocking stat or open of a stalled source cannot be cancelled, so the wait
// is bounded; after it, the unique snapshot directory fences the worker (see
// readManager.close). Tests shorten it.
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

// readManager keeps one private snapshot per read_id.
type readManager struct {
	mu sync.Mutex
	// dir is the snapshot directory of this manager: a directory with a
	// unique name inside the reads directory of the state directory (see
	// newReadManager).
	dir     string
	entries map[string]*readEntry
	expired map[string]time.Time
	// pending maps a read_id to the snapshot path whose removal failed. It is
	// independent of expired: sweep retries the removal on every run until it
	// succeeds or the file is already gone, also after the read_id left the
	// expired memory. Guarded by mu.
	pending   map[string]string
	now       func() time.Time
	retention time.Duration
	logger    *slog.Logger
	// closed is set by close and guarded by mu; once it is set, no worker
	// creates a snapshot file and begin fails. stop is closed together with
	// it and ends the copy of a running worker; wg counts the workers.
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
	// removeFile removes a snapshot file; nil means os.Remove. Tests replace
	// it to fail a removal.
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
		pending:   map[string]string{},
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
	if e, ok := m.entries[readID]; ok {
		if e.path != nativePath(path) {
			return nil, errorf(ownsandv1.ErrorCode_ERROR_CODE_ALREADY_EXISTS, "read_id is bound to another path")
		}
		e.open++
		return e, nil
	}
	// A removal that failed for an earlier snapshot of this read_id is still
	// pending once the expired memory ended. The new snapshot uses the same
	// path, so settle the old removal first; otherwise a later retry would
	// delete the live snapshot.
	if path, ok := m.pending[readID]; ok {
		m.removeSnapshot(readID, path)
		if _, ok := m.pending[readID]; ok {
			return nil, errorf(ownsandv1.ErrorCode_ERROR_CODE_UNAVAILABLE,
				"read: the earlier snapshot of this read_id could not be removed yet")
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

// close ends the snapshot work of this manager. After it returns, begin
// fails and no worker creates a snapshot file. It stops the copy of running
// workers, which remove their partial output, and waits for them for at most
// grace, so that the caller can release the state directory lock afterwards.
// It reports whether every worker ended.
//
// Behavior when source I/O is still pending (decision): a blocking stat or
// open of the source cannot be cancelled, so close does not wait for it
// beyond grace. Such a worker is harmless to a replacement helper: it cannot
// create a file after the closed flag was set, and its removals address its
// own unique snapshot directory only. Its open handles can keep that old
// directory on Windows until they close. A handler that waits for the
// snapshot is released by its own cancellation, not by close. close is safe
// to call more than once.
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
// m.mu so that it is ordered against end(): a failure published after the
// last call ended removes the entry here, and a failure published before it
// is removed by end(). Either way a retry of the read_id snapshots again.
func (m *readManager) prepare(readID string, e *readEntry) {
	defer m.wg.Done()
	kind, size, err := m.makeSnapshot(e.path, e.snapshot)
	if err != nil {
		os.Remove(e.snapshot)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	defer close(e.ready)
	if err == nil && m.entries[readID] != e {
		// Defense in depth: sweep skips an entry whose snapshot is still
		// being prepared, so no path forgets the entry before this point.
		// If one ever does, nothing tracks the file that makeSnapshot just
		// created, and no later sweep could find it. Remove it here.
		m.removeSnapshot(readID, e.snapshot)
		err = errorf(ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND, "read expired")
	}
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
	if e.open == 0 {
		// No call is left, and the last call may have ended long before the
		// snapshot became ready. Start the retention time now, so that a
		// snapshot that succeeds after the last call ended stays available
		// for a later call of the same read_id for the whole retention time.
		e.lastEnd = m.now()
	}
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
	in, err := openSource(src)
	if err != nil {
		return 0, 0, fsError("read", err)
	}
	defer in.Close()
	// The pathname may have been swapped between the stat and the open, so
	// the descriptor that is copied must itself be the validated file.
	if inInfo, err := in.Stat(); err != nil || !inInfo.Mode().IsRegular() || !os.SameFile(before, inInfo) {
		return 0, 0, errorf(ownsandv1.ErrorCode_ERROR_CODE_ABORTED, "read: file changed during the snapshot copy")
	}
	// Check and create under mu, so that close cannot interleave: once close
	// set the flag, no snapshot file is created.
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return 0, 0, errReadsClosed()
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	m.mu.Unlock()
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
	// Copy in steps, so that close ends the copy and prepare removes the
	// partial output before the lock of the state directory is released.
	var n int64
	for {
		select {
		case <-m.stop:
			return 0, 0, errReadsClosed()
		default:
		}
		c, err := io.CopyN(out, in, snapshotCopyChunk)
		n += c
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, 0, fsError("read snapshot", err)
		}
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

// removeSnapshot removes the snapshot file of readID. The caller holds m.mu.
// A failure keeps the path in m.pending, so that a later sweep retries it;
// success or an already absent file clears it. The warning is logged once per
// pending removal, so retries of a persistent failure do not repeat it.
func (m *readManager) removeSnapshot(readID, snapshot string) {
	remove := m.removeFile
	if remove == nil {
		remove = os.Remove
	}
	_, retry := m.pending[readID]
	err := remove(snapshot)
	if err == nil || errors.Is(err, os.ErrNotExist) {
		delete(m.pending, readID)
		if retry {
			m.logger.Info("read snapshot removed after retry", "read_id", readID)
		}
		return
	}
	m.pending[readID] = snapshot
	if !retry {
		// The error is reduced to its reason: the helper never logs paths.
		m.logger.Warn("remove read snapshot failed", "read_id", readID, "error", sanitizedError(err))
	}
}

// sweep deletes snapshots whose retention time has passed since the later of
// the last call end and the moment the snapshot became ready, and forgets
// expired IDs after ExpiredMemory. It never deletes an entry whose snapshot is
// still being prepared: the worker has not created the snapshot file yet or
// still writes it, so deleting the entry would leave a file that nothing
// tracks. A preparation always ends, so the entry is swept after that.
func (m *readManager) sweep(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
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
		m.removeSnapshot(id, e.snapshot)
		m.expired[id] = now
		m.logger.Info("read expired", "read_id", id)
	}
	// Retry removals that failed earlier, whether or not the read_id is still
	// in the expired memory. An id with a live entry cannot be pending (begin
	// settles the removal first); skip it anyway, so that a retry can never
	// delete the snapshot of an active or still prepared read.
	for id, path := range m.pending {
		if _, live := m.entries[id]; live {
			continue
		}
		m.removeSnapshot(id, path)
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
