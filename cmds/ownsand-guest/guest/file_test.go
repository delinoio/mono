// SPDX-License-Identifier: Apache-2.0

package guest

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
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

func TestReadRetention(t *testing.T) {
	h := newHarness(t, nil)
	path := filepath.Join(t.TempDir(), "f")
	os.WriteFile(path, []byte("x"), 0o644)
	req := readReq(t, path)
	h.readAll(req)
	h.clock.Advance(14 * time.Minute)
	h.srv.Sweep()
	if data, _, errd := h.readAll(req); errd != nil || string(data) != "x" {
		t.Fatalf("read within 15 minutes: %v", errd)
	}
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

// blockedReadManager returns a read manager whose copy blocks in the copy
// hook until release is closed. started receives one value per copy that
// reaches the hook.
func blockedReadManager(t *testing.T) (m *readManager, started chan struct{}, release chan struct{}) {
	t.Helper()
	m, err := newReadManager(filepath.Join(t.TempDir(), "reads"), time.Now, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	started = make(chan struct{}, 4)
	release = make(chan struct{})
	m.forceCopy = true
	m.copyHook = func() {
		started <- struct{}{}
		<-release
	}
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
	close(release)
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
	close(release)
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
	close(release)
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
