// SPDX-License-Identifier: FSL-1.1-ALv2

package guest

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// readAll runs one ReadFile call and returns the bytes, the final size and
// the error frame, if any.
func (h *harness) readAll(req *ownsandv1.ReadFileRequest) ([]byte, int64, *ownsandv1.ErrorDetail) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s := h.open(ctx)
	defer s.s.CloseRequest()
	s.send(3, req)
	var data []byte
	next := req.GetOffset()
	for {
		f := s.recv()
		if f.GetError() != nil {
			return data, 0, f.GetError()
		}
		ev := f.GetFileReadEvent()
		switch {
		case ev.GetChunk() != nil:
			c := ev.GetChunk()
			if c.GetOffset() != next || len(c.GetData()) > 1<<20 {
				h.t.Fatalf("chunk offset %d size %d, want offset %d", c.GetOffset(), len(c.GetData()), next)
			}
			next += int64(len(c.GetData()))
			data = append(data, c.GetData()...)
		case ev.GetEnd() != nil:
			return data, ev.GetEnd().GetSizeBytes(), nil
		}
	}
}

func readReq(t *testing.T, path string) *ownsandv1.ReadFileRequest {
	return &ownsandv1.ReadFileRequest{SandboxId: testSandbox, ReadId: newID(t), Path: path, BindingSalt: make([]byte, 32)}
}

func randomBytes(t *testing.T, n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestReadFileChunksAndResume(t *testing.T) {
	h := newHarness(t, nil)
	path := filepath.Join(t.TempDir(), "data.bin")
	content := randomBytes(t, 2<<20+12345)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	req := readReq(t, path)
	data, size, errd := h.readAll(req)
	if errd != nil || size != int64(len(content)) || !bytes.Equal(data, content) {
		t.Fatalf("read: %d bytes size %d err %v", len(data), size, errd)
	}
	// The original changes; a resumed read of the same read_id returns the
	// snapshot.
	if err := os.WriteFile(path, []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	req.Offset = 1 << 20
	data, size, errd = h.readAll(req)
	if errd != nil || size != int64(len(content)) || !bytes.Equal(data, content[1<<20:]) {
		t.Fatalf("resumed read: %d bytes err %v", len(data), errd)
	}
	req.Offset = size
	if data, _, errd := h.readAll(req); errd != nil || len(data) != 0 {
		t.Fatalf("read at end: %d %v", len(data), errd)
	}
	req.Offset = size + 1
	if _, _, errd := h.readAll(req); errd.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_OUT_OF_RANGE {
		t.Fatalf("read beyond end: %v", errd)
	}
	other := proto.Clone(req).(*ownsandv1.ReadFileRequest)
	other.Path = filepath.Join(filepath.Dir(path), "other")
	other.Offset = 0
	if _, _, errd := h.readAll(other); errd.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_ALREADY_EXISTS {
		t.Fatalf("other path: %v", errd)
	}
}

func TestReadFileErrors(t *testing.T) {
	h := newHarness(t, nil)
	dir := t.TempDir()
	if _, _, errd := h.readAll(readReq(t, filepath.Join(dir, "missing"))); errd.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND {
		t.Fatalf("missing: %v", errd)
	}
	if _, _, errd := h.readAll(readReq(t, dir)); errd.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION {
		t.Fatalf("directory: %v", errd)
	}
	if _, _, errd := h.readAll(readReq(t, "relative/path")); errd.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT {
		t.Fatalf("relative: %v", errd)
	}
	// A failed snapshot is forgotten, so a retry of the same read_id works.
	req := readReq(t, filepath.Join(dir, "later"))
	if _, _, errd := h.readAll(req); errd.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND {
		t.Fatalf("missing: %v", errd)
	}
	os.WriteFile(req.GetPath(), []byte("now"), 0o644)
	if data, _, errd := h.readAll(req); errd != nil || string(data) != "now" {
		t.Fatalf("retry: %q %v", data, errd)
	}
}

func TestReadCopyDetectsChange(t *testing.T) {
	h := newHarness(t, nil)
	path := filepath.Join(t.TempDir(), "f")
	os.WriteFile(path, []byte("before"), 0o644)
	h.srv.reads.forceCopy = true
	h.srv.reads.copyHook = func() {
		os.WriteFile(path, []byte("after, longer"), 0o644)
	}
	if _, _, errd := h.readAll(readReq(t, path)); errd.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_ABORTED {
		t.Fatalf("changed during copy: %v", errd)
	}
	h.srv.reads.copyHook = nil
	if data, _, errd := h.readAll(readReq(t, path)); errd != nil || string(data) != "after, longer" {
		t.Fatalf("copy read: %q %v", data, errd)
	}
}

func TestReadKeepaliveWhilePreparing(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.KeepaliveInterval = 20 * time.Millisecond })
	path := filepath.Join(t.TempDir(), "f")
	os.WriteFile(path, []byte("x"), 0o644)
	h.srv.reads.forceCopy = true
	h.srv.reads.copyHook = func() { time.Sleep(200 * time.Millisecond) }
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s := h.open(ctx)
	s.send(1, readReq(t, path))
	if ev := s.recv().GetFileReadEvent(); ev.GetKeepalive() == nil {
		t.Fatalf("first event %v", ev)
	}
}

// gatedSnapshot wraps a snapshot file and blocks its second ReadAt call
// until release is closed, like a disk that stalls on the second chunk.
// Release the gate with open, which is idempotent; gateSnapshots also
// releases it when the test ends, so a failing test cannot leave the worker
// blocked on the gate.
type gatedSnapshot struct {
	snapshotFile
	reads       atomic.Int32
	started     chan struct{}
	release     chan struct{}
	closed      chan struct{}
	closeOnce   sync.Once
	releaseOnce sync.Once
}

// open lets the stalled read return. It is safe to call more than once.
func (g *gatedSnapshot) open() {
	g.releaseOnce.Do(func() { close(g.release) })
}

func (g *gatedSnapshot) ReadAt(p []byte, off int64) (int, error) {
	if g.reads.Add(1) == 2 {
		close(g.started)
		<-g.release
	}
	return g.snapshotFile.ReadAt(p, off)
}

func (g *gatedSnapshot) Close() error {
	g.closeOnce.Do(func() { close(g.closed) })
	return g.snapshotFile.Close()
}

// gateSnapshots makes the read manager of h open gated snapshot files and
// returns the gate of the first one. The gate opens when the test ends, on
// any exit path, so the worker returns and closes the snapshot handle.
func gateSnapshots(h *harness) *gatedSnapshot {
	g := &gatedSnapshot{started: make(chan struct{}), release: make(chan struct{}), closed: make(chan struct{})}
	h.srv.reads.openSnapshot = func(path string) (snapshotFile, error) {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		g.snapshotFile = f
		return g, nil
	}
	h.t.Cleanup(g.open)
	return g
}

// A test that ends without opening the gate must not leave the stalled
// worker blocked: the cleanup of gateSnapshots opens the gate, and the worker
// closes its snapshot handle.
func TestGateSnapshotsOpensOnCleanup(t *testing.T) {
	var g *gatedSnapshot
	t.Run("stalled", func(t *testing.T) {
		h := newHarness(t, nil)
		g = gateSnapshots(h)
		path := filepath.Join(t.TempDir(), "f")
		if err := os.WriteFile(path, randomBytes(t, 2<<20+100), 0o644); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		s := h.open(ctx)
		s.send(1, readReq(t, path))
		waitStarted(t, g.started)
		select {
		case <-g.closed:
			t.Fatal("snapshot closed while its read is still stalled")
		default:
		}
		// The subtest returns without opening the gate.
	})
	waitClosed(t, g.closed)
	// An explicit open after the cleanup does not panic.
	g.open()
}

// readEntryOpen returns the number of open calls of a read_id.
func (m *readManager) readEntryOpen(readID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.entries[readID]; ok {
		return e.open
	}
	return 0
}

// waitReadCallsEnded waits until the helper has ended every call of a
// read_id. The handler ends a call (and records its end time) in a deferred
// call after it sent the end event, so the client can see the end of a read
// before the helper counts the call as ended. A test that advances the clock
// right after a read must wait for that, or the call ends at the advanced
// time, or is still open when the sweep runs.
func (h *harness) waitReadCallsEnded(readID string) {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for h.srv.reads.readEntryOpen(readID) != 0 {
		if time.Now().After(deadline) {
			h.t.Fatal("the read calls did not end")
		}
		time.Sleep(time.Millisecond)
	}
}

// A stalled chunk read must not stop the keepalives, and the stream
// continues with the right offset after the read returns.
func TestReadKeepaliveWhileChunkPending(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.KeepaliveInterval = 20 * time.Millisecond })
	g := gateSnapshots(h)
	path := filepath.Join(t.TempDir(), "f")
	content := randomBytes(t, 2<<20+100)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s := h.open(ctx)
	defer s.s.CloseRequest()
	s.send(1, readReq(t, path))
	var chunk *ownsandv1.FileChunk
	for chunk == nil {
		ev := s.recv().GetFileReadEvent()
		if ev.GetEnd() != nil {
			t.Fatal("end before the first chunk")
		}
		chunk = ev.GetChunk()
	}
	if chunk.GetOffset() != 0 || !bytes.Equal(chunk.GetData(), content[:len(chunk.GetData())]) {
		t.Fatalf("first chunk offset %d size %d", chunk.GetOffset(), len(chunk.GetData()))
	}
	got := append([]byte(nil), chunk.GetData()...)
	waitStarted(t, g.started)
	// The second read is stalled: keepalives arrive meanwhile, and no
	// chunk can.
	for i := 0; i < 3; i++ {
		ev := s.recv().GetFileReadEvent()
		if ev.GetKeepalive() == nil {
			t.Fatalf("event while the read is stalled: %v", ev)
		}
	}
	g.open()
	for {
		ev := s.recv().GetFileReadEvent()
		if ev.GetKeepalive() != nil {
			continue
		}
		if ev.GetEnd() != nil {
			if ev.GetEnd().GetSizeBytes() != int64(len(content)) {
				t.Fatalf("end size %d", ev.GetEnd().GetSizeBytes())
			}
			break
		}
		c := ev.GetChunk()
		if c.GetOffset() != int64(len(got)) {
			t.Fatalf("chunk offset %d, want %d", c.GetOffset(), len(got))
		}
		got = append(got, c.GetData()...)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("read %d bytes, content differs", len(got))
	}
	waitClosed(t, g.closed)
}

// A cancellation ends the call while a chunk read is stalled; the late
// worker closes its handle when the read returns.
func TestReadCancelWhileChunkPending(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.KeepaliveInterval = 20 * time.Millisecond })
	g := gateSnapshots(h)
	path := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(path, randomBytes(t, 2<<20+100), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := h.open(ctx)
	req := readReq(t, path)
	s.send(1, req)
	waitStarted(t, g.started)
	cancel()
	// The call ends while the gate is closed.
	deadline := time.Now().Add(10 * time.Second)
	for h.srv.reads.readEntryOpen(req.GetReadId()) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the call did not end while the chunk read was stalled")
		}
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case <-g.closed:
		t.Fatal("snapshot closed while its read is still stalled")
	default:
	}
	g.open()
	waitClosed(t, g.closed)
}

func TestReadRetention(t *testing.T) {
	h := newHarness(t, nil)
	path := filepath.Join(t.TempDir(), "f")
	os.WriteFile(path, []byte("x"), 0o644)
	req := readReq(t, path)
	h.readAll(req)
	h.waitReadCallsEnded(req.GetReadId())
	h.clock.Advance(14 * time.Minute)
	h.srv.Sweep()
	if data, _, errd := h.readAll(req); errd != nil || string(data) != "x" {
		t.Fatalf("read within 15 minutes: %v", errd)
	}
	h.waitReadCallsEnded(req.GetReadId())
	h.clock.Advance(15 * time.Minute)
	h.srv.Sweep()
	if _, _, errd := h.readAll(req); errd.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND {
		t.Fatalf("read after expiry: %v", errd)
	}
	entries, _ := os.ReadDir(h.srv.reads.dir)
	if len(entries) != 0 {
		t.Fatalf("snapshot files left: %d", len(entries))
	}
}

func (h *harness) write(req *ownsandv1.WriteFileRequest) (*ownsandv1.WriteFileResponse, *ownsandv1.ErrorDetail) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s := h.open(ctx)
	defer s.s.CloseRequest()
	s.send(9, req)
	f := s.recv()
	return f.GetFileWriteResult(), f.GetError()
}

func TestWriteFile(t *testing.T) {
	h := newHarness(t, nil)
	dir := t.TempDir()
	target := filepath.Join(dir, "out.bin")
	id := newID(t)
	salt := make([]byte, 32)
	chunk := func(off int64, data string) *ownsandv1.WriteFileRequest {
		return &ownsandv1.WriteFileRequest{SandboxId: testSandbox, WriteId: id, Offset: off, Data: []byte(data)}
	}
	first := chunk(5, "world")
	first.Path, first.BindingSalt = target, salt
	resp, errd := h.write(first)
	if errd != nil || resp.GetOffset() != 5 || resp.GetLength() != 5 || resp.GetFinished() {
		t.Fatalf("first chunk %v %v", resp, errd)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("target exists before finish")
	}
	// A repeated chunk with the same bytes is harmless.
	if _, errd := h.write(chunk(5, "world")); errd != nil {
		t.Fatalf("repeated chunk: %v", errd)
	}
	// Bytes 5..9 overlap with equal content and byte 4 is new: allowed.
	if _, errd := h.write(chunk(4, "Xworld")); errd != nil {
		t.Fatalf("partial overlap with equal bytes: %v", errd)
	}
	if _, errd := h.write(chunk(6, "XXX")); errd.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION {
		t.Fatalf("conflicting overlap: %v", errd)
	}
	fin := chunk(0, "hell")
	fin.Finish = &ownsandv1.WriteFileFinish{SizeBytes: 12, Mode: 0o600}
	if _, errd := h.write(fin); errd.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION {
		t.Fatalf("finish with wrong size: %v", errd)
	}
	fin.Finish.SizeBytes = 10
	resp, errd = h.write(fin)
	if errd != nil || !resp.GetFinished() || resp.GetSizeBytes() != 10 {
		t.Fatalf("finish %v %v", resp, errd)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "hellXworld" {
		t.Fatalf("target %q %v", got, err)
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(target); st.Mode().Perm() != 0o600 {
			t.Fatalf("mode %v", st.Mode())
		}
	}
	if _, err := os.Stat(tempPath(target, id)); !os.IsNotExist(err) {
		t.Fatal("temporary file left")
	}
	// A repeated finish returns the same result without writing again.
	os.WriteFile(target, []byte("modified"), 0o644)
	resp2, errd := h.write(fin)
	if errd != nil || resp2.GetSizeBytes() != 10 || !resp2.GetFinished() {
		t.Fatalf("repeated finish %v %v", resp2, errd)
	}
	if got, _ := os.ReadFile(target); string(got) != "modified" {
		t.Fatal("repeated finish wrote the target again")
	}
	fin2 := chunk(0, "hell")
	fin2.Finish = &ownsandv1.WriteFileFinish{SizeBytes: 10, Mode: 0o644}
	if _, errd := h.write(fin2); errd.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION {
		t.Fatalf("finish with other fields: %v", errd)
	}
	if _, errd := h.write(chunk(0, "hell")); errd.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION {
		t.Fatalf("chunk after finish: %v", errd)
	}
	h.clock.Advance(16 * time.Minute)
	h.srv.Sweep()
	if _, errd := h.write(fin); errd.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND {
		t.Fatalf("finish after retention: %v", errd)
	}
}

func TestWireToFileMode(t *testing.T) {
	cases := []struct {
		wire uint32
		want fs.FileMode
	}{
		{0, 0o644},
		{0o170000, 0o644}, // bits above 0o7777 are masked, leaving 0
		{0o600, 0o600},
		{0o755, 0o755},
		{0o4755, 0o755 | fs.ModeSetuid},
		{0o2755, 0o755 | fs.ModeSetgid},
		{0o1755, 0o755 | fs.ModeSticky},
		{0o7777, 0o777 | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky},
		{0o4000, fs.ModeSetuid},
	}
	for _, c := range cases {
		if got := wireToFileMode(c.wire); got != c.want {
			t.Errorf("wireToFileMode(%#o) = %v, want %v", c.wire, got, c.want)
		}
	}
}

func TestWriteSpecialModeBits(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes are ignored on Windows guests")
	}
	cases := []struct {
		name string
		mode uint32
		want fs.FileMode
	}{
		{"setuid", 0o4755, 0o755 | fs.ModeSetuid},
		{"setgid", 0o2755, 0o755 | fs.ModeSetgid},
		{"sticky", 0o1755, 0o755 | fs.ModeSticky},
		{"plain", 0o755, 0o755},
		{"private", 0o600, 0o600},
		{"default", 0, 0o644},
	}
	const special = fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// The OS may drop a special bit (setgid without group membership,
			// sticky on some systems, nosuid mounts). Skip when a control chmod
			// of the same bits on a scratch file does not stick.
			control := filepath.Join(t.TempDir(), "control")
			if err := os.WriteFile(control, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(control, c.want); err != nil {
				t.Skipf("control chmod: %v", err)
			}
			if st, err := os.Stat(control); err != nil || st.Mode()&(special|fs.ModePerm) != c.want {
				t.Skipf("OS does not keep mode %v on a control file", c.want)
			}
			h := newHarness(t, nil)
			target := filepath.Join(t.TempDir(), "out.bin")
			id := newID(t)
			req := &ownsandv1.WriteFileRequest{
				SandboxId: testSandbox, WriteId: id, Path: target, BindingSalt: make([]byte, 32),
				Data:   []byte("x"),
				Finish: &ownsandv1.WriteFileFinish{SizeBytes: 1, Mode: c.mode},
			}
			if resp, errd := h.write(req); errd != nil || !resp.GetFinished() {
				t.Fatalf("finish %v %v", resp, errd)
			}
			st, err := os.Stat(target)
			if err != nil {
				t.Fatal(err)
			}
			if got := st.Mode() & (special | fs.ModePerm); got != c.want {
				t.Fatalf("mode %v, want %v", got, c.want)
			}
			// A repeated finish returns the stored result without touching the file.
			if err := os.Chmod(target, 0o640); err != nil {
				t.Fatal(err)
			}
			if _, errd := h.write(req); errd != nil {
				t.Fatalf("repeated finish: %v", errd)
			}
			if st, _ := os.Stat(target); st.Mode()&(special|fs.ModePerm) != 0o640 {
				t.Fatalf("repeated finish changed the mode to %v", st.Mode())
			}
		})
	}
}

// TestWriteReadOnlyFinishMode covers the finish mode 0444. Linux and macOS
// guests apply it. Windows guests ignore it: Go's Chmod would set the
// read-only file attribute there, so the target must stay writable.
func TestWriteReadOnlyFinishMode(t *testing.T) {
	h := newHarness(t, nil)
	target := filepath.Join(t.TempDir(), "out.bin")
	req := &ownsandv1.WriteFileRequest{
		SandboxId: testSandbox, WriteId: newID(t), Path: target, BindingSalt: make([]byte, 32),
		Data:   []byte("x"),
		Finish: &ownsandv1.WriteFileFinish{SizeBytes: 1, Mode: 0o444},
	}
	resp, errd := h.write(req)
	if errd != nil || !resp.GetFinished() || resp.GetSizeBytes() != 1 {
		t.Fatalf("finish %v %v", resp, errd)
	}
	st, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		// Go reports the read-only file attribute as a mode without the owner
		// write bit.
		if st.Mode().Perm()&0o200 == 0 {
			t.Fatalf("read-only attribute set, mode %v", st.Mode())
		}
		f, err := os.OpenFile(target, os.O_WRONLY, 0)
		if err != nil {
			t.Fatalf("target is not writable: %v", err)
		}
		f.Close()
	} else if st.Mode().Perm() != 0o444 {
		t.Fatalf("mode %v, want 0444", st.Mode().Perm())
	}
	// A repeated identical finish returns the same result.
	resp2, errd := h.write(req)
	if errd != nil || !resp2.GetFinished() || resp2.GetSizeBytes() != 1 {
		t.Fatalf("repeated finish %v %v", resp2, errd)
	}
	// The mode is part of the recorded finish fields on every OS, so a repeat
	// with another mode fails, also where the mode is ignored.
	other := &ownsandv1.WriteFileRequest{
		SandboxId: req.GetSandboxId(), WriteId: req.GetWriteId(), Path: target, BindingSalt: make([]byte, 32),
		Data:   []byte("x"),
		Finish: &ownsandv1.WriteFileFinish{SizeBytes: 1, Mode: 0o644},
	}
	if _, errd := h.write(other); errd.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION {
		t.Fatalf("repeated finish with another mode: %v", errd)
	}
}

// TestWriteFinishRetryAfterRenameFailureKeepsReadOnlyMode covers a finish with
// mode 0444 whose rename fails once. The staging file already has the
// read-only mode then; an unprivileged guest user must still be able to repeat
// the same finish and get the file with the requested mode.
func TestWriteFinishRetryAfterRenameFailureKeepsReadOnlyMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows guests ignore the finish mode")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory permission that induces the rename failure")
	}
	h := newHarness(t, nil)
	dir := t.TempDir()
	target := filepath.Join(dir, "out.bin")
	id := newID(t)
	first := &ownsandv1.WriteFileRequest{
		SandboxId: testSandbox, WriteId: id, Path: target, BindingSalt: make([]byte, 32),
		Data: []byte("hello"),
	}
	if _, errd := h.write(first); errd != nil {
		t.Fatalf("first chunk: %v", errd)
	}
	fin := &ownsandv1.WriteFileRequest{
		SandboxId: testSandbox, WriteId: id, Path: target, BindingSalt: make([]byte, 32),
		Data:   []byte("hello"),
		Finish: &ownsandv1.WriteFileFinish{SizeBytes: 5, Mode: 0o444},
	}
	// Without write permission on the directory the rename fails, while the
	// owner can still open the staging file.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	if _, errd := h.write(fin); errd == nil {
		t.Fatal("finish succeeded although the rename must fail")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("target exists after failed rename: %v", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	resp, errd := h.write(fin)
	if errd != nil || !resp.GetFinished() || resp.GetSizeBytes() != 5 {
		t.Fatalf("repeated finish %v %v", resp, errd)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "hello" {
		t.Fatalf("target %q %v", got, err)
	}
	if st, err := os.Stat(target); err != nil || st.Mode().Perm() != 0o444 {
		t.Fatalf("target mode %v %v, want 0444", st.Mode(), err)
	}
	if _, err := os.Stat(tempPath(target, id)); !os.IsNotExist(err) {
		t.Fatal("temporary file left")
	}
}

func TestWriteEmptyFileAndErrors(t *testing.T) {
	h := newHarness(t, nil)
	dir := t.TempDir()
	req := &ownsandv1.WriteFileRequest{SandboxId: testSandbox, WriteId: newID(t), Path: filepath.Join(dir, "empty"), BindingSalt: make([]byte, 32), Finish: &ownsandv1.WriteFileFinish{SizeBytes: 0}}
	if resp, errd := h.write(req); errd != nil || !resp.GetFinished() {
		t.Fatalf("empty file: %v %v", resp, errd)
	}
	if st, err := os.Stat(req.GetPath()); err != nil || st.Size() != 0 {
		t.Fatalf("empty target: %v", err)
	}
	if _, errd := h.write(&ownsandv1.WriteFileRequest{SandboxId: testSandbox, WriteId: newID(t), Data: []byte("x")}); errd.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND {
		t.Fatalf("unknown write without path: %v", errd)
	}
	if _, errd := h.write(&ownsandv1.WriteFileRequest{SandboxId: testSandbox, WriteId: newID(t), Path: filepath.Join(dir, "no", "dir", "f")}); errd.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION {
		t.Fatalf("missing parent: %v", errd)
	}
	if _, errd := h.write(&ownsandv1.WriteFileRequest{SandboxId: testSandbox, WriteId: newID(t), Path: dir}); errd.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION {
		t.Fatalf("directory target: %v", errd)
	}
}

func TestWriteExpiryRemovesTemporaryFile(t *testing.T) {
	h := newHarness(t, nil)
	target := filepath.Join(t.TempDir(), "f")
	req := &ownsandv1.WriteFileRequest{SandboxId: testSandbox, WriteId: newID(t), Path: target, BindingSalt: make([]byte, 32), Data: []byte("partial")}
	if _, errd := h.write(req); errd != nil {
		t.Fatal(errd)
	}
	h.clock.Advance(14 * time.Minute)
	h.srv.Sweep()
	if _, err := os.Stat(tempPath(target, req.GetWriteId())); err != nil {
		t.Fatal("temporary file removed too early")
	}
	h.clock.Advance(2 * time.Minute)
	h.srv.Sweep()
	if _, err := os.Stat(tempPath(target, req.GetWriteId())); !os.IsNotExist(err) {
		t.Fatal("temporary file kept after 15 minutes")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("partial upload reached the target")
	}
	if _, errd := h.write(req); errd.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND {
		t.Fatalf("write after expiry: %v", errd)
	}
}

func TestMergeSpan(t *testing.T) {
	var r []span
	for _, s := range []span{{10, 20}, {0, 5}, {5, 10}, {30, 40}, {35, 50}} {
		r = mergeSpan(r, s)
	}
	if len(r) != 2 || r[0] != (span{0, 20}) || r[1] != (span{30, 50}) {
		t.Fatalf("ranges %v", r)
	}
}

// newReadPreparationGate pauses workers at a hook until release or cleanup.
func newReadPreparationGate(t *testing.T) (hook func(), started <-chan struct{}, release func()) {
	reached := make(chan struct{}, 4)
	released := make(chan struct{})
	release = sync.OnceFunc(func() { close(released) })
	t.Cleanup(release)
	return func() {
		reached <- struct{}{}
		<-released
	}, reached, release
}

func newPreparationTestReadManager(t *testing.T, now func() time.Time) *readManager {
	t.Helper()
	m, err := newReadManager(filepath.Join(t.TempDir(), "reads"), now, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	m.forceCopy = true
	t.Cleanup(func() {
		if !m.close(readCloseGrace) {
			t.Error("snapshot workers did not finish before removing test files")
		}
	})
	return m
}

// blockedReadManager pauses workers after copying the source file.
func blockedReadManager(t *testing.T) (m *readManager, started <-chan struct{}, release func()) {
	t.Helper()
	m = newPreparationTestReadManager(t, time.Now)
	m.copyHook, started, release = newReadPreparationGate(t)
	return m, started, release
}

func waitClosed(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the snapshot")
	}
}

func waitStarted(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	waitClosed(t, ch)
}

func TestReadPreparationGateCleanup(t *testing.T) {
	for _, phase := range []struct {
		name  string
		setup func(*readManager, func())
	}{
		{"before-open", func(m *readManager, hook func()) { m.openHook = hook }},
		{"after-copy", func(m *readManager, hook func()) { m.copyHook = hook }},
	} {
		for _, scenario := range []struct {
			name       string
			late       bool
			concurrent bool
		}{
			{name: "blocked-worker"},
			{name: "late-worker", late: true},
			{name: "concurrent-release", concurrent: true},
		} {
			t.Run(phase.name+"/"+scenario.name, func(t *testing.T) {
				m := newPreparationTestReadManager(t, time.Now)
				const contents = "0123456789"
				src := filepath.Join(t.TempDir(), "src")
				if err := os.WriteFile(src, []byte(contents), 0o644); err != nil {
					t.Fatal(err)
				}
				begin := func(t *testing.T) *readEntry {
					t.Helper()
					e, err := m.begin("read-gate-cleanup", src)
					if err != nil {
						t.Fatal(err)
					}
					return e
				}
				var entry *readEntry
				var started <-chan struct{}
				var release func()
				releaseReturned := make(chan struct{})
				if !t.Run("gate", func(t *testing.T) {
					var hook func()
					hook, started, release = newReadPreparationGate(t)
					phase.setup(m, hook)
					if scenario.late {
						return
					}
					entry = begin(t)
					waitStarted(t, started)
					select {
					case <-entry.ready:
						t.Fatal("snapshot became ready while preparation was paused")
					default:
					}
					if scenario.concurrent {
						releasing := make(chan struct{})
						t.Cleanup(func() { close(releasing) })
						go func() {
							<-releasing
							release()
							close(releaseReturned)
						}()
					}
				}) {
					return
				}
				if scenario.late {
					entry = begin(t)
					waitStarted(t, started)
				}
				waitClosed(t, entry.ready)
				if scenario.concurrent {
					waitClosed(t, releaseReturned)
				}
				release()
				if entry.err != nil {
					t.Fatalf("snapshot failed: %v", entry.err)
				}
				got, err := os.ReadFile(entry.snapshot)
				if err != nil || string(got) != contents {
					t.Fatalf("snapshot: %q, %v", got, err)
				}
			})
		}
	}
}

func (m *readManager) hasEntry(readID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.entries[readID]
	return ok
}

// A failure published after the last call ended removes the entry, so the
// next call for the read_id makes a new snapshot.
func TestReadFailureAfterLastCallEndedIsForgotten(t *testing.T) {
	m, started, release := blockedReadManager(t)
	path := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(path, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	const readID = "read-failure-after-end"
	e, err := m.begin(readID, path)
	if err != nil {
		t.Fatal(err)
	}
	waitStarted(t, started)
	m.end(readID, e)
	// The source changes during the copy, so the copy check fails.
	if err := os.WriteFile(path, []byte("after, longer"), 0o644); err != nil {
		t.Fatal(err)
	}
	release()
	waitClosed(t, e.ready)
	if e.err == nil {
		t.Fatal("expected the snapshot to fail")
	}
	if m.hasEntry(readID) {
		t.Fatal("failed entry stayed after the last call ended")
	}
	m.copyHook = nil
	e2, err := m.begin(readID, path)
	if err != nil {
		t.Fatal(err)
	}
	defer m.end(readID, e2)
	if e2 == e {
		t.Fatal("retry reused the failed entry")
	}
	waitClosed(t, e2.ready)
	if e2.err != nil || e2.size != int64(len("after, longer")) {
		t.Fatalf("retry: size %d err %v", e2.size, e2.err)
	}
}

// A call that still waits receives the failure, and the entry goes away when
// that call ends.
func TestReadFailureReachesWaitingCall(t *testing.T) {
	m, started, release := blockedReadManager(t)
	path := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(path, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	const readID = "read-failure-waiting"
	first, err := m.begin(readID, path)
	if err != nil {
		t.Fatal(err)
	}
	waitStarted(t, started)
	second, err := m.begin(readID, path)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("second call did not share the entry")
	}
	m.end(readID, first)
	if err := os.WriteFile(path, []byte("after, longer"), 0o644); err != nil {
		t.Fatal(err)
	}
	release()
	waitClosed(t, second.ready)
	if second.err == nil {
		t.Fatal("waiting call did not receive the failure")
	}
	if !m.hasEntry(readID) {
		t.Fatal("entry was removed while a call was still waiting")
	}
	m.end(readID, second)
	if m.hasEntry(readID) {
		t.Fatal("failed entry stayed after its last call ended")
	}
}

// A snapshot that succeeds after the last call ended stays for the
// retention time, so a later call reads the original snapshot.
func TestReadSuccessAfterLastCallEndedIsKept(t *testing.T) {
	m, started, release := blockedReadManager(t)
	path := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(path, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	const readID = "read-success-after-end"
	e, err := m.begin(readID, path)
	if err != nil {
		t.Fatal(err)
	}
	waitStarted(t, started)
	m.end(readID, e)
	release()
	waitClosed(t, e.ready)
	if e.err != nil {
		t.Fatalf("snapshot failed: %v", e.err)
	}
	if !m.hasEntry(readID) {
		t.Fatal("successful entry was removed after the last call ended")
	}
	// The source changes; a later call still sees the original snapshot.
	if err := os.WriteFile(path, []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	e2, err := m.begin(readID, path)
	if err != nil {
		t.Fatal(err)
	}
	if e2 != e {
		t.Fatal("retry did not reuse the snapshot")
	}
	got, err := os.ReadFile(e2.snapshot)
	if err != nil || string(got) != "0123456789" || e2.size != 10 {
		t.Fatalf("snapshot %q size %d err %v", got, e2.size, err)
	}
	// The snapshot stays until the retention time after the last call.
	m.end(readID, e2)
	m.sweep(e2.lastEnd.Add(m.retention - time.Minute))
	if !m.hasEntry(readID) {
		t.Fatal("snapshot was swept before the retention time")
	}
	m.sweep(e2.lastEnd.Add(m.retention))
	if m.hasEntry(readID) {
		t.Fatal("snapshot was kept after the retention time")
	}
}

// readTestClock is a controllable clock for the read manager.
type readTestClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *readTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *readTestClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// preOpenReadManager pauses workers before they open the source file.
func preOpenReadManager(t *testing.T) (m *readManager, clock *readTestClock, started <-chan struct{}, release func()) {
	t.Helper()
	clock = &readTestClock{now: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)}
	m = newPreparationTestReadManager(t, clock.Now)
	m.openHook, started, release = newReadPreparationGate(t)
	return m, clock, started, release
}

func snapshotFiles(t *testing.T, m *readManager) []string {
	t.Helper()
	items, err := os.ReadDir(m.dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, item := range items {
		names = append(names, item.Name())
	}
	return names
}

// A sweep must not delete an entry whose snapshot is still being prepared:
// the worker would create a snapshot file after the sweep that no entry owns.
// The retention time of the snapshot then starts when it became ready.
func TestReadSweepKeepsSnapshotInPreparation(t *testing.T) {
	m, clock, started, release := preOpenReadManager(t)
	path := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(path, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	const readID = "read-sweep-in-preparation"
	e, err := m.begin(readID, path)
	if err != nil {
		t.Fatal(err)
	}
	waitStarted(t, started)
	// The only call is cancelled while the worker has not created the file.
	m.end(readID, e)
	clock.Advance(m.retention + time.Minute)
	m.sweep(clock.Now())
	if !m.hasEntry(readID) {
		t.Fatal("sweep removed an entry whose snapshot is still being prepared")
	}
	// A retry of the read_id still joins the preparation.
	retry, err := m.begin(readID, path)
	if err != nil {
		t.Fatalf("retry after the sweep: %v", err)
	}
	if retry != e {
		t.Fatal("retry did not reuse the entry in preparation")
	}
	m.end(readID, retry)
	clock.Advance(time.Minute)
	release()
	waitClosed(t, e.ready)
	if e.err != nil {
		t.Fatalf("snapshot failed: %v", e.err)
	}
	ready := clock.Now()
	if names := snapshotFiles(t, m); len(names) != 1 || names[0] != readID {
		t.Fatalf("snapshot files %v, want exactly the snapshot of the entry", names)
	}
	// The retention time counts from the moment the snapshot became ready,
	// not from the last call end that happened more than the retention
	// time ago.
	m.sweep(ready.Add(m.retention - time.Minute))
	if !m.hasEntry(readID) {
		t.Fatal("snapshot was swept before its retention time after it became ready")
	}
	if got, err := os.ReadFile(e.snapshot); err != nil || string(got) != "0123456789" {
		t.Fatalf("snapshot %q err %v", got, err)
	}
	m.sweep(ready.Add(m.retention))
	if m.hasEntry(readID) {
		t.Fatal("snapshot was kept after its retention time")
	}
	if names := snapshotFiles(t, m); len(names) != 0 {
		t.Fatalf("snapshot files %v remain after the sweep", names)
	}
	if _, err := m.begin(readID, path); err == nil {
		t.Fatal("expected an expired read_id after the sweep")
	}
}

// A preparation that fails while a sweep skips it leaves neither an entry nor
// a file.
func TestReadSweepKeepsFailingSnapshotInPreparation(t *testing.T) {
	m, clock, started, release := preOpenReadManager(t)
	path := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(path, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	const readID = "read-sweep-failing-preparation"
	e, err := m.begin(readID, path)
	if err != nil {
		t.Fatal(err)
	}
	waitStarted(t, started)
	m.end(readID, e)
	clock.Advance(m.retention + time.Minute)
	m.sweep(clock.Now())
	if !m.hasEntry(readID) {
		t.Fatal("sweep removed an entry whose snapshot is still being prepared")
	}
	// The source disappears before the worker opens it.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	release()
	waitClosed(t, e.ready)
	if e.err == nil {
		t.Fatal("expected the snapshot to fail")
	}
	if m.hasEntry(readID) {
		t.Fatal("failed entry stayed after the last call ended")
	}
	if names := snapshotFiles(t, m); len(names) != 0 {
		t.Fatalf("snapshot files %v remain after the failure", names)
	}
}

// Any file that cannot be retained is removed: if an entry vanished by any
// other path while its worker still ran, the worker removes the file it made.
func TestReadSnapshotOfForgottenEntryIsRemoved(t *testing.T) {
	m, _, started, release := preOpenReadManager(t)
	path := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(path, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	const readID = "read-forgotten-entry"
	e, err := m.begin(readID, path)
	if err != nil {
		t.Fatal(err)
	}
	waitStarted(t, started)
	m.mu.Lock()
	delete(m.entries, readID)
	m.mu.Unlock()
	release()
	waitClosed(t, e.ready)
	if e.err == nil {
		t.Fatal("expected the snapshot of a forgotten entry to fail")
	}
	if names := snapshotFiles(t, m); len(names) != 0 {
		t.Fatalf("snapshot files %v remain for an entry that nothing tracks", names)
	}
	m.end(readID, e)
}

func TestReadCopyZeroSizeProcfs(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("procfs exists only on Linux")
	}
	const path = "/proc/cpuinfo"
	st, err := os.Stat(path)
	if err != nil || st.Size() != 0 {
		t.Skipf("%s is missing or reports a size: %v", path, err)
	}
	h := newHarness(t, nil)
	h.srv.reads.forceCopy = true
	req := readReq(t, path)
	data, size, errd := h.readAll(req)
	if errd != nil || len(data) == 0 || size != int64(len(data)) {
		t.Fatalf("procfs read: %d bytes size %d err %v", len(data), size, errd)
	}
	// A resumed read returns the suffix of the snapshot.
	req.Offset = 1
	suffix, size2, errd := h.readAll(req)
	if errd != nil || size2 != size || !bytes.Equal(suffix, data[1:]) {
		t.Fatalf("resumed procfs read: %d bytes size %d err %v", len(suffix), size2, errd)
	}
}

func TestReadCopyEmptyFileAndSizeChange(t *testing.T) {
	h := newHarness(t, nil)
	h.srv.reads.forceCopy = true
	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if data, size, errd := h.readAll(readReq(t, empty)); errd != nil || len(data) != 0 || size != 0 {
		t.Fatalf("empty file: %d bytes size %d err %v", len(data), size, errd)
	}
	// An ordinary empty file that grows during the copy is still aborted.
	h.srv.reads.copyHook = func() { os.WriteFile(empty, []byte("grown"), 0o644) }
	if _, _, errd := h.readAll(readReq(t, empty)); errd.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_ABORTED {
		t.Fatalf("empty file that grew: %v", errd)
	}
}

// writeLongName writes a small file whose name has nameLen bytes and checks
// the staging behavior around the finish.
func writeLongName(t *testing.T, h *harness, dir string, nameLen int) {
	t.Helper()
	target := filepath.Join(dir, strings.Repeat("a", nameLen))
	id := newID(t)
	req := &ownsandv1.WriteFileRequest{SandboxId: testSandbox, WriteId: id, Path: target, BindingSalt: make([]byte, 32), Data: []byte("hello")}
	if _, errd := h.write(req); errd != nil {
		t.Fatalf("%d-byte name, first chunk: %v", nameLen, errd)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("%d-byte name: target exists before finish", nameLen)
	}
	if _, err := os.Stat(tempPath(target, id)); err != nil {
		t.Fatalf("%d-byte name: temporary file missing: %v", nameLen, err)
	}
	req.Data = nil
	req.Finish = &ownsandv1.WriteFileFinish{SizeBytes: 5}
	req.Offset = 5
	if _, errd := h.write(req); errd != nil {
		t.Fatalf("%d-byte name, finish: %v", nameLen, errd)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "hello" {
		t.Fatalf("%d-byte name: target %q %v", nameLen, got, err)
	}
	if _, err := os.Stat(tempPath(target, id)); !os.IsNotExist(err) {
		t.Fatalf("%d-byte name: temporary file left", nameLen)
	}
}

func TestWriteLongTargetNames(t *testing.T) {
	h := newHarness(t, nil)
	dir := t.TempDir()
	for _, n := range []int{205, 206, 255} {
		writeLongName(t, h, dir, n)
	}
}

func TestWriteTempNameNeverEqualsTarget(t *testing.T) {
	h := newHarness(t, nil)
	dir := t.TempDir()
	id := newID(t)
	target := filepath.Join(dir, ".ownsand-"+id+".tmp")
	if tempPath(target, id) == target {
		t.Fatal("temporary path equals target")
	}
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	req := &ownsandv1.WriteFileRequest{SandboxId: testSandbox, WriteId: id, Path: target, BindingSalt: make([]byte, 32), Data: []byte("new")}
	if _, errd := h.write(req); errd != nil {
		t.Fatal(errd)
	}
	if got, _ := os.ReadFile(target); string(got) != "old" {
		t.Fatalf("target changed before finish: %q", got)
	}
	req.Data, req.Offset = nil, 3
	req.Finish = &ownsandv1.WriteFileFinish{SizeBytes: 3}
	for i := 0; i < 2; i++ { // the repeated finish returns the same result
		if _, errd := h.write(req); errd != nil {
			t.Fatalf("finish %d: %v", i, errd)
		}
	}
	if got, _ := os.ReadFile(target); string(got) != "new" {
		t.Fatalf("target after finish: %q", got)
	}
}

// On a case-insensitive file system, a target that differs from the
// temporary name only in letter case names the same file, so the temporary
// name must differ from the target name under case folding too.
func TestWriteTempNameDiffersFromTargetUnderCaseFolding(t *testing.T) {
	dir := t.TempDir()
	id := newID(t)
	for _, base := range []string{
		".ownsand-" + id + ".tmp",
		strings.ToUpper(".ownsand-" + id + ".tmp"),
		".OwnSand-" + id + ".Tmp",
	} {
		target := filepath.Join(dir, base)
		if got := filepath.Base(tempPath(target, id)); strings.EqualFold(got, base) {
			t.Fatalf("temporary name %q equals target name %q under case folding", got, base)
		}
	}
}

func TestWriteLongWriteIDIsBounded(t *testing.T) {
	target := filepath.Join(t.TempDir(), "f")
	long := strings.Repeat("x", 1000)
	if n := len(filepath.Base(tempPath(target, long))); n > 255 {
		t.Fatalf("temporary name has %d bytes", n)
	}
}

func TestWriteExpiryOfLongTargetRemovesTemporaryFile(t *testing.T) {
	h := newHarness(t, nil)
	target := filepath.Join(t.TempDir(), strings.Repeat("b", 255))
	req := &ownsandv1.WriteFileRequest{SandboxId: testSandbox, WriteId: newID(t), Path: target, BindingSalt: make([]byte, 32), Data: []byte("partial")}
	if _, errd := h.write(req); errd != nil {
		t.Fatal(errd)
	}
	h.clock.Advance(16 * time.Minute)
	h.srv.Sweep()
	if _, err := os.Stat(tempPath(target, req.GetWriteId())); !os.IsNotExist(err) {
		t.Fatal("temporary file kept after expiry")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("target exists after expiry")
	}
}

// statSource keeps the identity of the file that the path named when it ran.
// On Windows, os.Stat keeps only the path and os.SameFile opens it again when
// it compares, so a swap after the stat would not be seen.
func TestStatSourceKeepsIdentityAfterPathSwap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	other := filepath.Join(dir, "other")
	if err := os.WriteFile(path, []byte("aaaa"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte("bbbb"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := statSource(path)
	if err != nil {
		t.Fatal(err)
	}
	same, err := statSource(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, same) {
		t.Fatal("two stats of an unchanged path name different files")
	}
	if err := os.Rename(other, path); err != nil {
		t.Fatal(err)
	}
	after, err := statSource(path)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Fatal("the stat before the swap names the file after the swap")
	}
}

// A path swapped to another same-size file between the first stat and the
// open must abort the snapshot and leave no snapshot file behind.
func TestReadSnapshotAbortsWhenPathSwappedBeforeOpen(t *testing.T) {
	m, err := newReadManager(filepath.Join(t.TempDir(), "reads"), time.Now, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	other := filepath.Join(dir, "other")
	if err := os.WriteFile(path, []byte("aaaa"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte("bbbb"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, forceCopy := range []bool{true, false} {
		m.forceCopy = forceCopy
		swapped := false
		m.openHook = func() {
			if swapped {
				return
			}
			swapped = true
			if err := os.Rename(other, path); err != nil {
				t.Fatal(err)
			}
		}
		dst := filepath.Join(m.dir, "snap")
		_, _, created, err := m.makeSnapshot(path, dst)
		if created || err == nil || !strings.Contains(err.Error(), "file changed during the snapshot copy") {
			t.Fatalf("forceCopy=%v: expected abort before creation, got created=%v, err=%v", forceCopy, created, err)
		}
		// The abort must leave no snapshot file behind.
		if _, err := os.Lstat(dst); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("forceCopy=%v: snapshot file left after abort", forceCopy)
		}
		// Restore the fixture for the next round.
		os.WriteFile(path, []byte("aaaa"), 0o644)
		os.WriteFile(other, []byte("bbbb"), 0o644)
	}
	// A stable file still reads.
	m.openHook = nil
	if kind, n, created, err := m.makeSnapshot(path, filepath.Join(m.dir, "ok")); err != nil || !created || n != 4 {
		t.Fatalf("stable read: kind %v n %d created %v err %v", kind, n, created, err)
	}
}
