// SPDX-License-Identifier: FSL-1.1-ALv2

package guest

import (
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// These tests drive the write manager directly and inject failures at specific
// file operations without depending on directory permissions, timers or a server.

// reopenFixture is a write manager with one staged write that has all of its
// five bytes and a finish request that is ready to send.
type reopenFixture struct {
	m      *writeManager
	clock  *fakeClock
	id     string
	target string
	chunk  *ownsandv1.WriteFileRequest
	finish *ownsandv1.WriteFileRequest
}

// failRename makes the next n renames into place fail.
func failRename(t *testing.T, n int) *int {
	t.Helper()
	orig := renameFile
	t.Cleanup(func() { renameFile = orig })
	calls := 0
	renameFile = func(from, to string) error {
		calls++
		if calls <= n {
			return &os.LinkError{Op: "rename", Old: from, New: to, Err: fs.ErrPermission}
		}
		return orig(from, to)
	}
	return &calls
}

// failReopen makes the next n reopens of the staging file fail with err.
func failReopen(t *testing.T, n int, err error) *int {
	t.Helper()
	orig := openStagingFile
	t.Cleanup(func() { openStagingFile = orig })
	calls := 0
	openStagingFile = func(name string, flag int, perm os.FileMode) (*os.File, error) {
		calls++
		if calls <= n {
			return nil, &os.PathError{Op: "open", Path: name, Err: err}
		}
		return orig(name, flag, perm)
	}
	return &calls
}

func newReopenFixture(t *testing.T, mode uint32) *reopenFixture {
	t.Helper()
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	m, err := newWriteManager(t.TempDir(), clock.Now, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Close the staging files so the temporary directory can be removed on
		// Windows.
		m.mu.Lock()
		defer m.mu.Unlock()
		for _, e := range m.entries {
			if e.file != nil {
				e.file.Close()
			}
			e.closeModeFile()
		}
	})
	id := newID(t)
	target := filepath.Join(t.TempDir(), "out.bin")
	f := &reopenFixture{m: m, clock: clock, id: id, target: target}
	f.chunk = &ownsandv1.WriteFileRequest{WriteId: id, Path: target, Data: []byte("hello")}
	f.finish = &ownsandv1.WriteFileRequest{
		WriteId: id, Path: target, Data: []byte("hello"),
		Finish: &ownsandv1.WriteFileFinish{SizeBytes: 5, Mode: mode},
	}
	return f
}

func (f *reopenFixture) entry(t *testing.T) *writeEntry {
	t.Helper()
	f.m.mu.Lock()
	defer f.m.mu.Unlock()
	e := f.m.entries[f.id]
	if e == nil {
		t.Fatal("write entry is not registered")
	}
	return e
}

func wantOpError(t *testing.T, err error, code ownsandv1.ErrorCode, msg string) {
	t.Helper()
	var op *opError
	if !errors.As(err, &op) || op.code != code || op.msg != msg {
		t.Fatalf("error %v, want %v %q", err, code, msg)
	}
}

func (f *reopenFixture) stageAndFailClose(t *testing.T) {
	t.Helper()
	if _, err := f.m.apply(f.chunk); err != nil {
		t.Fatalf("first chunk: %v", err)
	}
	closeFile := closeStagingFile
	defer func() { closeStagingFile = closeFile }()
	closeStagingFile = func(file *os.File) error {
		if err := closeFile(file); err != nil {
			t.Fatalf("close staging file before injecting EIO: %v", err)
		}
		return syscall.EIO
	}

	_, err := f.m.apply(f.finish)
	wantOpError(t, err, ownsandv1.ErrorCode_ERROR_CODE_INTERNAL, "write: file system error")
	if e := f.entry(t); e.file != nil || e.finished || e.gone {
		t.Fatalf("entry after failed close: file set %v, finished %v, gone %v", e.file != nil, e.finished, e.gone)
	}
	if got, err := os.ReadFile(tempPath(f.target, f.id)); err != nil || string(got) != "hello" {
		t.Fatalf("staging file after failed close: %q %v", got, err)
	}
	if _, err := os.Stat(f.target); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("target after failed close: %v", err)
	}
}

func TestWriteFinishRetryReopensStagingFileAfterFailedClose(t *testing.T) {
	f := newReopenFixture(t, 0)
	f.stageAndFailClose(t)

	if resp, err := f.m.apply(f.finish); err != nil || !resp.GetFinished() || resp.GetSizeBytes() != 5 {
		t.Fatalf("repeated finish: %v %v", resp, err)
	}
	if got, err := os.ReadFile(f.target); err != nil || string(got) != "hello" {
		t.Fatalf("target: %q %v", got, err)
	}
	if _, err := os.Stat(tempPath(f.target, f.id)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("staging file left: %v", err)
	}
}

func TestWriteChunkReopensStagingFileAfterFailedClose(t *testing.T) {
	f := newReopenFixture(t, 0o444)
	f.stageAndFailClose(t)

	tail := &ownsandv1.WriteFileRequest{WriteId: f.id, Path: f.target, Offset: 5, Data: []byte(" world")}
	if _, err := f.m.apply(tail); err != nil {
		t.Fatalf("chunk after failed close: %v", err)
	}
	e := f.entry(t)
	if e.file == nil || len(e.ranges) != 1 || e.ranges[0] != (span{0, 11}) {
		t.Fatalf("entry after chunk: file open %v, ranges %v", e.file != nil, e.ranges)
	}
	if runtime.GOOS != "windows" {
		st, err := e.file.Stat()
		if err != nil {
			t.Fatal(err)
		}
		if mode := st.Mode().Perm(); mode != 0o600 {
			t.Fatalf("reopened staging mode: %04o, want 0600", mode)
		}
	}
	f.finish.Finish.SizeBytes = 11
	if resp, err := f.m.apply(f.finish); err != nil || !resp.GetFinished() || resp.GetSizeBytes() != 11 {
		t.Fatalf("finish after appended chunk: %v %v", resp, err)
	}
	if got, err := os.ReadFile(f.target); err != nil || string(got) != "hello world" {
		t.Fatalf("target: %q %v", got, err)
	}
}

// stageAndFailFinish stages the write and runs a finish whose rename and
// reopen both fail once, so the entry keeps no open file.
func (f *reopenFixture) stageAndFailFinish(t *testing.T) {
	t.Helper()
	renames := failRename(t, 1)
	reopens := failReopen(t, 1, fs.ErrPermission)
	if _, err := f.m.apply(f.chunk); err != nil {
		t.Fatalf("first chunk: %v", err)
	}
	if _, err := f.m.apply(f.finish); err == nil {
		t.Fatal("finish succeeded although the rename must fail")
	}
	if *renames != 1 || *reopens != 1 {
		t.Fatalf("rename calls %d, reopen calls %d, want 1 and 1", *renames, *reopens)
	}
	if e := f.entry(t); e.file != nil || e.finished || e.gone {
		t.Fatalf("entry after failed rename and reopen: file open %v, finished %v, gone %v", e.file != nil, e.finished, e.gone)
	}
	if _, err := os.Stat(f.target); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("target after failed rename: %v", err)
	}
}

func TestWriteFinishRetryReopensStagingFileAfterFailedReopen(t *testing.T) {
	f := newReopenFixture(t, 0o444)
	f.stageAndFailFinish(t)

	resp, err := f.m.apply(f.finish)
	if err != nil || !resp.GetFinished() || resp.GetSizeBytes() != 5 {
		t.Fatalf("repeated finish %v %v", resp, err)
	}
	got, readErr := os.ReadFile(f.target)
	if readErr != nil || string(got) != "hello" {
		t.Fatalf("target %q %v", got, readErr)
	}
	if runtime.GOOS != "windows" {
		if st, err := os.Stat(f.target); err != nil || st.Mode().Perm() != 0o444 {
			t.Fatalf("target mode %v %v, want 0444", st.Mode(), err)
		}
	}
	if _, err := os.Stat(tempPath(f.target, f.id)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("staging file left: %v", err)
	}
	// A repeat of the successful finish returns the stored result.
	again, err := f.m.apply(f.finish)
	if err != nil || again != resp {
		t.Fatalf("repeat after success %v %v, want the stored result", again, err)
	}
}

func TestWriteChunkReopensStagingFileAfterFailedReopen(t *testing.T) {
	f := newReopenFixture(t, 0)
	f.stageAndFailFinish(t)

	// A second chunk with a new range uses the reopened file and the written
	// ranges stay consistent with it.
	tail := &ownsandv1.WriteFileRequest{WriteId: f.id, Path: f.target, Offset: 5, Data: []byte(" world")}
	if _, err := f.m.apply(tail); err != nil {
		t.Fatalf("chunk after failed reopen: %v", err)
	}
	e := f.entry(t)
	if e.file == nil || len(e.ranges) != 1 || e.ranges[0] != (span{0, 11}) {
		t.Fatalf("entry after chunk: file open %v, ranges %v", e.file != nil, e.ranges)
	}
	fin := &ownsandv1.WriteFileRequest{
		WriteId: f.id, Path: f.target, Offset: 5, Data: []byte(" world"),
		Finish: &ownsandv1.WriteFileFinish{SizeBytes: 11},
	}
	if resp, err := f.m.apply(fin); err != nil || !resp.GetFinished() || resp.GetSizeBytes() != 11 {
		t.Fatalf("finish %v %v", resp, err)
	}
	if got, err := os.ReadFile(f.target); err != nil || string(got) != "hello world" {
		t.Fatalf("target %q %v", got, err)
	}
}

func TestWriteReopenFailureStaysRetryable(t *testing.T) {
	f := newReopenFixture(t, 0)
	f.stageAndFailFinish(t)

	// Two more temporary reopen failures: each one fails the call without
	// retiring the write.
	reopens := failReopen(t, 2, fs.ErrPermission)
	for i := 0; i < 2; i++ {
		_, err := f.m.apply(f.finish)
		wantOpError(t, err, ownsandv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED, "write: permission denied")
		if e := f.entry(t); e.gone || e.file != nil {
			t.Fatalf("write after temporary reopen failure %d: gone %v, file open %v", i+1, e.gone, e.file != nil)
		}
	}
	if resp, err := f.m.apply(f.finish); err != nil || !resp.GetFinished() {
		t.Fatalf("finish after the reopen works again: %v %v", resp, err)
	}
	if *reopens != 3 {
		t.Fatalf("reopen calls %d, want 3", *reopens)
	}
}

func TestWriteRetiresWriteWhenStagingFileIsGone(t *testing.T) {
	f := newReopenFixture(t, 0)
	f.stageAndFailFinish(t)
	tmp := tempPath(f.target, f.id)
	hadRecord := f.entry(t).identity != nil
	if err := os.Remove(tmp); err != nil {
		t.Fatal(err)
	}

	_, err := f.m.apply(f.finish)
	wantOpError(t, err, ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND, "write staging file is gone")
	if !f.entry(t).gone {
		t.Fatal("write is not marked gone")
	}
	// Later calls report the retired write as expired, not "staging file is
	// not open".
	for _, req := range []*ownsandv1.WriteFileRequest{f.finish, f.chunk} {
		_, err := f.m.apply(req)
		wantOpError(t, err, ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND, "write expired")
	}
	if hadRecord && !exists(f.m.recordPath(f.id)) {
		t.Fatal("record is gone before the sweep")
	}
	f.m.sweep(f.clock.Now())
	f.m.mu.Lock()
	_, registered := f.m.entries[f.id]
	_, expired := f.m.expired[f.id]
	f.m.mu.Unlock()
	if registered || !expired {
		t.Fatalf("after the sweep: registered %v, expired %v", registered, expired)
	}
	if exists(f.m.recordPath(f.id)) {
		t.Fatal("record left after the sweep")
	}
	_, err = f.m.apply(f.finish)
	wantOpError(t, err, ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND, "write expired")
}

func TestWriteRetiresWriteWhenReopenReportsMissingFile(t *testing.T) {
	f := newReopenFixture(t, 0)
	f.stageAndFailFinish(t)
	failReopen(t, 1, fs.ErrNotExist)

	_, err := f.m.apply(f.finish)
	wantOpError(t, err, ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND, "write staging file is gone")
	if !f.entry(t).gone {
		t.Fatal("write is not marked gone")
	}
}

func TestWriteDoesNotReopenReplacedStagingFile(t *testing.T) {
	f := newReopenFixture(t, 0)
	f.stageAndFailFinish(t)
	if f.entry(t).identity == nil {
		t.Skip("the platform keeps no staging file identity")
	}
	replaceStagingFile(t, f)

	f.checkRejectedReplacement(t)
}

func TestWriteDoesNotReopenReplacedStagingFileDuringModeRestore(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not restore staging permissions")
	}
	f := newReopenFixture(t, 0)
	f.stageAndFailFinish(t)

	chmod := chmodStagingFile
	t.Cleanup(func() { chmodStagingFile = chmod })
	chmodStagingFile = func(file *os.File, mode os.FileMode) error {
		// ensureOpen has verified the path. Change it before the mode
		// operation and let the writable reopen see the new file.
		replaceStagingFile(t, f)
		return chmod(file, mode)
	}

	f.checkRejectedReplacement(t)
}

func replaceStagingFile(t *testing.T, f *reopenFixture) {
	t.Helper()
	tmp := tempPath(f.target, f.id)
	// Keep the original inode or file index from being reused.
	if err := os.Link(tmp, tmp+".keep"); err != nil {
		t.Skipf("hard links are not available: %v", err)
	}
	if err := os.Remove(tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmp, []byte("foreign"), 0o644); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(tmp, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	checkReplacement(t, tmp)
}

func checkReplacement(t *testing.T, path string) {
	t.Helper()
	if got, err := os.ReadFile(path); err != nil || string(got) != "foreign" {
		t.Fatalf("replaced file %q %v", got, err)
	}
	if runtime.GOOS != "windows" {
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if mode := st.Mode().Perm(); mode != 0o644 {
			t.Fatalf("replacement mode %04o, want 0644", mode)
		}
	}
}

func (f *reopenFixture) checkRejectedReplacement(t *testing.T) {
	t.Helper()
	tmp := tempPath(f.target, f.id)
	_, err := f.m.apply(f.finish)
	wantOpError(t, err, ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND, "write staging file is gone")
	checkReplacement(t, tmp)
	_, err = f.m.apply(f.finish)
	wantOpError(t, err, ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND, "write expired")

	record := f.m.recordPath(f.id)
	if !exists(record) {
		t.Fatal("write record is gone before the sweep")
	}
	f.m.sweep(f.clock.Now())
	checkReplacement(t, tmp)
	if exists(record) {
		t.Fatal("write record left after the sweep")
	}
	_, err = f.m.apply(f.finish)
	wantOpError(t, err, ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND, "write expired")
	if _, err := os.Stat(f.target); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("target after a replaced staging file: %v", err)
	}
}

func TestWriteFinishRetryWithoutOwnerReadOrWriteMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows ignores the finish mode")
	}
	f := newReopenFixture(t, 0o100)
	f.stageAndFailFinish(t)

	if resp, err := f.m.apply(f.finish); err != nil || !resp.GetFinished() {
		t.Fatalf("repeated finish: %v %v", resp, err)
	}
	st, err := os.Stat(f.target)
	if err != nil {
		t.Fatal(err)
	}
	if mode := st.Mode().Perm(); mode != 0o100 {
		t.Fatalf("target mode %04o, want 0100", mode)
	}
	if err := os.Chmod(f.target, 0o400); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(f.target); err != nil || string(got) != "hello" {
		t.Fatalf("target: %q %v", got, err)
	}
}
