// SPDX-License-Identifier: FSL-1.1-ALv2

package guest

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
)

// shutdownRig is a server whose Run the test ends itself.
type shutdownRig struct {
	srv    *Server
	client ownsandv1connect.GuestServiceClient
	stop   context.CancelFunc
	// result receives the return value of Run.
	result chan error
}

func startShutdownRig(t *testing.T) *shutdownRig {
	t.Helper()
	srv, err := New(Config{StateDir: t.TempDir(), Version: "test", Clock: ClockModeIgnore, KeepaliveInterval: time.Hour, SweepInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		result <- srv.Run(ctx, l)
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
	return &shutdownRig{srv: srv, client: ownsandv1connect.NewGuestServiceClient(hc, "http://guest"), stop: cancel, result: result}
}

// setShutdownGrace replaces the grace period for the test.
func setShutdownGrace(t *testing.T, d time.Duration) {
	t.Helper()
	orig := shutdownGrace
	shutdownGrace = d
	t.Cleanup(func() { shutdownGrace = orig })
}

func TestRunReturnsPromptlyWhenIdle(t *testing.T) {
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
	openReplacement(t, rig.srv.cfg.StateDir, 0)
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

func openReplacement(t *testing.T, dir string, wait time.Duration) *Server {
	t.Helper()
	deadline := time.Now().Add(wait)
	for {
		replacement, err := New(Config{StateDir: dir})
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

func TestRunWriteShutdownLockHandoff(t *testing.T) {
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
				release()
			} else {
				select {
				case err := <-rig.result:
					t.Fatalf("Run returned %v while the write was blocked", err)
				case <-time.After(50 * time.Millisecond):
				}
				release()
				f := s.recv()
				if resp := f.GetFileWriteResult(); !resp.GetFinished() || resp.GetSizeBytes() != 1 {
					t.Fatalf("final write response: %v", f)
				}
				if err := s.s.CloseRequest(); err != nil {
					t.Fatal(err)
				}
				waitForRun(t, rig)
				rig.srv.Close()
			}

			var wait time.Duration
			if forced {
				wait = 10 * time.Second
			}
			replacement := openReplacement(t, dir, wait)
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
	replacement := openReplacement(t, dir, 0)
	h.srv.Close()
	requireStateDirBusy(t, replacement.cfg.StateDir)
}

func TestWriteCloseRejectsNewWork(t *testing.T) {
	h := newHarness(t, nil)
	dir := h.srv.cfg.StateDir
	target, req := startPartialWrite(t, h)
	staging := tempPath(target, req.GetWriteId())
	record := h.srv.writes.recordPath(req.GetWriteId())
	h.srv.Close()
	h.clock.Advance(time.Hour)
	h.srv.Sweep()
	for _, path := range []string{staging, record} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("sweep worked after Close: %s: %v", path, err)
		}
	}
	late := &ownsandv1.WriteFileRequest{
		SandboxId: testSandbox, WriteId: newID(t), Path: filepath.Join(filepath.Dir(target), "late"),
		BindingSalt: make([]byte, 32), Data: []byte("y"),
		Finish: &ownsandv1.WriteFileFinish{SizeBytes: 1},
	}
	if _, detail := h.write(late); detail.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_UNAVAILABLE {
		t.Fatalf("write after Close: %v, want unavailable", detail)
	}
	releaseWriteFiles(h)
	openReplacement(t, dir, 0)
}
