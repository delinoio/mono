// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	connectrpc "connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
)

// runCapturingStderr runs run with args and returns its exit code and what
// it wrote to os.Stderr.
func runCapturingStderr(t *testing.T, args []string) (int, string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	code := run(context.Background(), args, nil)
	os.Stderr = orig
	w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	return code, string(out)
}

func TestRunRejectsInvalidLogLevel(t *testing.T) {
	for _, level := range []string{"ınfo", "ınfo+1", "info+1", "warn-2", "İNFO", "verbose"} {
		// An invalid level must stop run before it starts a listener, so
		// an unreachable listen spec makes any later step fail visibly.
		code, stderr := runCapturingStderr(t, []string{"--log-level", level, "--listen", "unix://" + t.TempDir() + "/guest.sock"})
		if code != 2 {
			t.Errorf("log level %q: exit code %d, want 2", level, code)
		}
		want := "ownsand-guest: invalid log level \"" + level + "\"\n"
		if stderr != want {
			t.Errorf("log level %q: stderr %q, want %q", level, stderr, want)
		}
	}
}

func TestRunAcceptsASCIICaseFoldedLogLevel(t *testing.T) {
	// An invalid clock mode makes run return 2 right after the level check,
	// so the test reaches the step after level parsing without starting a
	// listener.
	for _, level := range []string{"DEBUG", "Warn", "Error", "iNfO"} {
		code, stderr := runCapturingStderr(t, []string{"--log-level", level, "--clock", "bogus"})
		if code != 2 {
			t.Errorf("log level %q: exit code %d, want 2", level, code)
		}
		if strings.Contains(stderr, "invalid log level") {
			t.Errorf("log level %q rejected: %q", level, stderr)
		}
		if !strings.Contains(stderr, "invalid clock mode") {
			t.Errorf("log level %q: stderr %q does not reach the clock check", level, stderr)
		}
	}
}

func TestRunListenFailureLogOmitsSocketPath(t *testing.T) {
	t.Chdir(t.TempDir())
	// The parent directory does not exist, so the bind fails.
	code, stderr := runCapturingStderr(t, []string{
		"--listen", "unix://no-such-dir-secret/guest-secret.sock",
		"--state-dir", "state", "--clock", "ignore",
	})
	if code != 1 {
		t.Fatalf("exit code %d, want 1; stderr %q", code, stderr)
	}
	if !strings.Contains(stderr, "listen failed") || !strings.Contains(stderr, "bind unix socket") {
		t.Fatalf("stderr %q does not report the bind failure", stderr)
	}
	for _, leak := range []string{"no-such-dir-secret", "guest-secret"} {
		if strings.Contains(stderr, leak) {
			t.Fatalf("stderr contains %q: %q", leak, stderr)
		}
	}
}

// syncBuffer is a log sink that run's goroutine and the test share.
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

// helperProcess is one run of the helper in a goroutine.
type helperProcess struct {
	stop context.CancelFunc
	code chan int
	// finished closes after run returned, whoever read code.
	finished chan struct{}
	log      *syncBuffer
}

func startHelper(t *testing.T, sock, stateDir string) *helperProcess {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h := &helperProcess{stop: cancel, code: make(chan int, 1), finished: make(chan struct{}), log: &syncBuffer{}}
	go func() {
		defer close(h.finished)
		h.code <- run(ctx, []string{"--listen", "unix://" + sock, "--state-dir", stateDir, "--clock", "ignore"},
			func(string) (io.Writer, error) { return h.log, nil })
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-h.finished:
		case <-time.After(30 * time.Second):
		}
	})
	return h
}

// unixClient returns a GuestService client that dials the unix socket.
func unixClient(sock string) ownsandv1connect.GuestServiceClient {
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	hc := &http.Client{Transport: &http.Transport{
		Protocols: &protocols,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}}
	return ownsandv1connect.NewGuestServiceClient(hc, "http://guest")
}

// waitServing blocks until the helper on sock answers CheckReady. A dial
// alone is not enough: the listener accepts connections before the helper
// owns its state directory.
func waitServing(t *testing.T, sock string) {
	t.Helper()
	client := unixClient(sock)
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, err := client.CheckReady(ctx, connectrpc.NewRequest(&ownsandv1.CheckReadyRequest{}))
		cancel()
		if err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("helper did not start serving")
}

func waitExit(t *testing.T, h *helperProcess) int {
	t.Helper()
	select {
	case code := <-h.code:
		return code
	case <-time.After(30 * time.Second):
		t.Fatal("helper did not exit")
		return -1
	}
}

func TestSecondHelperDoesNotTouchStateOfRunningHelper(t *testing.T) {
	stateDir := t.TempDir()
	sockDir := t.TempDir()
	sock1 := filepath.Join(sockDir, "a.sock")
	sock2 := filepath.Join(sockDir, "b.sock")

	first := startHelper(t, sock1, stateDir)
	waitServing(t, sock1)
	// A retained read snapshot of the running helper.
	snapshot := filepath.Join(stateDir, "reads", "snapshot")
	if err := os.WriteFile(snapshot, []byte("kept"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertSnapshot := func(when string) {
		t.Helper()
		if data, err := os.ReadFile(snapshot); err != nil || string(data) != "kept" {
			t.Fatalf("snapshot %s: %q, %v", when, data, err)
		}
	}

	// Same listener and same state directory: the bind fails.
	dup := startHelper(t, sock1, stateDir)
	if code := waitExit(t, dup); code != 1 {
		t.Fatalf("duplicate helper exit code %d, want 1; log %s", code, dup.log)
	}
	if !strings.Contains(dup.log.String(), "listen failed") {
		t.Fatalf("duplicate helper log %q does not report the listen failure", dup.log)
	}
	assertSnapshot("after the duplicate launch")
	waitServing(t, sock1) // The first helper still answers on its socket.

	// Another listener, same state directory: the state directory lock
	// fails the start, and the helper gives up its listener.
	other := startHelper(t, sock2, stateDir)
	if code := waitExit(t, other); code != 1 {
		t.Fatalf("helper on another listener exit code %d, want 1; log %s", code, other.log)
	}
	if log := other.log.String(); !strings.Contains(log, "start failed") || !strings.Contains(log, "state directory is in use") {
		t.Fatalf("helper on another listener log %q does not report the state directory lock", log)
	}
	for _, leak := range []string{stateDir, sockDir} {
		if strings.Contains(other.log.String(), leak) {
			t.Fatalf("log contains a path: %q", other.log)
		}
	}
	if _, err := os.Stat(sock2); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the failed helper left its socket: %v", err)
	}
	assertSnapshot("after the start on another listener")
	waitServing(t, sock1)

	// After the first helper stopped, a replacement starts and reclaims
	// the obsolete snapshot.
	first.stop()
	if code := waitExit(t, first); code != 0 {
		t.Fatalf("first helper exit code %d, want 0; log %s", code, first.log)
	}
	replacement := startHelper(t, sock2, stateDir)
	waitServing(t, sock2)
	if _, err := os.Stat(snapshot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("obsolete snapshot survived the replacement: %v", err)
	}
	replacement.stop()
	if code := waitExit(t, replacement); code != 0 {
		t.Fatalf("replacement exit code %d, want 0; log %s", code, replacement.log)
	}
}
