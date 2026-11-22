// SPDX-License-Identifier: FSL-1.1-ALv2

package fcrt

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/go/ownsand/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
)

// recorder keeps the order of guest execs and Firecracker API calls.
type recorder struct {
	mu     sync.Mutex
	events []string
	// rejectResume makes the fake API answer a Resumed state with an error.
	rejectResume bool
}

func (r *recorder) add(e string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

// setRejectResume changes rejectResume while the fake API serves requests.
func (r *recorder) setRejectResume(reject bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rejectResume = reject
}

func (r *recorder) rejectsResume() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rejectResume
}

func (r *recorder) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

// fakeGuest answers every exec with a zero exit.
type fakeGuest struct {
	ownsandv1connect.UnimplementedGuestServiceHandler
	rec *recorder
}

func (g *fakeGuest) Serve(_ context.Context, s *connect.BidiStream[ownsandv1.ServeRequest, ownsandv1.ServeResponse]) error {
	for {
		req, err := s.Receive()
		if err != nil {
			return nil
		}
		exec := req.GetFrame().GetExecRequest()
		if exec == nil {
			continue
		}
		g.rec.add("exec " + strings.Join(exec.GetCommand(), " "))
		if err := s.Send(&ownsandv1.ServeResponse{Frame: datapath.MustNewFrame(req.GetFrame().GetStreamId(), exec.GetSandboxId(),
			&ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Exited{Exited: &ownsandv1.ExecExited{}}})}); err != nil {
			return nil
		}
	}
}

// vsockListener does the host side of the Firecracker vsock handshake
// before it hands a connection to the guest server.
type vsockListener struct{ net.Listener }

func (l vsockListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		// Read the CONNECT line byte by byte so no stream byte is lost.
		var b [1]byte
		ok := true
		for {
			if _, err := c.Read(b[:]); err != nil {
				ok = false
				break
			}
			if b[0] == '\n' {
				break
			}
		}
		if !ok {
			c.Close()
			continue
		}
		if _, err := c.Write([]byte("OK 1\n")); err != nil {
			c.Close()
			continue
		}
		return c, nil
	}
}

// startFakeVM serves a fake guest helper on the vsock socket and a fake
// Firecracker API on the API socket of a registered VM.
func startFakeVM(t *testing.T, r *Runtime, id string, rec *recorder) *vm {
	t.Helper()
	dir := t.TempDir()
	f, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	v := &vm{id: id, dir: dir, dirFile: f, exited: make(chan struct{})}

	gl, err := net.Listen("unix", v.path(vsockSocket))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle(ownsandv1connect.NewGuestServiceHandler(&fakeGuest{rec: rec}))
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	guest := &http.Server{Handler: mux, Protocols: &protocols}
	go guest.Serve(vsockListener{gl})
	t.Cleanup(func() { guest.Close() })

	al, err := net.Listen("unix", v.path(apiSocket))
	if err != nil {
		t.Fatal(err)
	}
	api := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		rec.add("api " + req.Method + " " + req.URL.Path + " " + string(body))
		if rec.rejectsResume() && strings.Contains(string(body), `"Resumed"`) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"fault_message":"resume rejected"}`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})}
	go api.Serve(al)
	t.Cleanup(func() { api.Close() })
	v.api = newAPIClient(v.path(apiSocket))

	r.mu.Lock()
	r.vms[id] = v
	r.mu.Unlock()
	return v
}

// Pause flushes the guest page cache before it freezes the vCPUs, so a
// disk-only snapshot of the paused sandbox holds the writes completed
// before the pause; Snapshot of the paused VM does not resume it.
func TestPauseFlushesGuestBeforeFreezing(t *testing.T) {
	r, err := New(Options{Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	v := startFakeVM(t, r, "s1", rec)
	ctx := context.Background()
	if err := r.Pause(ctx, v); err != nil {
		t.Fatal(err)
	}
	want := []string{"exec sync", `api PATCH /vm {"state":"Paused"}`}
	if got := rec.list(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("pause events %q, want %q", got, want)
	}

	// A disk-only (OUTBOUND) snapshot of the paused VM copies the disk
	// without another pause or a resume.
	v.network = ownsandv1.NetworkMode_NETWORK_MODE_OUTBOUND
	if err := os.WriteFile(filepath.Join(v.dir, diskFile), []byte("disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	snap := filepath.Join(t.TempDir(), "snap")
	if _, err := r.Snapshot(ctx, v, snap); err != nil {
		t.Fatal(err)
	}
	if got := rec.list(); len(got) != len(want) {
		t.Fatalf("snapshot of a paused VM changed its state: %q", got)
	}
	if b, err := os.ReadFile(filepath.Join(snap, snapDisk)); err != nil || string(b) != "disk" {
		t.Fatalf("snapshot disk %q %v", b, err)
	}
}

// A snapshot of a running VM flushes, pauses, copies and resumes it.
func TestSnapshotOfRunningVMFlushesOnce(t *testing.T) {
	r, err := New(Options{Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	v := startFakeVM(t, r, "s2", rec)
	v.network = ownsandv1.NetworkMode_NETWORK_MODE_OUTBOUND
	if err := os.WriteFile(filepath.Join(v.dir, diskFile), []byte("disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Snapshot(context.Background(), v, filepath.Join(t.TempDir(), "snap")); err != nil {
		t.Fatal(err)
	}
	want := []string{"exec sync", `api PATCH /vm {"state":"Paused"}`, `api PATCH /vm {"state":"Resumed"}`}
	if got := rec.list(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("snapshot events %q, want %q", got, want)
	}
}

// A resume that Firecracker rejects after a completed capture fails the
// snapshot, so no READY record is saved for a VM that stays paused.
func TestSnapshotReportsResumeFailure(t *testing.T) {
	r, err := New(Options{Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	rec := &recorder{rejectResume: true}
	v := startFakeVM(t, r, "s3", rec)
	v.network = ownsandv1.NetworkMode_NETWORK_MODE_OUTBOUND
	if err := os.WriteFile(filepath.Join(v.dir, diskFile), []byte("disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := r.Snapshot(context.Background(), v, filepath.Join(t.TempDir(), "snap"))
	if err == nil || !strings.Contains(err.Error(), "resume after snapshot") {
		t.Fatalf("snapshot error %v, want a resume error", err)
	}
	if info.SizeBytes != 0 || info.HasMemoryState {
		t.Fatalf("snapshot info %+v, want no capture", info)
	}
	if info.SourceState != ownsandv1.SandboxState_SANDBOX_STATE_PAUSED {
		t.Fatalf("source state %v, want PAUSED", info.SourceState)
	}
}

// A capture error stays the result when the resume that follows fails too,
// and the runtime still attempts the resume.
func TestSnapshotCaptureErrorWinsOverResumeFailure(t *testing.T) {
	r, err := New(Options{Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	rec := &recorder{rejectResume: true}
	v := startFakeVM(t, r, "s4", rec)
	v.network = ownsandv1.NetworkMode_NETWORK_MODE_OUTBOUND
	// The source disk is missing, so the copy fails.
	info, err := r.Snapshot(context.Background(), v, filepath.Join(t.TempDir(), "snap"))
	if err == nil || strings.Contains(err.Error(), "resume after snapshot") {
		t.Fatalf("snapshot error %v, want the disk error", err)
	}
	if info.SourceState != ownsandv1.SandboxState_SANDBOX_STATE_PAUSED {
		t.Fatalf("source state %v, want PAUSED with the capture error", info.SourceState)
	}
	if got := strings.Join(rec.list(), "\n"); !strings.Contains(got, `"state":"Resumed"`) {
		t.Fatalf("no resume attempted: %q", got)
	}
}

// After a rejected resume of a snapshot, the VM is paused for good until
// Firecracker accepts a resume: the runtime still tracks it as paused, so the
// explicit resume that follows reaches the Firecracker API and continues it.
func TestExplicitResumeAfterSnapshotResumeFailure(t *testing.T) {
	r, err := New(Options{Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	rec := &recorder{rejectResume: true}
	v := startFakeVM(t, r, "s5", rec)
	v.network = ownsandv1.NetworkMode_NETWORK_MODE_OUTBOUND
	if err := os.WriteFile(filepath.Join(v.dir, diskFile), []byte("disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := r.Snapshot(context.Background(), v, filepath.Join(t.TempDir(), "snap"))
	if err == nil || info.SourceState != ownsandv1.SandboxState_SANDBOX_STATE_PAUSED {
		t.Fatalf("snapshot info %+v error %v, want a PAUSED outcome with an error", info, err)
	}
	v.mu.Lock()
	paused := v.paused
	v.mu.Unlock()
	if !paused {
		t.Fatal("the runtime tracks the VM as running after the rejected resume")
	}
	resumes := func() int {
		n := 0
		for _, e := range rec.list() {
			if strings.Contains(e, `"state":"Resumed"`) {
				n++
			}
		}
		return n
	}
	before := resumes()
	rec.setRejectResume(false)
	if err := r.Resume(context.Background(), v); err != nil {
		t.Fatalf("explicit resume: %v", err)
	}
	if got := resumes(); got != before+1 {
		t.Fatalf("%d resume API calls, want %d", got, before+1)
	}
	v.mu.Lock()
	paused = v.paused
	v.mu.Unlock()
	if paused {
		t.Fatal("the runtime tracks the VM as paused after the explicit resume")
	}
}
