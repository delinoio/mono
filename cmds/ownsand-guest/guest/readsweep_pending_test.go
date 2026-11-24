// SPDX-License-Identifier: FSL-1.1-ALv2

package guest

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// flakyRemover fails removals while fail is set and otherwise removes the file
// for real. The tests drive sweep with explicit times, so they use no timers.
type flakyRemover struct {
	fail  bool
	calls int
}

func (f *flakyRemover) remove(path string) error {
	f.calls++
	if f.fail {
		return &fs.PathError{Op: "remove", Path: path, Err: errors.New("device busy")}
	}
	return os.Remove(path)
}

// pendingFixture is a manager with one expired read whose snapshot file
// exists.
type pendingFixture struct {
	m     *readManager
	rm    *flakyRemover
	snap  string
	end   time.Time
	out   func() string
	count func(substr string) int
}

func newPendingFixture(t *testing.T, readID string) *pendingFixture {
	t.Helper()
	logger, buf := newBufferLogger()
	m, err := newReadManager(filepath.Join(t.TempDir(), "reads"), time.Now, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.close(time.Second) })
	rm := &flakyRemover{fail: true}
	m.removeFile = rm.remove
	snap := filepath.Join(m.dir, readID)
	if err := os.WriteFile(snap, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	end := time.Now()
	m.entries[readID] = settledReadEntry(snap, end)
	return &pendingFixture{
		m: m, rm: rm, snap: snap, end: end,
		out:   buf.String,
		count: func(substr string) int { return strings.Count(buf.String(), substr) },
	}
}

func (f *pendingFixture) exists(t *testing.T) bool {
	t.Helper()
	_, err := os.Lstat(f.snap)
	if err == nil {
		return true
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	return false
}

func TestReadSweepRetriesFailedRemoval(t *testing.T) {
	f := newPendingFixture(t, "read-1")
	m := f.m
	first := f.end.Add(m.retention)
	m.sweep(first)

	if !f.exists(t) {
		t.Fatal("snapshot was removed although the removal failed")
	}
	if got := m.pending["read-1"]; got != f.snap {
		t.Fatalf("pending = %q, want the snapshot path", got)
	}
	if _, ok := m.entries["read-1"]; ok {
		t.Fatal("entry stayed after expiry")
	}
	// The read_id fails as expired while the removal is still pending.
	_, err := m.begin("read-1", filepath.Join(t.TempDir(), "src"))
	if detailOf(err).GetCode() != ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND || !strings.Contains(err.Error(), "read expired") {
		t.Fatalf("begin = %v, want read expired", err)
	}

	// A sweep whose retry still fails keeps the path and logs no new warning.
	m.sweep(first.Add(time.Second))
	if len(m.pending) != 1 || !f.exists(t) {
		t.Fatal("failed retry dropped the pending removal")
	}
	if n := f.count("remove read snapshot failed"); n != 1 {
		t.Fatalf("warning count = %d, want 1: %q", n, f.out())
	}

	// The next sweep after the failure cleared removes the file.
	f.rm.fail = false
	m.sweep(first.Add(2 * time.Second))
	if f.exists(t) {
		t.Fatal("snapshot was not removed by the retry")
	}
	if len(m.pending) != 0 {
		t.Fatalf("pending = %v, want empty", m.pending)
	}
	if _, expired := m.expired["read-1"]; !expired {
		t.Fatal("the expired memory of the read_id was lost")
	}
	if n := f.count("remove read snapshot failed"); n != 1 {
		t.Fatalf("warning count = %d, want 1: %q", n, f.out())
	}

	// A cleared removal is not retried again.
	calls := f.rm.calls
	m.sweep(first.Add(3 * time.Second))
	if f.rm.calls != calls {
		t.Fatal("a cleared removal was retried")
	}
	for _, secret := range []string{f.snap, m.dir} {
		if strings.Contains(f.out(), secret) {
			t.Fatalf("log leaks %q: %q", secret, f.out())
		}
	}
}

func TestReadSweepRetriesPastExpiredMemory(t *testing.T) {
	f := newPendingFixture(t, "read-1")
	m := f.m
	first := f.end.Add(m.retention)
	m.sweep(first)

	// The expired memory ends while the removal keeps failing.
	later := first.Add(ExpiredMemory)
	m.sweep(later)
	if _, ok := m.expired["read-1"]; ok {
		t.Fatal("the read_id stayed in the expired memory past ExpiredMemory")
	}
	if len(m.pending) != 1 || !f.exists(t) {
		t.Fatal("the pending removal ended with the expired memory")
	}
	m.sweep(later.Add(time.Minute))
	if n := f.count("remove read snapshot failed"); n != 1 {
		t.Fatalf("warning count = %d, want 1: %q", n, f.out())
	}

	f.rm.fail = false
	m.sweep(later.Add(2 * time.Minute))
	if f.exists(t) || len(m.pending) != 0 {
		t.Fatalf("snapshot exists = %v, pending = %v after the failure cleared", f.exists(t), m.pending)
	}
}

func TestReadSweepAbsentFileClearsPending(t *testing.T) {
	f := newPendingFixture(t, "read-1")
	m := f.m
	first := f.end.Add(m.retention)
	m.sweep(first)
	if len(m.pending) != 1 {
		t.Fatal("removal was not left pending")
	}

	// Somebody else removed the file; the retry sees ErrNotExist.
	f.rm.fail = false
	if err := os.Remove(f.snap); err != nil {
		t.Fatal(err)
	}
	m.sweep(first.Add(time.Second))
	if len(m.pending) != 0 {
		t.Fatalf("pending = %v, want empty", m.pending)
	}
	if n := f.count("remove read snapshot failed"); n != 1 {
		t.Fatalf("warning count = %d, want 1: %q", n, f.out())
	}
}

func TestReadSweepImmediateSuccessLeavesNothingPending(t *testing.T) {
	f := newPendingFixture(t, "read-1")
	m := f.m
	f.rm.fail = false
	m.sweep(f.end.Add(m.retention))
	if f.exists(t) || len(m.pending) != 0 {
		t.Fatalf("snapshot exists = %v, pending = %v", f.exists(t), m.pending)
	}
	if strings.Contains(f.out(), "level=WARN") {
		t.Fatalf("unexpected warning: %q", f.out())
	}
}

func TestReadSweepPendingSkipsLiveEntries(t *testing.T) {
	f := newPendingFixture(t, "read-1")
	m := f.m
	// An entry that is active, or whose snapshot is still being prepared, is
	// never removed, and a pending path of its read_id is not retried.
	f.rm.fail = false
	active := settledReadEntry(f.snap, f.end)
	active.open = 1
	m.entries["read-1"] = active
	m.pending["read-1"] = f.snap
	preparing := filepath.Join(m.dir, "read-2")
	if err := os.WriteFile(preparing, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	m.entries["read-2"] = &readEntry{snapshot: preparing, ready: make(chan struct{}), lastEnd: f.end}

	m.sweep(f.end.Add(2 * m.retention))
	if f.rm.calls != 0 {
		t.Fatalf("remove was called %d times for live entries", f.rm.calls)
	}
	if !f.exists(t) {
		t.Fatal("the snapshot of an active read was removed")
	}
	if _, err := os.Lstat(preparing); err != nil {
		t.Fatalf("the snapshot being prepared was removed: %v", err)
	}
}

func TestReadBeginSettlesPendingRemovalFirst(t *testing.T) {
	f := newPendingFixture(t, "read-1")
	m := f.m
	src := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(src, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	first := f.end.Add(m.retention)
	m.sweep(first)
	// Let the expired memory end while the removal keeps failing.
	m.sweep(first.Add(ExpiredMemory))
	if _, ok := m.expired["read-1"]; ok || len(m.pending) != 1 {
		t.Fatalf("expired = %v, pending = %v", m.expired, m.pending)
	}

	// The read_id is free again, but the old file still blocks its path.
	_, err := m.begin("read-1", src)
	if detailOf(err).GetCode() != ownsandv1.ErrorCode_ERROR_CODE_UNAVAILABLE {
		t.Fatalf("begin = %v, want unavailable", err)
	}
	if _, ok := m.entries["read-1"]; ok {
		t.Fatal("begin registered an entry although the old removal is pending")
	}
	if strings.Contains(err.Error(), f.snap) {
		t.Fatalf("error leaks the path: %v", err)
	}

	// Once the removal works, begin clears it and starts a new snapshot that
	// a later sweep retry does not delete.
	f.rm.fail = false
	e, err := m.begin("read-1", src)
	if err != nil {
		t.Fatal(err)
	}
	<-e.ready
	if e.err != nil {
		t.Fatal(e.err)
	}
	m.end("read-1", e)
	if len(m.pending) != 0 {
		t.Fatalf("pending = %v, want empty", m.pending)
	}
	if !f.exists(t) {
		t.Fatal("the new snapshot is missing")
	}
	// The new entry ended just now, so a sweep at the current time keeps it.
	m.sweep(m.now())
	if !f.exists(t) {
		t.Fatal("a sweep removed the live snapshot of the new read")
	}
}

func TestReadPreparationRetriesFailedRemoval(t *testing.T) {
	for _, retry := range []string{"sweep", "already-absent", "same-ID admission"} {
		t.Run(retry, func(t *testing.T) {
			h := newHarness(t, nil)
			m := h.srv.reads
			logger, logs := newBufferLogger()
			m.logger = logger
			m.forceCopy = true
			gate, started, releaseCopy := newReadPreparationGate(t)
			m.copyHook = gate

			path := filepath.Join(t.TempDir(), "private-source")
			if err := os.WriteFile(path, []byte("before"), 0o600); err != nil {
				t.Fatal(err)
			}
			req := readReq(t, path)
			id := req.GetReadId()
			snap := filepath.Join(m.dir, id)
			remover := &flakyRemover{fail: true}
			if runtime.GOOS != "windows" {
				m.removeFile = remover.remove
			}

			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			s := h.open(ctx)
			defer s.s.CloseRequest()
			s.send(1, req)
			waitStarted(t, started)
			waiter, err := m.begin(id, path)
			if err != nil {
				t.Fatal(err)
			}
			unblockRemoval := func() { remover.fail = false }
			if runtime.GOOS == "windows" {
				// A Windows os.Open handle denies delete sharing.
				locked, err := os.Open(snap)
				if err != nil {
					t.Fatal(err)
				}
				defer locked.Close()
				unblockRemoval = func() {
					if err := locked.Close(); err != nil {
						t.Fatal(err)
					}
				}
			}
			const updated = "after, longer"
			if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
				t.Fatal(err)
			}
			releaseCopy()

			const abortMessage = "read: file changed during the snapshot copy"
			if got := s.recv().GetError(); got.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_ABORTED || got.GetMessage() != abortMessage {
				t.Fatalf("read returned %v, want the original preparation error", got)
			}
			waitClosed(t, waiter.ready)
			if got := detailOf(waiter.err); got.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_ABORTED || got.GetMessage() != abortMessage {
				t.Fatalf("waiting caller got %v, want the original preparation error", got)
			}
			m.end(id, waiter)
			h.waitReadCallsEnded(id)
			if m.hasEntry(id) {
				t.Fatal("failed entry stayed after the last call ended")
			}

			m.sweep(h.clock.Now())
			if _, err := os.Lstat(snap); err != nil || m.pending[id] != snap {
				t.Fatalf("failed sweep lost the snapshot or pending removal: stat error %v, pending %v", err, m.pending)
			}
			if _, _, got := h.readAll(req); got.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_UNAVAILABLE {
				t.Fatalf("retry while removal fails returned %v, want unavailable", got)
			}
			if got := logs.String(); strings.Count(got, "remove read snapshot failed") != 1 ||
				strings.Contains(got, filepath.Base(m.dir)) || strings.Contains(got, filepath.Base(path)) ||
				strings.Contains(got, updated) || strings.Contains(got, "before") {
				t.Fatalf("want one warning and no paths or file contents in the logs, got %q", got)
			}

			m.copyHook = nil
			unblockRemoval()
			if retry == "already-absent" {
				if err := os.Remove(snap); err != nil {
					t.Fatal(err)
				}
			}
			if retry != "same-ID admission" {
				m.sweep(h.clock.Now())
				if _, err := os.Lstat(snap); !errors.Is(err, fs.ErrNotExist) || len(m.pending) != 0 {
					t.Fatalf("sweep did not clear the snapshot and pending removal: stat error %v, pending %v", err, m.pending)
				}
			}
			if data, size, got := h.readAll(req); got != nil || string(data) != updated || size != int64(len(updated)) {
				t.Fatalf("retry after cleanup returned %q, size %d, error %v", data, size, got)
			}
			h.waitReadCallsEnded(id)
			m.sweep(h.clock.Now())
			if data, err := os.ReadFile(snap); err != nil || string(data) != updated || len(m.pending) != 0 {
				t.Fatalf("new snapshot after sweep contains %q, error %v, pending %v", data, err, m.pending)
			}
		})
	}
}

func TestReadPreparationPreservesExistingDestination(t *testing.T) {
	h := newHarness(t, nil)
	m := h.srv.reads
	path := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(path, []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	req := readReq(t, path)
	id := req.GetReadId()
	snap := filepath.Join(m.dir, id)
	if err := os.WriteFile(snap, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, got := h.readAll(req); got.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_INTERNAL {
		t.Fatalf("exclusive creation returned %v, want the original filesystem error", got)
	}
	h.waitReadCallsEnded(id)
	m.sweep(h.clock.Now())
	if data, err := os.ReadFile(snap); err != nil || string(data) != "existing" || len(m.pending) != 0 {
		t.Fatalf("preexisting destination contains %q, error %v, pending %v", data, err, m.pending)
	}
}
