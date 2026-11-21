// SPDX-License-Identifier: FSL-1.1-ALv2

package guest

import (
	"bytes"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// releaseWriteFiles closes the staging files that the server holds open, as
// the exit of the helper process does. A test must call it before it removes
// or renames a staging file itself: Windows refuses to delete a file that a
// process still holds open (the helper opens it without FILE_SHARE_DELETE).
func releaseWriteFiles(h *harness) {
	h.srv.writes.mu.Lock()
	defer h.srv.writes.mu.Unlock()
	for _, e := range h.srv.writes.entries {
		if e.file != nil {
			e.file.Close()
			e.file = nil
		}
	}
}

// releaseOnCleanup closes the staging files that h holds when the test ends.
// Windows cannot delete a directory that holds an open file, and t.Cleanup
// functions run last-in first-out: call it after the t.TempDir() of the
// target directory (the harness registers its own cleanup before the test
// creates that directory), so the files close before the directory is removed.
func releaseOnCleanup(t *testing.T, h *harness) {
	t.Helper()
	t.Cleanup(func() { releaseWriteFiles(h) })
}

// restart simulates a helper restart on the same state directory and clock:
// it closes the files that the old server holds open (the process exit does
// that) and starts a new server.
func restart(t *testing.T, h *harness, mutate func(*Config)) *harness {
	t.Helper()
	releaseWriteFiles(h)
	stateDir, now := h.srv.cfg.StateDir, h.clock.Now
	h2 := newHarness(t, func(c *Config) {
		c.StateDir = stateDir
		c.Now = now
		if mutate != nil {
			mutate(c)
		}
	})
	h2.clock = h.clock
	releaseOnCleanup(t, h2)
	return h2
}

func recordFiles(t *testing.T, h *harness) []string {
	t.Helper()
	items, err := os.ReadDir(filepath.Join(h.srv.cfg.StateDir, "writes"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, item := range items {
		names = append(names, item.Name())
	}
	return names
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// startPartialWrite writes one non-final chunk to a new target that holds
// "old".
func startPartialWrite(t *testing.T, h *harness) (target string, req *ownsandv1.WriteFileRequest) {
	t.Helper()
	target = filepath.Join(t.TempDir(), "f")
	releaseOnCleanup(t, h)
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	req = &ownsandv1.WriteFileRequest{SandboxId: testSandbox, WriteId: newID(t), Path: target, BindingSalt: make([]byte, 32), Data: []byte("partial")}
	if _, errd := h.write(req); errd != nil {
		t.Fatal(errd)
	}
	return target, req
}

func TestWriteRecoveryExpiresStagingFileAfterRestart(t *testing.T) {
	h := newHarness(t, nil)
	target, req := startPartialWrite(t, h)
	tmp := tempPath(target, req.GetWriteId())
	if got := recordFiles(t, h); len(got) != 1 {
		t.Fatalf("records after the first chunk: %v", got)
	}

	h2 := restart(t, h, nil)
	if got := recordFiles(t, h2); len(got) != 1 {
		t.Fatalf("records after restart: %v", got)
	}
	// A chunk of the recovered write cannot continue: the write was lost.
	if _, errd := h2.write(req); errd.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND || !strings.Contains(errd.GetMessage(), "helper restart") {
		t.Fatalf("chunk of a recovered write: %v", errd)
	}
	fin := &ownsandv1.WriteFileRequest{SandboxId: testSandbox, WriteId: req.GetWriteId(), Finish: &ownsandv1.WriteFileFinish{SizeBytes: 7}}
	if _, errd := h2.write(fin); errd.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND {
		t.Fatalf("finish of a recovered write: %v", errd)
	}
	if !exists(tmp) {
		t.Fatal("a rejected chunk removed the staging file")
	}

	h2.clock.Advance(14 * time.Minute)
	h2.srv.Sweep()
	if !exists(tmp) {
		t.Fatal("staging file removed too early after restart")
	}
	h2.clock.Advance(2 * time.Minute)
	h2.srv.Sweep()
	if exists(tmp) {
		t.Fatal("staging file kept after 15 minutes without a chunk")
	}
	if got := recordFiles(t, h2); len(got) != 0 {
		t.Fatalf("record kept after cleanup: %v", got)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "old" {
		t.Fatalf("target %q %v", got, err)
	}
	if _, errd := h2.write(req); errd.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND || !strings.Contains(errd.GetMessage(), "expired") {
		t.Fatalf("write after expiry: %v", errd)
	}
	// The expired ID is remembered for 30 minutes, then forgotten.
	h2.clock.Advance(ExpiredMemory)
	h2.srv.Sweep()
	h2.srv.writes.mu.Lock()
	left := len(h2.srv.writes.expired) + len(h2.srv.writes.entries)
	h2.srv.writes.mu.Unlock()
	if left != 0 {
		t.Fatalf("write state left after the expired memory: %d", left)
	}
}

func TestWriteRecoveryUsesTimeOfLastChunk(t *testing.T) {
	h := newHarness(t, nil)
	target, req := startPartialWrite(t, h)
	tmp := tempPath(target, req.GetWriteId())
	h.clock.Advance(10 * time.Minute)
	more := &ownsandv1.WriteFileRequest{SandboxId: testSandbox, WriteId: req.GetWriteId(), Offset: 7, Data: []byte("more")}
	if _, errd := h.write(more); errd != nil {
		t.Fatal(errd)
	}
	h2 := restart(t, h, nil)
	// 6 minutes after the second chunk, 16 after the first.
	h2.clock.Advance(6 * time.Minute)
	h2.srv.Sweep()
	if !exists(tmp) {
		t.Fatal("record was not refreshed by the second chunk")
	}
	h2.clock.Advance(10 * time.Minute)
	h2.srv.Sweep()
	if exists(tmp) {
		t.Fatal("staging file kept after 15 minutes without a chunk")
	}
}

func TestWriteRecoveryClampsFutureLastChunk(t *testing.T) {
	h := newHarness(t, nil)
	target, req := startPartialWrite(t, h)
	tmp := tempPath(target, req.GetWriteId())
	// The guest clock went back by an hour (a clock set) before the restart.
	h.clock.Advance(-time.Hour)
	h2 := restart(t, h, nil)
	h2.clock.Advance(14 * time.Minute)
	h2.srv.Sweep()
	if !exists(tmp) {
		t.Fatal("staging file removed too early")
	}
	h2.clock.Advance(2 * time.Minute)
	h2.srv.Sweep()
	if exists(tmp) {
		t.Fatal("staging file of a future record never expired")
	}
}

func TestWriteRecoveryKeepsReplacedFile(t *testing.T) {
	h := newHarness(t, nil)
	target, req := startPartialWrite(t, h)
	tmp := tempPath(target, req.GetWriteId())
	releaseWriteFiles(h)
	// Replace the staging file by another file. The old file stays until the
	// new one exists, so that the file system cannot give its inode number to
	// the replacement.
	held := tmp + ".held"
	if err := os.Rename(tmp, held); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmp, []byte("somebody else"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(held); err != nil {
		t.Fatal(err)
	}
	h2 := restart(t, h, nil)
	h2.clock.Advance(16 * time.Minute)
	h2.srv.Sweep()
	if got, err := os.ReadFile(tmp); err != nil || string(got) != "somebody else" {
		t.Fatalf("replaced file %q %v", got, err)
	}
	if got := recordFiles(t, h2); len(got) != 0 {
		t.Fatalf("record kept after cleanup: %v", got)
	}
	if got, _ := os.ReadFile(target); string(got) != "old" {
		t.Fatalf("target %q", got)
	}
}

func TestWriteRecoveryDropsRecordsWithoutStagingFile(t *testing.T) {
	h := newHarness(t, nil)
	target, req := startPartialWrite(t, h)
	releaseWriteFiles(h)
	if err := os.Remove(tempPath(target, req.GetWriteId())); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(h.srv.cfg.StateDir, "writes")
	// An unreadable record, a record under the wrong name and the temporary
	// file of an interrupted save.
	for name, content := range map[string]string{
		"broken.json":             "{not json",
		writeRecordName("other"):  `{"version":1,"write_id":"unrelated","tmp_path":"/nonexistent","last_chunk_unix_nano":1}`,
		"interrupted.json.tmp":    "{",
		"keep-unknown-file.txt":   "x",
		writeRecordName("future"): `{"version":99,"write_id":"future"}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	h2 := restart(t, h, nil)
	if got := recordFiles(t, h2); len(got) != 1 || got[0] != "keep-unknown-file.txt" {
		t.Fatalf("records after restart: %v", got)
	}
	h2.srv.writes.mu.Lock()
	n := len(h2.srv.writes.entries)
	h2.srv.writes.mu.Unlock()
	if n != 0 {
		t.Fatalf("recovered entries: %d", n)
	}
	// The write is unknown now, not expired: a first call with a path starts it.
	if _, errd := h2.write(req); errd != nil {
		t.Fatalf("restart of a dropped write: %v", errd)
	}
}

func TestWriteFinishedUploadSurvivesRestartAndExpiry(t *testing.T) {
	h := newHarness(t, nil)
	target := filepath.Join(t.TempDir(), "f")
	releaseOnCleanup(t, h)
	req := &ownsandv1.WriteFileRequest{SandboxId: testSandbox, WriteId: newID(t), Path: target, BindingSalt: make([]byte, 32), Data: []byte("final"), Finish: &ownsandv1.WriteFileFinish{SizeBytes: 5}}
	first := &ownsandv1.WriteFileRequest{SandboxId: testSandbox, WriteId: req.GetWriteId(), Path: target, BindingSalt: make([]byte, 32), Data: []byte("fi")}
	if _, errd := h.write(first); errd != nil {
		t.Fatal(errd)
	}
	req.Offset, req.Data = 2, []byte("nal")
	if resp, errd := h.write(req); errd != nil || !resp.GetFinished() {
		t.Fatalf("finish %v %v", resp, errd)
	}
	if got := recordFiles(t, h); len(got) != 0 {
		t.Fatalf("record kept after finish: %v", got)
	}
	h2 := restart(t, h, nil)
	h2.clock.Advance(time.Hour)
	h2.srv.Sweep()
	if got, err := os.ReadFile(target); err != nil || string(got) != "final" {
		t.Fatalf("target %q %v", got, err)
	}
}

func TestWriteFinishedRecordFailureIsRetriedBySweep(t *testing.T) {
	h := newHarness(t, nil)
	target := filepath.Join(t.TempDir(), "f")
	releaseOnCleanup(t, h)
	// A record that cannot be removed (a directory with a file) does not fail
	// the finish; the sweep keeps trying.
	req := &ownsandv1.WriteFileRequest{SandboxId: testSandbox, WriteId: newID(t), Path: target, BindingSalt: make([]byte, 32), Data: []byte("x"), Finish: &ownsandv1.WriteFileFinish{SizeBytes: 1}}
	rec := h.srv.writes.recordPath(req.GetWriteId())
	if err := os.MkdirAll(filepath.Join(rec, "blocker"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Saving the record fails as well (rename onto a directory), which only logs.
	if resp, errd := h.write(req); errd != nil || !resp.GetFinished() {
		t.Fatalf("finish %v %v", resp, errd)
	}
	if got, _ := os.ReadFile(target); string(got) != "x" {
		t.Fatalf("target %q", got)
	}
	h.clock.Advance(16 * time.Minute)
	h.srv.Sweep()
	h.srv.writes.mu.Lock()
	_, pending := h.srv.writes.entries[req.GetWriteId()]
	h.srv.writes.mu.Unlock()
	if !pending {
		t.Fatal("the entry was forgotten while its record still exists")
	}
	if err := os.RemoveAll(rec); err != nil {
		t.Fatal(err)
	}
	h.srv.Sweep()
	h.srv.writes.mu.Lock()
	_, pending = h.srv.writes.entries[req.GetWriteId()]
	h.srv.writes.mu.Unlock()
	if pending {
		t.Fatal("the entry stayed after its cleanup succeeded")
	}
}

// A staging file lookup that fails for a reason other than a missing file
// (a transient I/O error, a permission denial) does not show that the file is
// gone. The record is kept and the write is registered, so the sweep retries
// the cleanup after the lookup works again.
func TestWriteRecoveryKeepsRecordWhenStagingLookupFails(t *testing.T) {
	h := newHarness(t, nil)
	target, req := startPartialWrite(t, h)
	tmp := tempPath(target, req.GetWriteId())
	releaseWriteFiles(h)

	orig := lstatStaging
	t.Cleanup(func() { lstatStaging = orig })
	lstatStaging = func(path string) (os.FileInfo, error) {
		return nil, &fs.PathError{Op: "lstat", Path: path, Err: errors.New("input/output error")}
	}
	h2 := restart(t, h, nil)
	lstatStaging = orig

	if got := recordFiles(t, h2); len(got) != 1 {
		t.Fatalf("records after a failed lookup: %v", got)
	}
	h2.srv.writes.mu.Lock()
	_, registered := h2.srv.writes.entries[req.GetWriteId()]
	h2.srv.writes.mu.Unlock()
	if !registered {
		t.Fatal("the write was not registered as recovered")
	}
	h2.clock.Advance(16 * time.Minute)
	h2.srv.Sweep()
	if exists(tmp) {
		t.Fatal("staging file kept after the sweep that follows a failed lookup")
	}
	if got := recordFiles(t, h2); len(got) != 0 {
		t.Fatalf("record kept after cleanup: %v", got)
	}
}

func TestWriteCleanupRetriesAfterFailure(t *testing.T) {
	h := newHarness(t, nil)
	target, req := startPartialWrite(t, h)
	tmp := tempPath(target, req.GetWriteId())
	fail := true
	h.srv.writes.removeFile = func(path string) error {
		if fail {
			return &fs.PathError{Op: "remove", Path: path, Err: errors.New("device busy")}
		}
		return os.Remove(path)
	}
	h.clock.Advance(16 * time.Minute)
	h.srv.Sweep()
	if !exists(tmp) {
		t.Fatal("staging file vanished although its removal failed")
	}
	if got := recordFiles(t, h); len(got) != 1 {
		t.Fatalf("record must stay while the staging file stays: %v", got)
	}
	// The write is expired for callers even while its cleanup is pending.
	if _, errd := h.write(req); errd.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND {
		t.Fatalf("write while cleanup is pending: %v", errd)
	}
	// A restart in between recovers the record and retries.
	h2 := restart(t, h, nil)
	fail2 := true
	h2.srv.writes.removeFile = func(path string) error {
		if fail2 {
			return errors.New("device busy")
		}
		return os.Remove(path)
	}
	h2.srv.Sweep()
	if !exists(tmp) || len(recordFiles(t, h2)) != 1 {
		t.Fatal("cleanup must wait for a working removal")
	}
	fail2 = false
	h2.srv.Sweep()
	if exists(tmp) || len(recordFiles(t, h2)) != 0 {
		t.Fatal("cleanup did not finish after the removal worked")
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestWriteRecordFailuresLogWriteIDWithoutPath(t *testing.T) {
	var logs syncBuffer
	h := newHarness(t, func(c *Config) { c.Logger = slog.New(slog.NewJSONHandler(&logs, nil)) })
	// Without the record directory every save fails; the upload still works.
	if err := os.RemoveAll(filepath.Join(h.srv.cfg.StateDir, "writes")); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	releaseOnCleanup(t, h)
	target := filepath.Join(dir, "secret-name")
	req := &ownsandv1.WriteFileRequest{SandboxId: testSandbox, WriteId: newID(t), Path: target, BindingSalt: make([]byte, 32), Data: []byte("x"), Finish: &ownsandv1.WriteFileFinish{SizeBytes: 1}}
	if resp, errd := h.write(req); errd != nil || !resp.GetFinished() {
		t.Fatalf("write without records: %v %v", resp, errd)
	}
	out := logs.String()
	if !strings.Contains(out, "save write record failed") || !strings.Contains(out, req.GetWriteId()) {
		t.Fatalf("missing log with the write id: %s", out)
	}
	for _, secret := range []string{dir, "secret-name", h.srv.cfg.StateDir} {
		if strings.Contains(out, secret) {
			t.Fatalf("log contains %q: %s", secret, out)
		}
	}
}

func TestSanitizedErrorOmitsPaths(t *testing.T) {
	_, err := os.ReadFile(filepath.Join(t.TempDir(), "secret-name"))
	got := sanitizedError(err)
	if strings.Contains(got, "secret-name") || got == "" {
		t.Fatalf("sanitized error %q", got)
	}
	if got := sanitizedError(&os.LinkError{Op: "rename", Old: "/a/secret", New: "/b/secret", Err: errors.New("denied")}); got != "denied" {
		t.Fatalf("sanitized link error %q", got)
	}
}

func TestFileIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fromFile, err := identityOfFile(f)
	if errors.Is(err, errors.ErrUnsupported) {
		t.Skip("no file identity on this platform")
	}
	if err != nil {
		t.Fatal(err)
	}
	fromPath, err := identityOfPath(path)
	if err != nil || fromPath != fromFile {
		t.Fatalf("identity by path %v differs from identity by handle %v (%v)", fromPath, fromFile, err)
	}
	other := filepath.Join(filepath.Dir(path), "g")
	if err := os.WriteFile(other, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if otherID, err := identityOfPath(other); err != nil || otherID == fromFile {
		t.Fatalf("two files share an identity: %v %v", otherID, err)
	}
	if _, err := identityOfPath(filepath.Join(filepath.Dir(path), "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing file: %v", err)
	}
}
