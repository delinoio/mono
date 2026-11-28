// SPDX-License-Identifier: FSL-1.1-ALv2

package guest

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"time"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
)

// shutdownRig is a server whose Run the test ends itself.
type shutdownRig struct {
	srv            *Server
	client         ownsandv1connect.GuestServiceClient
	clock          *fakeClock
	stop           context.CancelFunc
	listenerClosed <-chan struct{}
	// result receives the return value of Run.
	result chan error
}

type shutdownListener struct {
	net.Listener
	closed chan struct{}
}

func (l *shutdownListener) Close() error {
	err := l.Listener.Close()
	if err == nil {
		close(l.closed)
	}
	return err
}

func startShutdownRig(t *testing.T) *shutdownRig {
	t.Helper()
	clock := &fakeClock{now: time.Now()}
	srv, err := New(Config{StateDir: t.TempDir(), Version: "test", Clock: ClockModeIgnore, Now: clock.Now, KeepaliveInterval: time.Hour, SweepInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener := &shutdownListener{Listener: l, closed: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		result <- srv.Run(ctx, listener)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(30 * time.Second):
			t.Error("Run did not return at cleanup")
		}
		srv.execs.wg.Wait()
		// Release the state directory lock: Windows cannot delete the open
		// lock file when t.TempDir removes the state directory. This
		// cleanup runs after the one of t.TempDir was registered, so it
		// runs before it. Close is safe to call more than once.
		srv.Close()
	})
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	hc := &http.Client{Transport: &http.Transport{
		Protocols: &protocols,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", l.Addr().String())
		},
	}}
	return &shutdownRig{
		srv:            srv,
		client:         ownsandv1connect.NewGuestServiceClient(hc, "http://guest"),
		clock:          clock,
		stop:           cancel,
		listenerClosed: listener.closed,
		result:         result,
	}
}

func (rig *shutdownRig) waitForShutdownWithOpenRequest(t *testing.T, observation time.Duration) {
	t.Helper()
	select {
	case <-rig.listenerClosed:
	case err := <-rig.result:
		t.Fatalf("Run returned %v before listener closure was observed", err)
	case <-time.After(5 * time.Second):
		t.Fatal("listener did not close after shutdown was requested")
	}

	select {
	case err := <-rig.result:
		t.Fatalf("Run returned %v while the request was blocked during shutdown", err)
	case <-time.After(observation):
	}
	// The timer and Run can both be ready when the test goroutine resumes.
	select {
	case err := <-rig.result:
		t.Fatalf("Run returned %v while the request was blocked during shutdown", err)
	default:
	}
}

// setShutdownGrace replaces the grace period for the test.
func setShutdownGrace(t *testing.T, d time.Duration) {
	t.Helper()
	orig := shutdownGrace
	shutdownGrace = d
	t.Cleanup(func() { shutdownGrace = orig })
}

func TestRunReturnsPromptlyWhenIdle(t *testing.T) {
	t.Parallel()
	rig := startShutdownRig(t)
	start := time.Now()
	rig.stop()
	select {
	case err := <-rig.result:
		if err != nil {
			t.Fatalf("Run returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return for an idle server")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("idle Run took %v to return", d)
	}
	rig.srv.Close()
	openReplacement(t, Config{StateDir: rig.srv.cfg.StateDir}, 0)
}

func waitForRun(t *testing.T, rig *shutdownRig) {
	t.Helper()
	select {
	case err := <-rig.result:
		if err != nil {
			t.Fatalf("Run returned %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
	}
}

func requireStateDirBusy(t *testing.T, dir string) {
	t.Helper()
	replacement, err := New(Config{StateDir: dir})
	if err == nil {
		replacement.Close()
	}
	if !errors.Is(err, ErrStateDirInUse) {
		t.Fatalf("replacement start: %v, want ErrStateDirInUse", err)
	}
}

func openReplacement(t *testing.T, cfg Config, wait time.Duration) *Server {
	t.Helper()
	deadline := time.Now().Add(wait)
	for {
		replacement, err := New(cfg)
		if err == nil {
			t.Cleanup(func() { replacement.Close() })
			return replacement
		}
		if !errors.Is(err, ErrStateDirInUse) || time.Now().After(deadline) {
			t.Fatalf("replacement start: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func requireWriteArtifacts(t *testing.T, srv *Server, req *ownsandv1.WriteFileRequest, present bool) {
	t.Helper()
	for _, path := range []string{tempPath(req.GetPath(), req.GetWriteId()), srv.writes.recordPath(req.GetWriteId())} {
		_, err := os.Stat(path)
		if present && err != nil {
			t.Fatalf("expected staging file or recovery record %s: %v", path, err)
		}
		if !present && !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("expected staging file or recovery record %s to be absent: %v", path, err)
		}
	}
}

func requireStagingHandleClosed(t *testing.T, staging *os.File) {
	t.Helper()
	_, err := staging.Stat()
	// On Windows, File.Stat calls Win32 with the file handle and can
	// report ERROR_INVALID_HANDLE directly.
	const windowsInvalidHandle = syscall.Errno(6)
	if errors.Is(err, os.ErrClosed) || (runtime.GOOS == "windows" && errors.Is(err, windowsInvalidHandle)) {
		return
	}
	t.Fatalf("staging handle at handoff: %v, want a closed handle", err)
}

func TestRunWriteShutdownLockHandoff(t *testing.T) {
	// Not parallel: the test replaces package-level variables (renameFile, shutdownGrace)
	// that other tests read.
	for _, forced := range []bool{false, true} {
		name := "within_grace"
		if forced {
			name = "after_forced_close"
		}
		t.Run(name, func(t *testing.T) {
			if forced {
				setShutdownGrace(t, 250*time.Millisecond)
			}
			target := filepath.Join(t.TempDir(), "target")
			rig := startShutdownRig(t)
			dir := rig.srv.cfg.StateDir
			h := &harness{t: t, srv: rig.srv, client: rig.client, clock: rig.clock}
			idle := writePartialTo(t, h, filepath.Join(filepath.Dir(target), "idle"))
			staging := requireWriteEntry(t, rig.srv.writes, idle.GetWriteId()).file
			req := &ownsandv1.WriteFileRequest{
				SandboxId: testSandbox, WriteId: newID(t), Path: target,
				BindingSalt: make([]byte, 32), Data: []byte("x"),
				Finish: &ownsandv1.WriteFileFinish{SizeBytes: 1},
			}
			entered := make(chan struct{})
			resume := make(chan struct{})
			renamed := make(chan struct{})
			release := sync.OnceFunc(func() { close(resume) })
			defer release()
			rename := renameFile
			renameFile = func(from, to string) error {
				close(entered)
				<-resume
				defer close(renamed)
				return rename(from, to)
			}
			t.Cleanup(func() { renameFile = rename })

			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			s := &stream{t: t, s: rig.client.Serve(ctx)}
			s.send(1, req)
			t.Cleanup(func() {
				select {
				case <-entered:
					waitClosed(t, renamed)
				default:
				}
			})
			waitStarted(t, entered)
			rig.stop()
			if forced {
				waitForRun(t, rig)
				if _, err := s.s.Receive(); err == nil || ctx.Err() != nil {
					t.Fatalf("receive after forced close: %v, context: %v", err, ctx.Err())
				}
				rig.srv.Close()
				requireStateDirBusy(t, dir)
				if _, err := staging.Stat(); err != nil {
					t.Fatalf("idle staging handle before the held write drains: %v", err)
				}
				release()
			} else {
				rig.waitForShutdownWithOpenRequest(t, 50*time.Millisecond)
				release()
				f := s.recv()
				if resp := f.GetFileWriteResult(); !resp.GetFinished() || resp.GetSizeBytes() != 1 {
					t.Fatalf("final write response: %v", f)
				}
				if err := s.s.CloseRequest(); err != nil {
					t.Fatal(err)
				}
				waitForRun(t, rig)
				// Let the replacement contend for the lock during Close so
				// the check can observe a premature handoff.
				closed := make(chan struct{})
				go func() {
					defer close(closed)
					rig.srv.Close()
				}()
				t.Cleanup(func() { waitClosed(t, closed) })
			}

			replacement := openReplacement(t, Config{StateDir: dir, Now: rig.clock.Now}, 10*time.Second)
			requireStagingHandleClosed(t, staging)
			requireWriteArtifacts(t, rig.srv, idle, true)
			rig.clock.Advance(OperationRetention + time.Second)
			replacement.Sweep()
			requireWriteArtifacts(t, rig.srv, idle, false)
			if contents, err := os.ReadFile(target); err != nil || string(contents) != "x" {
				t.Fatalf("published target: %q, %v", contents, err)
			}
			records, err := os.ReadDir(replacement.writes.dir)
			if err != nil || len(records) != 0 {
				t.Fatalf("recovery records after handoff: %v, %v", records, err)
			}
		})
	}
}

func TestWriteSweepRetainsStateDirDuringConcurrentClose(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	dir := h.srv.cfg.StateDir
	target, req := startPartialWrite(t, h)
	staging := tempPath(target, req.GetWriteId())
	record := h.srv.writes.recordPath(req.GetWriteId())
	h.clock.Advance(time.Hour)

	entered := make(chan struct{})
	resume := make(chan struct{})
	h.srv.writes.removeFile = func(path string) error {
		close(entered)
		<-resume
		return os.Remove(path)
	}
	swept := make(chan struct{})
	go func() {
		defer close(swept)
		h.srv.Sweep()
	}()
	release := sync.OnceFunc(func() { close(resume) })
	t.Cleanup(func() {
		release()
		waitClosed(t, swept)
	})
	waitStarted(t, entered)

	closed := make(chan struct{})
	go func() {
		defer close(closed)
		var callers sync.WaitGroup
		for range 3 {
			callers.Go(func() { h.srv.Close() })
		}
		callers.Wait()
	}()
	waitClosed(t, closed)
	requireStateDirBusy(t, dir)
	release()
	waitClosed(t, swept)

	for _, path := range []string{staging, record} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("expired staging file or recovery record %s: %v", path, err)
		}
	}
	replacement := openReplacement(t, Config{StateDir: dir}, 0)
	h.srv.Close()
	requireStateDirBusy(t, replacement.cfg.StateDir)
}

func TestWriteCloseRejectsNewWork(t *testing.T) {
	t.Parallel()
	target := filepath.Join(t.TempDir(), "target")
	h := newHarness(t, nil)
	dir := h.srv.cfg.StateDir
	req := writePartialTo(t, h, target)
	entry := requireWriteEntry(t, h.srv.writes, req.GetWriteId())
	staging := entry.file
	h.srv.Close()
	requireStagingHandleClosed(t, staging)
	if entry.file != nil {
		t.Fatal("primary staging handle still registered after Close")
	}
	late := &ownsandv1.WriteFileRequest{
		SandboxId: testSandbox, WriteId: newID(t), Path: filepath.Join(filepath.Dir(target), "late"),
		BindingSalt: make([]byte, 32), Data: []byte("y"),
		Finish: &ownsandv1.WriteFileFinish{SizeBytes: 1},
	}
	if _, detail := h.write(late); detail.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_UNAVAILABLE {
		t.Fatalf("write after Close: %v, want unavailable", detail)
	}
	replacement := newHarness(t, func(cfg *Config) {
		cfg.StateDir = dir
		cfg.Now = h.clock.Now
	})
	requireWriteArtifacts(t, h.srv, req, true)
	assertWriteLostAfterRestart(t, replacement, req)
	h.clock.Advance(OperationRetention + time.Second)
	h.srv.Sweep()
	requireWriteArtifacts(t, h.srv, req, true)
	replacement.srv.Sweep()
	requireWriteArtifacts(t, h.srv, req, false)
	if got, err := os.ReadFile(target); err != nil || string(got) != "old" {
		t.Fatalf("original target after expiry: %q, %v", got, err)
	}
}
