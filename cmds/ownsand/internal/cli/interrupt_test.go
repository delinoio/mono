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

const ctxReaderPhaseTimeout = 30 * time.Second

func readCtxAsync(r *ctxReader, buf []byte) <-chan readResult {
	done := make(chan readResult, 1)
	go func() {
		n, err := r.Read(buf)
		done <- readResult{n: n, err: err}
	}()
	return done
}

func cancelReadAfterStart(t *testing.T, cancel context.CancelFunc, started <-chan struct{}, done <-chan readResult) {
	t.Helper()
	select {
	case <-started:
	case end := <-done:
		t.Fatalf("Read returned (%d, %v) before the underlying read started", end.n, end.err)
	case <-time.After(ctxReaderPhaseTimeout):
		t.Fatal("the underlying read did not start before the deadline")
	}
	select {
	case end := <-done:
		t.Fatalf("Read returned (%d, %v) before cancellation", end.n, end.err)
	default:
	}
	cancel()
}

func requireCanceledRead(t *testing.T, done <-chan readResult, phase string) {
	t.Helper()
	select {
	case end := <-done:
		if end.n != 0 || !errors.Is(end.err, context.Canceled) {
			t.Fatalf("%s returned (%d, %v), want (0, context.Canceled)", phase, end.n, end.err)
		}
	case <-time.After(ctxReaderPhaseTimeout):
		t.Fatalf("%s did not return before the deadline", phase)
	}
}

func TestCtxReaderReturnsTheContextErrorWhileBlocked(t *testing.T) {
	stdin := blockedStdin(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cr := newCtxReader(ctx, stdin)
	buf := make([]byte, 8)
	done := readCtxAsync(cr, buf)
	cancelReadAfterStart(t, cancel, stdin.started, done)
	requireCanceledRead(t, done, "initial Read after cancellation")
	requireCanceledRead(t, readCtxAsync(cr, buf), "repeat Read after cancellation")
}

type pipeReadRecorder struct {
	pipe     *io.PipeReader
	received chan string
}

func (r *pipeReadRecorder) Read(buf []byte) (int, error) {
	n, err := r.pipe.Read(buf)
	r.received <- string(buf[:n])
	return n, err
}

// After Read returned for the context, a late result of the abandoned read
// must not reach the caller's buffer.
func TestCtxReaderDoesNotWriteToTheCallerBufferAfterCancellation(t *testing.T) {
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close(); _ = pr.Close() })
	received := make(chan string, 1)
	stdin := newNotifyReader(&pipeReadRecorder{pipe: pr, received: received})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cr := newCtxReader(ctx, stdin)
	const sentinel = "........"
	buf := []byte(sentinel)
	done := readCtxAsync(cr, buf)
	cancelReadAfterStart(t, cancel, stdin.started, done)
	requireCanceledRead(t, done, "initial Read after cancellation")

	const late = "late"
	written := make(chan readResult, 1)
	go func() {
		n, err := pw.Write([]byte(late))
		written <- readResult{n: n, err: err}
	}()
	select {
	case result := <-written:
		if result.n != len(late) || result.err != nil {
			t.Fatalf("late pipe write returned (%d, %v), want (%d, nil)", result.n, result.err, len(late))
		}
	case <-time.After(ctxReaderPhaseTimeout):
		t.Fatal("the late pipe write did not complete before the deadline")
	}
	select {
	case got := <-received:
		if got != late {
			t.Fatalf("the abandoned read received %q, want %q", got, late)
		}
	case <-time.After(ctxReaderPhaseTimeout):
		t.Fatal("the abandoned read did not report receiving late input before the deadline")
	}
	if string(buf) != sentinel {
		t.Fatalf("the caller buffer contains %q after cancellation, want %q", buf, sentinel)
	}
}
