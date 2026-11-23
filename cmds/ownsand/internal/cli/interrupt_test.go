// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// notifyReader reports the first Read call on started and then delegates, so
// a test knows that the command is blocked in a read of standard input.
type notifyReader struct {
	r       io.Reader
	once    sync.Once
	started chan struct{}
}

func newNotifyReader(r io.Reader) *notifyReader {
	return &notifyReader{r: r, started: make(chan struct{})}
}

func (n *notifyReader) Read(p []byte) (int, error) {
	n.once.Do(func() { close(n.started) })
	return n.r.Read(p)
}

// blockedStdin returns a standard input that delivers no data until the test
// ends. The test ends the blocked read in t.Cleanup, so that no goroutine of
// the test outlives it.
func blockedStdin(t *testing.T) *notifyReader {
	t.Helper()
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close(); _ = pr.Close() })
	return newNotifyReader(pr)
}

// runInterrupted runs one invocation with stdin that never delivers data,
// waits until the command reads it, cancels the context as an interrupt does
// and returns the result. The test fails when the command does not end after
// the cancellation.
func runInterrupted(t *testing.T, r *runner, args ...string) result {
	t.Helper()
	stdin := blockedStdin(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out, errb bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- Run(ctx, Env{
			Args:   args,
			Stdin:  stdin,
			Stdout: &out,
			Stderr: &errb,
			LookupEnv: func(k string) (string, bool) {
				v, ok := r.env[k]
				return v, ok
			},
			LoginPollInterval: 10 * time.Millisecond,
		})
	}()
	select {
	case <-stdin.started:
	case code := <-done:
		t.Fatalf("the command ended with %d before it read standard input: %s", code, errb.String())
	case <-time.After(30 * time.Second):
		t.Fatal("the command did not read standard input")
	}
	cancel()
	select {
	case code := <-done:
		return result{code: code, stdout: out.String(), stderr: errb.String()}
	case <-time.After(30 * time.Second):
		t.Fatal("the command did not end after the cancellation")
	}
	return result{}
}

func TestMemberAcceptInterruptedWhileReadingStandardInput(t *testing.T) {
	r := newRunner(t)
	res := runInterrupted(t, r, "--no-color", "member", "accept")
	if res.code != ExitInterrupted {
		t.Fatalf("exit %d, want %d (stderr %q)", res.code, ExitInterrupted, res.stderr)
	}
	if !strings.Contains(res.stderr, "error: interrupted") {
		t.Fatalf("stderr %q does not say that the command was interrupted", res.stderr)
	}
	for _, c := range r.f.callNames() {
		if c == "AcceptInvitation" {
			t.Fatal("AcceptInvitation was sent after the interrupt")
		}
	}
}

func TestFileWriteInterruptedWhileReadingStandardInput(t *testing.T) {
	r := newRunner(t)
	res := runInterrupted(t, r, "--no-color", "file", "write", r.f.sb, "/tmp/x")
	if res.code != ExitInterrupted {
		t.Fatalf("exit %d, want %d (stderr %q)", res.code, ExitInterrupted, res.stderr)
	}
	if !strings.Contains(res.stderr, "interrupted; the write was not finished") {
		t.Fatalf("stderr %q does not say that the write was not finished", res.stderr)
	}
	r.f.mu.Lock()
	writes := r.f.writes
	r.f.mu.Unlock()
	if writes != 0 {
		t.Fatalf("%d WriteFile requests were sent after the interrupt", writes)
	}
}

// Data on standard input and in --secret-file keeps working through the
// ctx-aware reader.
func TestMemberAcceptAndFileWriteStillReadStandardInput(t *testing.T) {
	r := newRunner(t)
	r.stdin = "osi_from_stdin\n"
	if res := r.run("member", "accept"); res.code != ExitOK {
		t.Fatalf("member accept: exit %d (stderr %q)", res.code, res.stderr)
	}
	r.stdin = "hello"
	if res := r.run("file", "write", r.f.sb, "/tmp/x"); res.code != ExitOK {
		t.Fatalf("file write: exit %d (stderr %q)", res.code, res.stderr)
	}
}

func TestCtxReaderPassesDataAndEOF(t *testing.T) {
	cr := newCtxReader(context.Background(), strings.NewReader("abc"))
	got, err := io.ReadAll(cr)
	if err != nil || string(got) != "abc" {
		t.Fatalf("ReadAll = %q, %v", got, err)
	}
}

func TestCtxReaderReturnsTheContextErrorWhileBlocked(t *testing.T) {
	stdin := blockedStdin(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cr := newCtxReader(ctx, stdin)
	type readEnd struct {
		n   int
		err error
	}
	done := make(chan readEnd, 1)
	buf := make([]byte, 8)
	go func() {
		n, err := cr.Read(buf)
		done <- readEnd{n, err}
	}()
	<-stdin.started
	cancel()
	select {
	case end := <-done:
		if end.n != 0 || !errors.Is(end.err, context.Canceled) {
			t.Fatalf("Read = %d, %v; want 0 and context.Canceled", end.n, end.err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Read did not return after the cancellation")
	}
	if n, err := cr.Read(buf); n != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("Read after the cancellation = %d, %v", n, err)
	}
}

// After Read returned for the context, a late result of the abandoned read
// must not reach the caller's buffer.
func TestCtxReaderDoesNotWriteToTheCallerBufferAfterCancellation(t *testing.T) {
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close(); _ = pr.Close() })
	stdin := newNotifyReader(pr)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cr := newCtxReader(ctx, stdin)
	buf := bytes.Repeat([]byte{'.'}, 8)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = cr.Read(buf)
	}()
	<-stdin.started
	cancel()
	<-done
	// Unblock the abandoned read with data. Write returns after the read
	// has consumed the data.
	if _, err := pw.Write([]byte("late")); err != nil {
		t.Fatalf("write to the pipe: %v", err)
	}
	if string(buf) != "........" {
		t.Fatalf("the caller buffer changed after the cancellation: %q", buf)
	}
}
