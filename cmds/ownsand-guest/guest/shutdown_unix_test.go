// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build !windows

package guest

import (
	"context"
	"os"
	"path/filepath"
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
	select {
	case err := <-rig.result:
		t.Fatalf("Run returned %v while a stream was open", err)
	case <-time.After(500 * time.Millisecond):
	}

	release()
	evs := s.collectExec()
	if stdout, _ := outputs(evs); stdout != "readydone" || evs[len(evs)-1].GetExited().GetExitCode() != 0 {
		t.Fatalf("exec events %v", evs)
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

func TestRunClosesConnectionsAfterGrace(t *testing.T) {
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
}
