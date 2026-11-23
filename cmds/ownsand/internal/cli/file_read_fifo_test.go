// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package cli

import (
	"bytes"
	"context"
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
	if res := r.runCtx(ctx, "file", "read", "--to", fifo, r.f.sb, "/tmp/x"); res.code != ExitError {
		t.Fatalf("cancelled read: %+v", res)
	}
	f, err := os.Open(fifo)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	done := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(f)
		done <- b
	}()
	select {
	case b := <-done:
		if len(b) != 0 {
			t.Fatalf("late reader received %q", b)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the abandoned open kept the FIFO write end open")
	}
}
