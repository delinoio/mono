// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"

	ownsand "github.com/delinoio/mono/go/ownsand"
)

// TestLoginPromptWriteFailureStopsBeforePolling runs login with a standard
// error that rejects the sign-in instructions. The user cannot enter a code
// that they never saw, so login must exit with status 1 and the write error
// before it sends any token request.
func TestLoginPromptWriteFailureStopsBeforePolling(t *testing.T) {
	r := newRunner(t)
	delete(r.env, ownsand.EnvAPIKey)
	lg := &fakeLogto{}
	ls := httptest.NewServer(lg.handler(t))
	defer ls.Close()
	r.env[EnvLogtoEndpoint] = ls.URL
	r.env[EnvLogtoAppID] = "app"

	var out bytes.Buffer
	stderr := &failingWriter{failAfter: 0, err: errDiskFull}
	if code := r.runWriters(&out, stderr, "login"); code != ExitError {
		t.Fatalf("login with a failing standard error: exit %d, want %d", code, ExitError)
	}
	lg.mu.Lock()
	polls := lg.polls
	lg.mu.Unlock()
	if polls != 0 {
		t.Fatalf("login polled the token endpoint %d times after the prompt write failed", polls)
	}
	if out.Len() != 0 {
		t.Fatalf("login wrote to standard output: %q", out.String())
	}
}

// TestLoginPromptWriteFailureReportsError checks that the exit message names
// the write error. The failing writer receives the instructions and the
// error message, so the error text is checked on a stream that fails only
// the first write.
func TestLoginPromptWriteFailureReportsError(t *testing.T) {
	r := newRunner(t)
	delete(r.env, ownsand.EnvAPIKey)
	lg := &fakeLogto{}
	ls := httptest.NewServer(lg.handler(t))
	defer ls.Close()
	r.env[EnvLogtoEndpoint] = ls.URL
	r.env[EnvLogtoAppID] = "app"

	stderr := &firstWriteFails{err: errDiskFull}
	if code := r.runWriters(&bytes.Buffer{}, stderr, "login"); code != ExitError {
		t.Fatalf("exit %d, want %d", code, ExitError)
	}
	if !strings.Contains(stderr.buf.String(), errDiskFull.Error()) {
		t.Fatalf("standard error %q does not report the write error", stderr.buf.String())
	}
}

// TestLoginPromptWriteSuccessKeepsPolling checks that a working standard
// error still prints the instructions and starts the token polling.
func TestLoginPromptWriteSuccessKeepsPolling(t *testing.T) {
	r := newRunner(t)
	delete(r.env, ownsand.EnvAPIKey)
	lg := &fakeLogto{}
	ls := httptest.NewServer(lg.handler(t))
	defer ls.Close()
	r.env[EnvLogtoEndpoint] = ls.URL
	r.env[EnvLogtoAppID] = "app"

	res := r.run("login")
	if res.code != ExitOK {
		t.Fatalf("login: %+v", res)
	}
	lg.mu.Lock()
	polls := lg.polls
	lg.mu.Unlock()
	if polls == 0 {
		t.Fatal("login did not poll the token endpoint")
	}
}

// firstWriteFails fails its first write and records every later one.
type firstWriteFails struct {
	err    error
	failed bool
	buf    bytes.Buffer
}

func (w *firstWriteFails) Write(p []byte) (int, error) {
	if !w.failed {
		w.failed = true
		return 0, w.err
	}
	return w.buf.Write(p)
}
