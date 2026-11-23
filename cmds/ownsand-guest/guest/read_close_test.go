// SPDX-License-Identifier: FSL-1.1-ALv2

package guest

import (
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
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	first.reads.openHook = func() {
		started <- struct{}{}
		<-release
	}
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
	close(release)
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
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	srv.reads.openHook = func() {
		started <- struct{}{}
		<-release
	}
	e, err := srv.reads.begin("read-close-wait", src)
	if err != nil {
		t.Fatal(err)
	}
	waitStarted(t, started)
	go func() {
		time.Sleep(50 * time.Millisecond)
		close(release)
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
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	// The worker pauses after its copy, so it already created its file.
	first.reads.copyHook = func() {
		started <- struct{}{}
		<-release
	}
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
	close(release)
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
