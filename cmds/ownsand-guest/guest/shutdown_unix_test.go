// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build !windows

package guest

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// gatedExec returns a request for a command that prints "ready", waits for
// the release file and prints "done", and a function that releases it.
func gatedExec(t *testing.T) (req *ownsandv1.ExecRequest, release func()) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "release")
	r := execReq(t, "/bin/sh", "-c", `printf ready; while [ ! -e "$RELEASE" ]; do sleep 0.02; done; printf done`)
	r.Env = map[string]string{"RELEASE": file}
	release = func() { _ = os.WriteFile(file, nil, 0o600) }
	// An exec that nobody released must not outlive the test.
	t.Cleanup(release)
	return r, release
}

func TestRunWaitsForOpenStreamAfterContextEnds(t *testing.T) {
	t.Parallel()
	rig := startShutdownRig(t)
	req, release := gatedExec(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s := &stream{t: t, s: rig.client.Serve(ctx)}
	defer s.s.CloseRequest()
	s.send(1, req)
	if f := s.recv(); f.GetExecEvent().GetStarted() == nil {
		t.Fatalf("first frame %v", f)
	}

	rig.stop()
	rig.waitForShutdownWithOpenRequest(t, 500*time.Millisecond)

	release()
	evs := s.collectExec()
	if stdout, _ := outputs(evs); stdout != "readydone" {
		t.Fatalf("exec events %v", evs)
	}
	if err := requireExplicitSuccess(evs[len(evs)-1]); err != nil {
		t.Fatalf("exec events %v: %v", evs, err)
	}
	s.s.CloseRequest()
	select {
	case err := <-rig.result:
		if err != nil {
			t.Fatalf("Run returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the stream ended")
	}
}

func TestRunClosesConnectionsAfterGrace(t *testing.T) { //nolint:paralleltest // See the "Not parallel" comment below.
	// Not parallel: the test replaces package-level variables (shutdownGrace)
	// that other tests read.
	setShutdownGrace(t, 600*time.Millisecond)
	rig := startShutdownRig(t)
	req, _ := gatedExec(t) // Never released: the stream outlasts the grace.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s := &stream{t: t, s: rig.client.Serve(ctx)}
	defer s.s.CloseRequest()
	s.send(1, req)
	if f := s.recv(); f.GetExecEvent().GetStarted() == nil {
		t.Fatalf("first frame %v", f)
	}
	const ready = "ready"
	for stdout := ""; stdout != ready; {
		f := s.recv()
		chunk := f.GetExecEvent().GetStdout()
		if chunk == nil {
			t.Fatalf("expected stdout while waiting for %q, got %v", ready, f)
		}
		stdout += string(chunk.GetData())
		if !strings.HasPrefix(ready, stdout) {
			t.Fatalf("expected stdout %q, got %q", ready, stdout)
		}
	}

	start := time.Now()
	rig.stop()
	select {
	case err := <-rig.result:
		if err != nil {
			t.Fatalf("Run returned %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after the grace period")
	}
	if d := time.Since(start); d < 500*time.Millisecond {
		t.Fatalf("Run returned after %v, before the grace period ended", d)
	}

	// A pending HTTP/2 read can outlast the client context deadline.
	receiveErr := make(chan error, 1)
	go func() {
		_, err := s.s.Receive()
		receiveErr <- err
	}()
	select {
	case err := <-receiveErr:
		if ctxErr := ctx.Err(); ctxErr != nil {
			t.Fatalf("exec stream did not close before the client context ended: receive: %v, context: %v", err, ctxErr)
		}
		if err == nil || errors.Is(err, io.EOF) {
			t.Fatalf("expected an exec stream interruption after shutdown, got %v", err)
		}
		t.Logf("exec stream interrupted with the client context active: %v", err)
	case <-ctx.Done():
		t.Fatalf("exec stream did not close before the client context ended: %v", ctx.Err())
	}
}
