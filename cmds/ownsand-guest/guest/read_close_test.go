// SPDX-License-Identifier: FSL-1.1-ALv2

package guest

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func setReadCloseGrace(t *testing.T, d time.Duration) {
	t.Helper()
	orig := readCloseGrace
	readCloseGrace = d
	t.Cleanup(func() { readCloseGrace = orig })
}

// filesUnder lists every regular file below root.
func filesUnder(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func newCloseTestServer(t *testing.T, dir string) *Server {
	t.Helper()
	srv, err := New(Config{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	srv.reads.forceCopy = true
	t.Cleanup(func() { srv.Close() })
	return srv
}

// A worker that is paused before it creates its snapshot file must not create
// one in the state directory of a replacement helper, after the first server
// was closed and its lock released.
func TestReadWorkerCannotCreateAfterClose(t *testing.T) {
	setReadCloseGrace(t, 50*time.Millisecond)
	dir := t.TempDir()
	src := filepath.Join(t.TempDir(), "src")
	if err := os.WriteFile(src, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	first := newCloseTestServer(t, dir)
	hook, started, release := newReadPreparationGate(t)
	first.reads.openHook = hook
	const readID = "read-close-create"
	e, err := first.reads.begin(readID, src)
	if err != nil {
		t.Fatal(err)
	}
	waitStarted(t, started)
	// The handler is cancelled while preparation is pending.
	first.reads.end(readID, e)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := first.reads.begin("late", src); err == nil {
		t.Fatal("a closed read manager accepted a new read")
	}

	second := newCloseTestServer(t, dir)
	release()
	waitClosed(t, e.ready)
	first.reads.wg.Wait()
	var op *opError
	if !errors.As(e.err, &op) || op.code != ownsandv1.ErrorCode_ERROR_CODE_UNAVAILABLE {
		t.Fatalf("snapshot error after close: %v, want unavailable", e.err)
	}
	if files := filesUnder(t, filepath.Join(dir, "reads")); len(files) != 0 {
		t.Fatalf("old worker wrote into the replacement state directory: %v", files)
	}
	// The replacement works.
	if _, err := second.reads.begin("fresh", src); err != nil {
		t.Fatalf("replacement read: %v", err)
	}
}

// Close waits for a worker that ends within the grace, and a handler that
// waits for the snapshot is released by the snapshot result.
func TestReadCloseWaitsForWorkerWithinGrace(t *testing.T) {
	setReadCloseGrace(t, 10*time.Second)
	dir := t.TempDir()
	src := filepath.Join(t.TempDir(), "src")
	if err := os.WriteFile(src, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := newCloseTestServer(t, dir)
	hook, started, release := newReadPreparationGate(t)
	srv.reads.openHook = hook
	e, err := srv.reads.begin("read-close-wait", src)
	if err != nil {
		t.Fatal(err)
	}
	waitStarted(t, started)
	go func() {
		time.Sleep(50 * time.Millisecond)
		release()
	}()
	if !srv.reads.close(readCloseGrace) {
		t.Fatal("close gave up although the worker ended within the grace")
	}
	waitClosed(t, e.ready)
	if e.err == nil {
		t.Fatal("snapshot succeeded although close ended the worker before it created the file")
	}
	if files := filesUnder(t, filepath.Join(dir, "reads")); len(files) != 0 {
		t.Fatalf("files left after close: %v", files)
	}
}

// A late cleanup of an old worker must keep the snapshot of the same read_id
// in the replacement helper.
func TestReadLateCleanupKeepsReplacementSnapshot(t *testing.T) {
	setReadCloseGrace(t, 50*time.Millisecond)
	dir := t.TempDir()
	src := filepath.Join(t.TempDir(), "src")
	if err := os.WriteFile(src, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	first := newCloseTestServer(t, dir)
	hook, started, release := newReadPreparationGate(t)
	// The worker pauses after its copy, so it already created its file.
	first.reads.copyHook = hook
	const readID = "read-retry"
	e1, err := first.reads.begin(readID, src)
	if err != nil {
		t.Fatal(err)
	}
	waitStarted(t, started)
	first.reads.end(readID, e1)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second := newCloseTestServer(t, dir)
	e2, err := second.reads.begin(readID, src)
	if err != nil {
		t.Fatal(err)
	}
	waitClosed(t, e2.ready)
	if e2.err != nil {
		t.Fatalf("replacement snapshot: %v", e2.err)
	}
	// The source changes, so the old worker fails its check and removes its
	// own snapshot path.
	if err := os.WriteFile(src, []byte("changed after the copy"), 0o644); err != nil {
		t.Fatal(err)
	}
	release()
	waitClosed(t, e1.ready)
	first.reads.wg.Wait()
	if e1.err == nil {
		t.Fatal("old worker published a snapshot after the source changed")
	}
	got, err := os.ReadFile(e2.snapshot)
	if err != nil || string(got) != "0123456789" {
		t.Fatalf("replacement snapshot after the late cleanup: %q, %v", got, err)
	}
	if files := filesUnder(t, filepath.Join(dir, "reads")); len(files) != 1 {
		t.Fatalf("files after the late cleanup: %v, want only the replacement snapshot", files)
	}
}

func TestReadShutdownHandsOffWhileSnapshotIOBlocked(t *testing.T) {
	for _, phase := range []string{"create", "created", "failed preparation", "expiry", "admission retry"} {
		t.Run(phase, func(t *testing.T) {
			const grace = 40 * time.Millisecond
			setReadCloseGrace(t, grace)
			dir := t.TempDir()
			first := newHarness(t, func(cfg *Config) { cfg.StateDir = dir })
			m := first.srv.reads
			m.forceCopy = true
			src := filepath.Join(t.TempDir(), "src")
			payload := "original"
			if err := os.WriteFile(src, []byte(payload), 0o600); err != nil {
				t.Fatal(err)
			}
			req := readReq(t, src)
			oldSnapshot := filepath.Join(m.dir, req.GetReadId())

			gate, entered, release := newReadPreparationGate(t)
			defer release()
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			var call *stream
			var swept chan struct{}
			remove := func(path string) error {
				if path != oldSnapshot {
					t.Errorf("old helper removed %q, want %q", path, oldSnapshot)
				}
				gate()
				return os.Remove(path)
			}

			switch phase {
			case "create", "created":
				m.createFile = func(path string, flag int, perm os.FileMode) (*os.File, error) {
					if phase == "create" {
						gate()
						return os.OpenFile(path, flag, perm)
					}
					f, err := os.OpenFile(path, flag, perm)
					if err != nil {
						t.Errorf("snapshot create: %v", err)
					}
					gate()
					return f, err
				}
			case "failed preparation":
				payload = "changed to invalidate the first copy"
				m.copyHook = func() {
					if err := os.WriteFile(src, []byte(payload), 0o600); err != nil {
						t.Error(err)
					}
				}
				m.removeFile = remove
			case "expiry", "admission retry":
				requireReadPayload(t, first, req, payload)
				first.waitReadCallsEnded(req.GetReadId())
				first.clock.Advance(OperationRetention)
				if phase == "expiry" {
					m.removeFile = remove
					swept = make(chan struct{})
					t.Cleanup(func() { waitReadShutdownStep(t, swept, "old sweep cleanup") })
					go func() {
						defer close(swept)
						first.srv.Sweep()
					}()
				} else {
					m.removeFile = func(string) error { return fs.ErrPermission }
					first.srv.Sweep()
					first.clock.Advance(ExpiredMemory)
					first.srv.Sweep()
					m.removeFile = remove
				}
			}
			if swept == nil {
				call = first.open(ctx)
				defer call.s.CloseRequest()
				call.send(1, req)
			}
			waitReadShutdownStep(t, entered, "filesystem call entry")

			closed := make(chan struct{})
			start := time.Now()
			var elapsed time.Duration
			go func() {
				first.srv.Close()
				elapsed = time.Since(start)
				close(closed)
			}()
			waitReadShutdownStep(t, m.stop, "read admission fence")
			if _, _, got := first.readAll(readReq(t, src)); got.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_UNAVAILABLE {
				t.Fatalf("new read after close began: %v, want unavailable", got)
			}
			waitReadShutdownStep(t, closed, "close with snapshot I/O still blocked")
			if elapsed < grace {
				t.Fatalf("close returned in %v with unfinished work, want at least %v", elapsed, grace)
			}

			replacement := newHarness(t, func(cfg *Config) { cfg.StateDir = dir })
			requireReadPayload(t, replacement, req, payload)
			release()
			if swept != nil {
				waitReadShutdownStep(t, swept, "old sweep completion")
			} else {
				if got := call.recv().GetError(); got.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_UNAVAILABLE {
					t.Fatalf("old read after I/O returned: %v, want unavailable", got)
				}
			}
			requireReadPayload(t, replacement, req, payload)
			if files := filesUnder(t, filepath.Join(dir, "reads")); len(files) != 1 {
				t.Fatalf("snapshot files after old I/O completed: %v, want only the replacement snapshot", files)
			}
			t.Logf("close returned after %v with snapshot I/O blocked; replacement read the same ID before and after release", elapsed)
		})
	}
}

func TestReadPendingRemovalReservesIDAfterExpiry(t *testing.T) {
	h := newHarness(t, nil)
	src := filepath.Join(t.TempDir(), "src")
	const original, updated = "original", "updated after expiry"
	if err := os.WriteFile(src, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	req := readReq(t, src)
	requireReadPayload(t, h, req, original)
	h.waitReadCallsEnded(req.GetReadId())

	gate, entered, release := newReadPreparationGate(t)
	defer release()
	h.srv.reads.removeFile = func(path string) error {
		gate()
		return os.Remove(path)
	}
	h.clock.Advance(OperationRetention)
	swept := make(chan struct{})
	t.Cleanup(func() { waitReadShutdownStep(t, swept, "expiry removal cleanup") })
	go func() {
		defer close(swept)
		h.srv.Sweep()
	}()
	waitReadShutdownStep(t, entered, "expiry removal entry")
	if _, _, got := h.readAll(req); got.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND {
		t.Fatalf("read during expiry memory: %v, want not_found", got)
	}

	h.clock.Advance(ExpiredMemory)
	memorySwept := make(chan struct{})
	t.Cleanup(func() { waitReadShutdownStep(t, memorySwept, "expiry memory sweep cleanup") })
	go func() {
		defer close(memorySwept)
		h.srv.Sweep()
	}()
	waitReadShutdownStep(t, memorySwept, "expiry memory sweep with removal still active")
	if _, _, got := h.readAll(req); got.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_UNAVAILABLE {
		t.Fatalf("read after expiry memory with removal still active: %v, want unavailable", got)
	}
	if err := os.WriteFile(src, []byte(updated), 0o600); err != nil {
		t.Fatal(err)
	}
	requireReadPayload(t, h, readReq(t, src), updated)

	release()
	waitReadShutdownStep(t, swept, "expiry removal completion")
	requireReadPayload(t, h, req, updated)
	h.waitReadCallsEnded(req.GetReadId())
	h.srv.Sweep()
	requireReadPayload(t, h, req, updated)
}

func waitReadShutdownStep(t *testing.T, done <-chan struct{}, step string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", step)
	}
}

func requireReadPayload(t *testing.T, h *harness, req *ownsandv1.ReadFileRequest, want string) {
	t.Helper()
	data, size, got := h.readAll(req)
	if got != nil || string(data) != want || size != int64(len(want)) {
		t.Fatalf("read returned %q, size %d, error %v, want %q", data, size, got, want)
	}
}
