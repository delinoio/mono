package guest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/delinoio/mono/packages/ownsand-sdk-go/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// fakeClock is a settable clock.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// harness runs a Server over net.Pipe and plays the agent side.
type harness struct {
	t      *testing.T
	srv    *Server
	clock  *fakeClock
	conn   net.Conn
	fw     *datapath.FrameWriter
	wmu    sync.Mutex
	mu     sync.Mutex
	subs   map[uint64]chan *ownsandv1.DataFrame
	ctrl   chan *ownsandv1.GuestControl
	credit bool // return credit for every payload frame
	next   uint64
	clocks []time.Time
}

func newHarness(t *testing.T, cfg Config) *harness {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("guest helper tests use POSIX commands")
	}
	clock := &fakeClock{now: time.Now()}
	h := &harness{t: t, clock: clock, subs: map[uint64]chan *ownsandv1.DataFrame{}, ctrl: make(chan *ownsandv1.GuestControl, 4), credit: true}
	if cfg.StateDir == "" {
		cfg.StateDir = t.TempDir()
	}
	cfg.Now = clock.Now
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	cfg.SetClock = func(tm time.Time) error {
		h.mu.Lock()
		h.clocks = append(h.clocks, tm)
		h.mu.Unlock()
		return nil
	}
	h.srv = NewServer(cfg)
	server, client := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = h.srv.ServeConn(ctx, server)
	}()
	t.Cleanup(func() {
		cancel()
		_ = client.Close()
		<-done
	})
	h.conn = client
	h.fw = datapath.NewFrameWriter(client)
	go h.readLoop()
	return h
}

func (h *harness) readLoop() {
	fr := datapath.NewFrameReader(h.conn)
	for {
		f := &ownsandv1.GuestFrame{}
		if err := fr.Read(f); err != nil {
			return
		}
		switch p := f.GetFrame().(type) {
		case *ownsandv1.GuestFrame_Control:
			h.ctrl <- p.Control
		case *ownsandv1.GuestFrame_Data:
			d := p.Data
			h.mu.Lock()
			ch := h.subs[d.GetStreamId()]
			credit := h.credit
			h.mu.Unlock()
			if ch != nil {
				ch <- d
			}
			if credit && datapath.PayloadUsesCredit(d) {
				h.windowUpdate(d.GetStreamId(), datapath.FrameSize(d))
			}
		}
	}
}

func (h *harness) send(f *ownsandv1.GuestFrame) {
	h.wmu.Lock()
	defer h.wmu.Unlock()
	// Window updates race with the end of a test; a closed pipe is fine.
	if err := h.fw.Write(f); err != nil && !errors.Is(err, io.ErrClosedPipe) {
		h.t.Errorf("write frame: %v", err)
	}
}

func (h *harness) windowUpdate(id uint64, n int64) {
	go h.send(dataFrame(id, func(f *ownsandv1.DataFrame) {
		f.Payload = &ownsandv1.DataFrame_RelayWindowUpdate{RelayWindowUpdate: &ownsandv1.RelayWindowUpdate{AddedBytes: n}}
	}))
}

func dataFrame(id uint64, set func(*ownsandv1.DataFrame)) *ownsandv1.GuestFrame {
	return &ownsandv1.GuestFrame{Frame: &ownsandv1.GuestFrame_Data{Data: datapath.NewFrame(id, "sandbox", set)}}
}

// open sends a request frame on a new stream and returns its frames.
func (h *harness) open(set func(*ownsandv1.DataFrame)) (uint64, chan *ownsandv1.DataFrame) {
	h.mu.Lock()
	h.next++
	id := h.next
	ch := make(chan *ownsandv1.DataFrame, 1024)
	h.subs[id] = ch
	h.mu.Unlock()
	h.send(dataFrame(id, set))
	return id, ch
}

func (h *harness) recv(ch chan *ownsandv1.DataFrame) *ownsandv1.DataFrame {
	h.t.Helper()
	select {
	case f := <-ch:
		return f
	case <-time.After(10 * time.Second):
		h.t.Fatal("timed out waiting for a guest frame")
		return nil
	}
}

// execEvents collects exec events until the final event.
func (h *harness) execEvents(ch chan *ownsandv1.DataFrame) ([]*ownsandv1.ExecEvent, *ownsandv1.FrameError) {
	h.t.Helper()
	var evs []*ownsandv1.ExecEvent
	for {
		f := h.recv(ch)
		if e := f.GetError(); e != nil {
			return evs, e
		}
		ev := f.GetExecEvent()
		if ev == nil {
			h.t.Fatalf("unexpected frame kind %v", f.GetKind())
		}
		if ev.GetKeepalive() != nil {
			continue
		}
		evs = append(evs, ev)
		if ev.GetExited() != nil || ev.GetFailed() != nil {
			return evs, nil
		}
	}
}

func output(evs []*ownsandv1.ExecEvent, stream ownsandv1.OutputStream) string {
	var b strings.Builder
	for _, ev := range evs {
		if o := ev.GetOutput(); o != nil && o.GetStream() == stream {
			b.Write(o.GetData())
		}
	}
	return b.String()
}

func execReq(cmd ...string) *ownsandv1.ExecRequest {
	return &ownsandv1.ExecRequest{
		SandboxId:   "sandbox",
		ExecId:      datapath.NewID(),
		Command:     cmd,
		Env:         map[string]string{"OWNSAND_TEST": "value"},
		BindingSalt: datapath.NewBindingSalt(),
	}
}

func (h *harness) exec(req *ownsandv1.ExecRequest) ([]*ownsandv1.ExecEvent, *ownsandv1.FrameError) {
	_, ch := h.open(func(f *ownsandv1.DataFrame) { f.Payload = &ownsandv1.DataFrame_ExecRequest{ExecRequest: req} })
	return h.execEvents(ch)
}

func TestReadinessAndClock(t *testing.T) {
	h := newHarness(t, Config{})
	h.send(&ownsandv1.GuestFrame{Frame: &ownsandv1.GuestFrame_Control{Control: &ownsandv1.GuestControl{
		Message: &ownsandv1.GuestControl_ReadyRequest{ReadyRequest: &ownsandv1.GuestReadyRequest{}},
	}}})
	select {
	case c := <-h.ctrl:
		if c.GetReadyResponse().GetGuestVersion() != Version {
			t.Fatalf("unexpected ready response %v", c)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no ready response")
	}
	want := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	h.send(&ownsandv1.GuestFrame{Frame: &ownsandv1.GuestFrame_Control{Control: &ownsandv1.GuestControl{
		Message: &ownsandv1.GuestControl_SetClock{SetClock: &ownsandv1.SetClockRequest{Now: timestamppb.New(want)}},
	}}})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		n := len(h.clocks)
		h.mu.Unlock()
		if n == 1 {
			h.mu.Lock()
			got := h.clocks[0]
			h.mu.Unlock()
			if !got.Equal(want) {
				t.Fatalf("clock set to %v, want %v", got, want)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("set clock was not applied")
}

func TestExecOutputAndExitCode(t *testing.T) {
	h := newHarness(t, Config{})
	evs, ferr := h.exec(execReq("/bin/sh", "-c", `printf out; printf err >&2; printf "$OWNSAND_TEST"; exit 3`))
	if ferr != nil {
		t.Fatalf("error frame: %v", ferr)
	}
	if evs[0].GetStarted() == nil {
		t.Fatalf("first event is not started: %v", evs[0])
	}
	if got := output(evs, ownsandv1.OutputStream_OUTPUT_STREAM_STDOUT); got != "outvalue" {
		t.Fatalf("stdout = %q", got)
	}
	if got := output(evs, ownsandv1.OutputStream_OUTPUT_STREAM_STDERR); got != "err" {
		t.Fatalf("stderr = %q", got)
	}
	last := evs[len(evs)-1].GetExited()
	if last.GetExitCode() != 3 || last.GetTimedOut() {
		t.Fatalf("exited = %v", last)
	}
}

func TestExecStartFailure(t *testing.T) {
	h := newHarness(t, Config{})
	evs, ferr := h.exec(execReq("/nonexistent/program"))
	if ferr != nil {
		t.Fatalf("error frame: %v", ferr)
	}
	if evs[len(evs)-1].GetFailed() == nil {
		t.Fatalf("want failed event, got %v", evs[len(evs)-1])
	}
}

func TestExecTimeoutSendsSigtermThenSigkill(t *testing.T) {
	h := newHarness(t, Config{KillGrace: 300 * time.Millisecond})
	req := execReq("/bin/sh", "-c", "sleep 30")
	req.Timeout = durationpb.New(200 * time.Millisecond)
	evs, _ := h.exec(req)
	ex := evs[len(evs)-1].GetExited()
	if !ex.GetTimedOut() || ex.GetSignal() != 15 {
		t.Fatalf("want SIGTERM timeout, got %v", ex)
	}

	req = execReq("/bin/sh", "-c", `trap "" TERM; while :; do sleep 0.05; done`)
	req.Timeout = durationpb.New(200 * time.Millisecond)
	start := time.Now()
	evs, _ = h.exec(req)
	ex = evs[len(evs)-1].GetExited()
	if !ex.GetTimedOut() || ex.GetSignal() != 9 {
		t.Fatalf("want SIGKILL after grace, got %v", ex)
	}
	if time.Since(start) < 450*time.Millisecond {
		t.Fatalf("SIGKILL came before the grace period")
	}
}

func TestExecRunsOnceAndRejectsAnotherBinding(t *testing.T) {
	h := newHarness(t, Config{})
	marker := filepath.Join(t.TempDir(), "count")
	req := execReq("/bin/sh", "-c", "echo x >> "+marker+"; echo done")
	evs, _ := h.exec(req)
	if output(evs, ownsandv1.OutputStream_OUTPUT_STREAM_STDOUT) != "done\n" {
		t.Fatalf("unexpected output")
	}
	// The identical request attaches from offset 0 and does not run again.
	evs, _ = h.exec(req)
	if output(evs, ownsandv1.OutputStream_OUTPUT_STREAM_STDOUT) != "done\n" || evs[len(evs)-1].GetExited() == nil {
		t.Fatalf("duplicate request did not replay the exec")
	}
	data, _ := os.ReadFile(marker)
	if string(data) != "x\n" {
		t.Fatalf("command ran %d times", strings.Count(string(data), "x"))
	}
	other := execReq("/bin/true")
	other.ExecId = req.ExecId
	if _, ferr := h.exec(other); ferr == nil || ferr.GetCode() != "already_exists" {
		t.Fatalf("want already_exists, got %v", ferr)
	}
}

func TestAttachWithTruncationAndUnknownExec(t *testing.T) {
	h := newHarness(t, Config{OutputRetention: 1024})
	req := execReq("/bin/sh", "-c", "head -c 5000 /dev/zero")
	evs, _ := h.exec(req)
	if evs[len(evs)-1].GetExited() == nil {
		t.Fatalf("exec did not exit")
	}
	_, ch := h.open(func(f *ownsandv1.DataFrame) {
		f.Payload = &ownsandv1.DataFrame_ExecAttachRequest{ExecAttachRequest: &ownsandv1.AttachExecRequest{ExecId: req.ExecId}}
	})
	evs, ferr := h.execEvents(ch)
	if ferr != nil {
		t.Fatalf("attach error: %v", ferr)
	}
	if evs[0].GetStarted() == nil || evs[1].GetOutputTruncated() == nil {
		t.Fatalf("want started then output_truncated, got %v %v", evs[0], evs[1])
	}
	if tr := evs[1].GetOutputTruncated(); tr.GetFirstStdoutOffset() != 5000-1024 {
		t.Fatalf("first stdout offset = %d", tr.GetFirstStdoutOffset())
	}
	if got := len(output(evs, ownsandv1.OutputStream_OUTPUT_STREAM_STDOUT)); got != 1024 {
		t.Fatalf("attached output length = %d", got)
	}

	_, ch = h.open(func(f *ownsandv1.DataFrame) {
		f.Payload = &ownsandv1.DataFrame_ExecAttachRequest{ExecAttachRequest: &ownsandv1.AttachExecRequest{ExecId: datapath.NewID()}}
	})
	if _, ferr := h.execEvents(ch); ferr == nil || ferr.GetCode() != "not_found" {
		t.Fatalf("want not_found, got %v", ferr)
	}
}

func TestStreamCloseKeepsExecRunningAndResultExpires(t *testing.T) {
	h := newHarness(t, Config{})
	req := execReq("/bin/sh", "-c", "sleep 0.3; echo late")
	id, ch := h.open(func(f *ownsandv1.DataFrame) { f.Payload = &ownsandv1.DataFrame_ExecRequest{ExecRequest: req} })
	if h.recv(ch).GetExecEvent().GetStarted() == nil {
		t.Fatal("no started event")
	}
	h.send(dataFrame(id, func(f *ownsandv1.DataFrame) {
		f.Payload = &ownsandv1.DataFrame_RelayStreamClose{RelayStreamClose: &ownsandv1.RelayStreamClose{}}
	}))
	time.Sleep(600 * time.Millisecond)
	_, ch = h.open(func(f *ownsandv1.DataFrame) {
		f.Payload = &ownsandv1.DataFrame_ExecAttachRequest{ExecAttachRequest: &ownsandv1.AttachExecRequest{ExecId: req.ExecId}}
	})
	evs, _ := h.execEvents(ch)
	if output(evs, ownsandv1.OutputStream_OUTPUT_STREAM_STDOUT) != "late\n" || evs[len(evs)-1].GetExited() == nil {
		t.Fatalf("exec did not continue after stream close")
	}
	h.clock.Advance(ExecResultRetention + time.Second)
	h.srv.sweep()
	_, ch = h.open(func(f *ownsandv1.DataFrame) {
		f.Payload = &ownsandv1.DataFrame_ExecAttachRequest{ExecAttachRequest: &ownsandv1.AttachExecRequest{ExecId: req.ExecId}}
	})
	if _, ferr := h.execEvents(ch); ferr == nil || ferr.GetCode() != "not_found" {
		t.Fatalf("want not_found after retention, got %v", ferr)
	}
	// A retry of the expired exec never runs the command again.
	if _, ferr := h.exec(req); ferr == nil || ferr.GetCode() != "not_found" {
		t.Fatalf("want not_found for an expired exec_id, got %v", ferr)
	}
}

func TestRingKeepsLastBytes(t *testing.T) {
	r := ring{limit: 8}
	r.write([]byte("0123456789abc"))
	if r.first() != 5 || r.end != 13 {
		t.Fatalf("first=%d end=%d", r.first(), r.end)
	}
	if got := string(r.read(5, 100)); got != "56789abc" {
		t.Fatalf("read = %q", got)
	}
	if got := string(r.read(10, 2)); got != "ab" {
		t.Fatalf("read = %q", got)
	}
	if r.read(2, 4) != nil {
		t.Fatal("evicted bytes were returned")
	}
}

// readAll runs one ReadFile call and returns the data and the finished size.
func (h *harness) readFile(req *ownsandv1.ReadFileRequest) ([]byte, int64, *ownsandv1.FrameError) {
	h.t.Helper()
	_, ch := h.open(func(f *ownsandv1.DataFrame) { f.Payload = &ownsandv1.DataFrame_FileReadRequest{FileReadRequest: req} })
	var buf bytes.Buffer
	for {
		f := h.recv(ch)
		switch {
		case f.GetError() != nil:
			return buf.Bytes(), -1, f.GetError()
		case f.GetFileChunk() != nil:
			if f.GetFileChunk().GetOffset() != req.GetOffset()+int64(buf.Len()) {
				h.t.Fatalf("chunk offset %d out of order", f.GetFileChunk().GetOffset())
			}
			buf.Write(f.GetFileChunk().GetData())
		case f.GetFileReadFinished() != nil:
			return buf.Bytes(), f.GetFileReadFinished().GetSizeBytes(), nil
		default:
			h.t.Fatalf("unexpected frame %v", f.GetKind())
		}
	}
}

func TestReadSnapshotIsStableAndResumable(t *testing.T) {
	h := newHarness(t, Config{})
	path := filepath.Join(t.TempDir(), "file.bin")
	content := bytes.Repeat([]byte("abcdefgh"), 300_000) // 2.4 MB, several chunks
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	req := &ownsandv1.ReadFileRequest{SandboxId: "sandbox", ReadId: datapath.NewID(), Path: path, BindingSalt: datapath.NewBindingSalt()}
	got, size, ferr := h.readFile(req)
	if ferr != nil || !bytes.Equal(got, content) || size != int64(len(content)) {
		t.Fatalf("read mismatch: err=%v size=%d", ferr, size)
	}
	// The file changes; the same read_id still returns the snapshot.
	if err := os.WriteFile(path, []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	req.Offset = 1 << 20
	got, _, ferr = h.readFile(req)
	if ferr != nil || !bytes.Equal(got, content[1<<20:]) {
		t.Fatalf("resumed read mismatch: %v", ferr)
	}
	// Another binding for the same read_id is rejected.
	other := &ownsandv1.ReadFileRequest{SandboxId: "sandbox", ReadId: req.ReadId, Path: path, BindingSalt: datapath.NewBindingSalt()}
	if _, _, ferr := h.readFile(other); ferr == nil || ferr.GetCode() != "already_exists" {
		t.Fatalf("want already_exists, got %v", ferr)
	}
	// After 15 minutes without a call the snapshot is gone.
	h.clock.Advance(FileOperationRetention + time.Second)
	h.srv.sweep()
	req.Offset = 0
	if _, _, ferr := h.readFile(req); ferr == nil || ferr.GetCode() != "not_found" {
		t.Fatalf("want not_found after expiry, got %v", ferr)
	}
	entries, _ := os.ReadDir(filepath.Join(h.srv.cfg.StateDir, "reads"))
	if len(entries) != 0 {
		t.Fatalf("snapshot files left: %d", len(entries))
	}
}

func TestReadErrors(t *testing.T) {
	h := newHarness(t, Config{})
	missing := &ownsandv1.ReadFileRequest{ReadId: datapath.NewID(), Path: "/nonexistent/file", BindingSalt: datapath.NewBindingSalt()}
	if _, _, ferr := h.readFile(missing); ferr == nil || ferr.GetCode() != "not_found" {
		t.Fatalf("want not_found, got %v", ferr)
	}
	rel := &ownsandv1.ReadFileRequest{ReadId: datapath.NewID(), Path: "relative", BindingSalt: datapath.NewBindingSalt()}
	if _, _, ferr := h.readFile(rel); ferr == nil || ferr.GetCode() != "invalid_argument" {
		t.Fatalf("want invalid_argument, got %v", ferr)
	}
	dir := &ownsandv1.ReadFileRequest{ReadId: datapath.NewID(), Path: t.TempDir(), BindingSalt: datapath.NewBindingSalt()}
	if _, _, ferr := h.readFile(dir); ferr == nil || ferr.GetCode() != "failed_precondition" {
		t.Fatalf("want failed_precondition, got %v", ferr)
	}
}

func TestSameVersionDetectsChanges(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	_ = os.WriteFile(p, []byte("a"), 0o644)
	a, _ := os.Stat(p)
	_ = os.WriteFile(p, []byte("ab"), 0o644)
	b, _ := os.Stat(p)
	if sameVersion(a, b) {
		t.Fatal("size change not detected")
	}
	_ = os.Chtimes(p, time.Now(), time.Now().Add(time.Hour))
	c, _ := os.Stat(p)
	if sameVersion(b, c) {
		t.Fatal("modification time change not detected")
	}
}

func TestCreditWindowStallsUntilWindowUpdate(t *testing.T) {
	h := newHarness(t, Config{})
	h.credit = false
	path := filepath.Join(t.TempDir(), "big")
	if err := os.WriteFile(path, make([]byte, 6<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	req := &ownsandv1.ReadFileRequest{ReadId: datapath.NewID(), Path: path, BindingSalt: datapath.NewBindingSalt()}
	id, ch := h.open(func(f *ownsandv1.DataFrame) { f.Payload = &ownsandv1.DataFrame_FileReadRequest{FileReadRequest: req} })
	var received int64
	for received+datapath.FrameSize(&ownsandv1.DataFrame{}) < InitialWindow-datapath.MaxFrameSize {
		received += datapath.FrameSize(h.recv(ch))
	}
	select {
	case f := <-ch:
		received += datapath.FrameSize(f)
	case <-time.After(300 * time.Millisecond):
	}
	if received > InitialWindow {
		t.Fatalf("received %d bytes beyond the window", received)
	}
	select {
	case <-ch:
		t.Fatal("frame sent without credit")
	case <-time.After(300 * time.Millisecond):
	}
	h.windowUpdate(id, 8<<20)
	for {
		f := h.recv(ch)
		if f.GetFileReadFinished() != nil {
			return
		}
	}
}

func (h *harness) write(req *ownsandv1.WriteFileRequest) (*ownsandv1.WriteFileResponse, *ownsandv1.FrameError) {
	h.t.Helper()
	_, ch := h.open(func(f *ownsandv1.DataFrame) { f.Payload = &ownsandv1.DataFrame_FileWriteChunk{FileWriteChunk: req} })
	f := h.recv(ch)
	return f.GetFileWriteResult(), f.GetError()
}

func TestWriteRangesFinishAndIdempotency(t *testing.T) {
	h := newHarness(t, Config{})
	dir := t.TempDir()
	target := filepath.Join(dir, "out.txt")
	salt := datapath.NewBindingSalt()
	id := datapath.NewID()
	chunk := func(off int64, data string) *ownsandv1.FileChunk {
		return &ownsandv1.FileChunk{Offset: off, Data: []byte(data)}
	}
	// The second half arrives first, with the path and salt of the first call.
	res, ferr := h.write(&ownsandv1.WriteFileRequest{WriteId: id, Path: target, BindingSalt: salt, Chunk: chunk(5, "world")})
	if ferr != nil || res.GetWrittenBytes() != 0 {
		t.Fatalf("first chunk: %v %v", res, ferr)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("target exists before finish")
	}
	// Finishing before the ranges cover the file fails.
	if _, ferr := h.write(&ownsandv1.WriteFileRequest{WriteId: id, Finish: &ownsandv1.WriteFileFinish{SizeBytes: 10, Mode: 0o600}}); ferr == nil || ferr.GetCode() != "failed_precondition" {
		t.Fatalf("want failed_precondition for incomplete ranges, got %v", ferr)
	}
	res, ferr = h.write(&ownsandv1.WriteFileRequest{WriteId: id, Chunk: chunk(0, "hello")})
	if ferr != nil || res.GetWrittenBytes() != 10 {
		t.Fatalf("second chunk: %v %v", res, ferr)
	}
	// Writing the same bytes again is harmless; different bytes fail.
	if _, ferr := h.write(&ownsandv1.WriteFileRequest{WriteId: id, Chunk: chunk(3, "low")}); ferr != nil {
		t.Fatalf("identical overlap failed: %v", ferr)
	}
	if _, ferr := h.write(&ownsandv1.WriteFileRequest{WriteId: id, Chunk: chunk(3, "LOW")}); ferr == nil || ferr.GetCode() != "failed_precondition" {
		t.Fatalf("want failed_precondition for a conflicting overlap, got %v", ferr)
	}
	fin := &ownsandv1.WriteFileRequest{WriteId: id, Chunk: chunk(8, "ld"), Finish: &ownsandv1.WriteFileFinish{SizeBytes: 10, Mode: 0o600}}
	res, ferr = h.write(fin)
	if ferr != nil || !res.GetFinished() || res.GetWrittenBytes() != 10 {
		t.Fatalf("finish: %v %v", res, ferr)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "helloworld" {
		t.Fatalf("target content %q, %v", data, err)
	}
	if fi, _ := os.Stat(target); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", fi.Mode())
	}
	// A repeated finish returns the same result without writing again.
	_ = os.WriteFile(target, []byte("edited"), 0o600)
	res, ferr = h.write(fin)
	if ferr != nil || !res.GetFinished() {
		t.Fatalf("repeated finish: %v %v", res, ferr)
	}
	if data, _ := os.ReadFile(target); string(data) != "edited" {
		t.Fatal("repeated finish wrote the target again")
	}
	other := &ownsandv1.WriteFileRequest{WriteId: id, Finish: &ownsandv1.WriteFileFinish{SizeBytes: 10, Mode: 0o644}}
	if _, ferr := h.write(other); ferr == nil || ferr.GetCode() != "failed_precondition" {
		t.Fatalf("want failed_precondition for another finish, got %v", ferr)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("temporary files left: %d entries", len(entries))
	}
}

func TestWriteExpiryAndUnknownWrite(t *testing.T) {
	h := newHarness(t, Config{})
	dir := t.TempDir()
	id := datapath.NewID()
	if _, ferr := h.write(&ownsandv1.WriteFileRequest{WriteId: id, Chunk: &ownsandv1.FileChunk{Data: []byte("x")}}); ferr == nil || ferr.GetCode() != "not_found" {
		t.Fatalf("want not_found for a continuation of an unknown write, got %v", ferr)
	}
	req := &ownsandv1.WriteFileRequest{WriteId: id, Path: filepath.Join(dir, "f"), BindingSalt: datapath.NewBindingSalt(), Chunk: &ownsandv1.FileChunk{Data: []byte("x")}}
	if _, ferr := h.write(req); ferr != nil {
		t.Fatalf("first chunk: %v", ferr)
	}
	h.clock.Advance(FileOperationRetention + time.Second)
	h.srv.sweep()
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("temporary file not deleted: %d entries", len(entries))
	}
	if _, ferr := h.write(req); ferr == nil || ferr.GetCode() != "not_found" {
		t.Fatalf("want not_found after expiry, got %v", ferr)
	}
}

func TestDuplicateStreamIDIsRejected(t *testing.T) {
	h := newHarness(t, Config{})
	req := execReq("/bin/sh", "-c", "sleep 1")
	id, ch := h.open(func(f *ownsandv1.DataFrame) { f.Payload = &ownsandv1.DataFrame_ExecRequest{ExecRequest: req} })
	if h.recv(ch).GetExecEvent().GetStarted() == nil {
		t.Fatal("no started event")
	}
	h.send(dataFrame(id, func(f *ownsandv1.DataFrame) {
		f.Payload = &ownsandv1.DataFrame_ExecRequest{ExecRequest: execReq("/bin/true")}
	}))
	for {
		f := h.recv(ch)
		if f.GetError() != nil {
			if f.GetError().GetCode() != "invalid_argument" {
				t.Fatalf("unexpected code %s", f.GetError().GetCode())
			}
			return
		}
	}
}
