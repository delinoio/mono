// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func makeFIFO(t *testing.T) string {
	t.Helper()
	fifo := filepath.Join(t.TempDir(), "out.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo is not available: %v", err)
	}
	return fifo
}

func openTestFIFOReader(ctx context.Context, fifo string) (*os.File, error) {
	type openedFile struct {
		file *os.File
		err  error
	}
	opened := make(chan openedFile)
	done := make(chan struct{})
	go func() {
		defer close(done)
		f, err := os.Open(fifo)
		select {
		case opened <- openedFile{file: f, err: err}:
		case <-ctx.Done():
			if f != nil {
				_ = f.Close()
			}
		}
	}()
	select {
	case result := <-opened:
		return result.file, result.err
	case <-ctx.Done():
		waitErr := fmt.Errorf("wait for FIFO writer: %w", ctx.Err())
		// The blocking open above confirms a real writer arrived, even if it
		// closed immediately. On timeout, supply a writer to release that
		// open. Hold a second reader so the rescue writer can open even if
		// the goroutine has yet to enter os.Open.
		reader, err := os.OpenFile(fifo, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return nil, errors.Join(waitErr, err)
		}
		defer reader.Close()
		writer, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return nil, errors.Join(waitErr, err)
		}
		defer writer.Close()
		<-done
		return nil, waitErr
	}
}

type testFIFOReader struct {
	ctx context.Context
	fd  int
}

func (r testFIFOReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		if err := r.ctx.Err(); err != nil {
			return 0, err
		}
		n, err := syscall.Read(r.fd, p)
		switch err {
		case nil:
			if n == 0 {
				return 0, io.EOF
			}
			return n, nil
		case syscall.EINTR:
			continue
		case syscall.EAGAIN:
			select {
			case <-r.ctx.Done():
				return 0, r.ctx.Err()
			case <-time.After(10 * time.Millisecond):
			}
		default:
			return 0, err
		}
	}
}

func readEmptyTestFIFO(fifo string) error {
	// Cleanup also calls this helper after the test context is canceled.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	f, err := openTestFIFOReader(ctx, fifo)
	if err != nil {
		return err
	}
	defer f.Close()
	// Go does not support FIFO read deadlines on macOS. Set nonblocking
	// mode after the rendezvous and use the same deadline while reading.
	fd := int(f.Fd())
	if err := syscall.SetNonblock(fd, true); err != nil {
		return fmt.Errorf("set FIFO reader to nonblocking: %w", err)
	}
	data, err := io.ReadAll(testFIFOReader{ctx: ctx, fd: fd})
	if err != nil {
		return fmt.Errorf("read FIFO through EOF: %w", err)
	}
	if len(data) != 0 {
		return fmt.Errorf("late reader received %q", data)
	}
	return nil
}

// runCtx runs one invocation on ctx, so that a test can cancel the command as
// an interrupt does.
func (r *runner) runCtx(ctx context.Context, args ...string) result {
	r.t.Helper()
	var out, errb bytes.Buffer
	code := Run(ctx, Env{
		Args:   args,
		Stdin:  strings.NewReader(r.stdin),
		Stdout: &out,
		Stderr: &errb,
		LookupEnv: func(k string) (string, bool) {
			v, ok := r.env[k]
			return v, ok
		},
		LoginPollInterval: 10 * time.Millisecond,
	})
	return result{code: code, stdout: out.String(), stderr: errb.String()}
}

// A FIFO without a reader blocks open(2) for writing. Cancelling the command
// must end the wait before any request reaches the API.
func TestFileReadFIFOWithoutReaderEndsOnCancel(t *testing.T) {
	r := newRunner(t)
	fifo := makeFIFO(t)
	t.Cleanup(func() {
		// Release the canceled open and wait for its writer to close before
		// TempDir removes the FIFO.
		if err := readEmptyTestFIFO(fifo); err != nil {
			t.Errorf("clean up canceled FIFO open: %v", err)
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan result, 1)
	go func() {
		done <- r.runCtx(ctx, "file", "read", "--to", fifo, r.f.sb, "/tmp/x")
	}()
	select {
	case res := <-done:
		t.Fatalf("the command ended before the cancellation, without a reader: %+v", res)
	case <-time.After(300 * time.Millisecond):
	}
	cancel()
	select {
	case res := <-done:
		if res.code != ExitError || !strings.Contains(res.stderr, context.Canceled.Error()) {
			t.Fatalf("want exit %d and a cancellation error, got %+v", ExitError, res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the command did not end after the cancellation")
	}
	if calls := r.f.callNames(); len(calls) != 0 {
		t.Fatalf("API called while waiting for a FIFO reader: %v", calls)
	}
}

// A FIFO with a reader receives the bytes, as before.
func TestFileReadFIFOWithReaderReceivesBytes(t *testing.T) {
	r := newRunner(t)
	r.stdin = "fifo contents"
	if res := r.run("file", "write", r.f.sb, "/tmp/x"); res.code != 0 {
		t.Fatalf("write: %+v", res)
	}
	fifo := makeFIFO(t)
	got := make(chan []byte, 1)
	go func() {
		f, err := os.Open(fifo)
		if err != nil {
			got <- nil
			return
		}
		defer f.Close()
		b, _ := io.ReadAll(f)
		got <- b
	}()
	if res := r.run("file", "read", "--to", fifo, r.f.sb, "/tmp/x"); res.code != 0 {
		t.Fatalf("read --to FIFO: %+v", res)
	}
	select {
	case b := <-got:
		if string(b) != "fifo contents" {
			t.Fatalf("FIFO received %q", b)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the reader received nothing")
	}
}

// A reader that attaches after the cancellation must not leave the
// descriptor of the abandoned open behind: the open completes and is closed,
// so the reader sees end of file.
func TestFileReadFIFOLateReaderSeesEOF(t *testing.T) {
	r := newRunner(t)
	fifo := makeFIFO(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if res := r.runCtx(ctx, "file", "read", "--to", fifo, r.f.sb, "/tmp/x"); res.code != ExitError || !strings.Contains(res.stderr, context.Canceled.Error()) {
		t.Fatalf("want exit %d and a cancellation error, got %+v", ExitError, res)
	}
	if err := readEmptyTestFIFO(fifo); err != nil {
		t.Fatal(err)
	}
}
