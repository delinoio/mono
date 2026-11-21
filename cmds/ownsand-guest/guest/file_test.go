// SPDX-License-Identifier: Apache-2.0

package guest

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
