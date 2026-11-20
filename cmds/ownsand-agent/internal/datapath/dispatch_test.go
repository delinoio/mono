package datapath

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	goruntime "runtime"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/runtime/runtimetest"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/sandbox"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/state"
	sdkdp "github.com/delinoio/mono/packages/ownsand-sdk-go/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

type testSandbox struct {
	mgr *sandbox.Manager
	rt  *runtimetest.Runtime
	id  string
}

func newSandbox(t *testing.T) *testSandbox {
	t.Helper()
	if goruntime.GOOS == "windows" {
		t.Skip("uses POSIX commands in the guest helper")
	}
	dir, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rt := runtimetest.New(t.TempDir())
	mgr, err := sandbox.New(context.Background(), sandbox.Config{Runtime: rt, Dir: dir, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	mgr.SetLimits(&ownsandv1.DeviceLimits{VcpuCount: 8, MemoryMib: 8192, DiskGib: 100})
	id := sdkdp.NewID()
	done := make(chan *ownsandv1.AgentCommandResult, 1)
	now := time.Now()
	mgr.Handle(&ownsandv1.AgentCommand{CommandId: "c", Command: &ownsandv1.AgentCommand_CreateSandbox{CreateSandbox: &ownsandv1.CreateSandboxCommand{
		SandboxId: id, Source: &ownsandv1.SandboxSource{Source: &ownsandv1.SandboxSource_BaseImage{BaseImage: ownsandv1.BaseImage_BASE_IMAGE_LINUX}},
		Resources:    &ownsandv1.Resources{VcpuCount: 1, MemoryMib: 512, DiskGib: 1},
		IdleDeadline: timestamppb.New(now.Add(time.Hour)), MaxDeadline: timestamppb.New(now.Add(2 * time.Hour)), IdleTtl: durationpb.New(time.Hour),
	}}}, func(r *ownsandv1.AgentCommandResult) { done <- r })
	if r := <-done; r.GetErrorCode() != "" {
		t.Fatalf("create sandbox: %v", r)
	}
	t.Cleanup(func() { _ = rt.Close() })
	return &testSandbox{mgr: mgr, rt: rt, id: id}
}

func (s *testSandbox) command(t *testing.T, cmd *ownsandv1.AgentCommand) {
	done := make(chan *ownsandv1.AgentCommandResult, 1)
	s.mgr.Handle(cmd, func(r *ownsandv1.AgentCommandResult) { done <- r })
	if r := <-done; r.GetErrorCode() != "" {
		t.Fatalf("command: %v", r)
	}
}

// memSink collects the frames of one stream.
type memSink struct {
	ctx    context.Context
	cancel context.CancelFunc
	frames chan *ownsandv1.DataFrame
}

func newSink() *memSink {
	ctx, cancel := context.WithCancel(context.Background())
	return &memSink{ctx: ctx, cancel: cancel, frames: make(chan *ownsandv1.DataFrame, 4096)}
}

func (m *memSink) Context() context.Context { return m.ctx }
func (m *memSink) Payload(set func(*ownsandv1.DataFrame)) error {
	if m.ctx.Err() != nil {
		return m.ctx.Err()
	}
	m.frames <- sdkdp.NewFrame(1, "", set)
	return nil
}
func (m *memSink) Error(code connect.Code, msg string) error {
	m.frames <- errorFrame(1, "", code, msg)
	return nil
}

func (m *memSink) all() []*ownsandv1.DataFrame {
	var out []*ownsandv1.DataFrame
	for {
		select {
		case f := <-m.frames:
			out = append(out, f)
		default:
			return out
		}
	}
}

func execFrame(sid string, req *ownsandv1.ExecRequest) *ownsandv1.DataFrame {
	req.SandboxId = sid
	return sdkdp.NewFrame(1, sid, func(f *ownsandv1.DataFrame) { f.Payload = &ownsandv1.DataFrame_ExecRequest{ExecRequest: req} })
}

func newExec(cmd ...string) *ownsandv1.ExecRequest {
	return &ownsandv1.ExecRequest{ExecId: sdkdp.NewID(), Command: cmd, BindingSalt: sdkdp.NewBindingSalt()}
}

func stdout(frames []*ownsandv1.DataFrame) string {
	var b bytes.Buffer
	for _, f := range frames {
		if o := f.GetExecEvent().GetOutput(); o != nil && o.GetStream() == ownsandv1.OutputStream_OUTPUT_STREAM_STDOUT {
			b.Write(o.GetData())
		}
	}
	return b.String()
}

func TestDispatchExecAndRetryAttaches(t *testing.T) {
	sb := newSandbox(t)
	d := NewDispatcher(context.Background(), sb.mgr, quiet, nil)
	req := newExec("/bin/sh", "-c", "echo hi; echo err >&2")
	sink := newSink()
	o := d.Serve(sb.id, execFrame(sb.id, req), sink)
	frames := sink.all()
	if !o.Finished || stdout(frames) != "hi\n" || frames[len(frames)-1].GetExecEvent().GetExited() == nil {
		t.Fatalf("outcome %+v, frames %v", o, frames)
	}
	// The identical request attaches and does not run again.
	sink = newSink()
	if o := d.Serve(sb.id, execFrame(sb.id, req), sink); !o.Finished || stdout(sink.all()) != "hi\n" {
		t.Fatal("retry did not replay the exec")
	}
	other := newExec("/bin/true")
	other.ExecId = req.ExecId
	sink = newSink()
	if o := d.Serve(sb.id, execFrame(sb.id, other), sink); o.Code != connect.CodeAlreadyExists {
		t.Fatalf("want already_exists, got %v", o.Code)
	}
}

func TestDispatchOperationIDRulesAndSandboxBinding(t *testing.T) {
	sb := newSandbox(t)
	d := NewDispatcher(context.Background(), sb.mgr, quiet, nil)
	old := newExec("/bin/true")
	old.ExecId = sdkdp.NewUUIDv7At(time.Now().Add(-16 * time.Minute)).String()
	if o := d.Serve(sb.id, execFrame(sb.id, old), newSink()); o.Code != connect.CodeNotFound {
		t.Fatalf("expired ID: %v", o.Code)
	}
	future := newExec("/bin/true")
	future.ExecId = sdkdp.NewUUIDv7At(time.Now().Add(6 * time.Minute)).String()
	if o := d.Serve(sb.id, execFrame(sb.id, future), newSink()); o.Code != connect.CodeFailedPrecondition {
		t.Fatalf("future ID: %v", o.Code)
	}
	if o := d.Serve("other-sandbox", execFrame(sb.id, newExec("/bin/true")), newSink()); o.Code != connect.CodePermissionDenied {
		t.Fatalf("other sandbox: %v", o.Code)
	}
	big := newExec("/bin/true")
	big.Env = map[string]string{"X": string(make([]byte, 300<<10))}
	if o := d.Serve(sb.id, execFrame(sb.id, big), newSink()); o.Code != connect.CodeInvalidArgument {
		t.Fatalf("oversized exec: %v", o.Code)
	}
}

func TestPausedSandboxAttachReturnsKeptOutputThenPaused(t *testing.T) {
	sb := newSandbox(t)
	d := NewDispatcher(context.Background(), sb.mgr, quiet, nil)
	req := newExec("/bin/sh", "-c", "echo before; sleep 30")
	sink := newSink()
	go d.Serve(sb.id, execFrame(sb.id, req), sink)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if f := <-sink.frames; f.GetExecEvent().GetOutput() != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no output")
		}
	}
	sink.cancel()
	sb.command(t, &ownsandv1.AgentCommand{CommandId: "p", Command: &ownsandv1.AgentCommand_PauseSandbox{PauseSandbox: &ownsandv1.PauseSandboxCommand{SandboxId: sb.id}}})
	// A new Exec on a paused sandbox fails; AttachExec returns the kept output.
	if o := d.Serve(sb.id, execFrame(sb.id, newExec("/bin/true")), newSink()); o.Code != connect.CodeFailedPrecondition {
		t.Fatalf("exec on a paused sandbox: %v", o.Code)
	}
	sink = newSink()
	o := d.Serve(sb.id, sdkdp.NewFrame(1, sb.id, func(f *ownsandv1.DataFrame) {
		f.Payload = &ownsandv1.DataFrame_ExecAttachRequest{ExecAttachRequest: &ownsandv1.AttachExecRequest{SandboxId: sb.id, ExecId: req.ExecId}}
	}), sink)
	frames := sink.all()
	if o.Finished || stdout(frames) != "before\n" || frames[len(frames)-1].GetExecEvent().GetPaused() == nil {
		t.Fatalf("paused attach: %+v %v", o, frames)
	}
}

func TestDispatchReadAndWrite(t *testing.T) {
	sb := newSandbox(t)
	d := NewDispatcher(context.Background(), sb.mgr, quiet, nil)
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	wid := sdkdp.NewID()
	w := &ownsandv1.WriteFileRequest{SandboxId: sb.id, WriteId: wid, Path: path, BindingSalt: sdkdp.NewBindingSalt(),
		Chunk: &ownsandv1.FileChunk{Data: []byte("hello")}, Finish: &ownsandv1.WriteFileFinish{SizeBytes: 5, Mode: 0o600}}
	sink := newSink()
	o := d.Serve(sb.id, sdkdp.NewFrame(1, sb.id, func(f *ownsandv1.DataFrame) { f.Payload = &ownsandv1.DataFrame_FileWriteChunk{FileWriteChunk: w} }), sink)
	if !o.Finished {
		t.Fatalf("write: %+v %v", o, sink.all())
	}
	if data, _ := os.ReadFile(path); string(data) != "hello" {
		t.Fatalf("written %q", data)
	}
	r := &ownsandv1.ReadFileRequest{SandboxId: sb.id, ReadId: sdkdp.NewID(), Path: path, BindingSalt: sdkdp.NewBindingSalt()}
	sink = newSink()
	o = d.Serve(sb.id, sdkdp.NewFrame(1, sb.id, func(f *ownsandv1.DataFrame) { f.Payload = &ownsandv1.DataFrame_FileReadRequest{FileReadRequest: r} }), sink)
	frames := sink.all()
	if !o.Finished || string(frames[0].GetFileChunk().GetData()) != "hello" || frames[1].GetFileReadFinished().GetSizeBytes() != 5 {
		t.Fatalf("read: %+v %v", o, frames)
	}
	// A continuation of an unknown write fails with not_found.
	cont := &ownsandv1.WriteFileRequest{SandboxId: sb.id, WriteId: sdkdp.NewID(), Chunk: &ownsandv1.FileChunk{Data: []byte("x")}}
	if o := d.Serve(sb.id, sdkdp.NewFrame(1, sb.id, func(f *ownsandv1.DataFrame) { f.Payload = &ownsandv1.DataFrame_FileWriteChunk{FileWriteChunk: cont} }), newSink()); o.Code != connect.CodeNotFound {
		t.Fatalf("unknown write: %v", o.Code)
	}
}

func TestCheckGrant(t *testing.T) {
	sb := newSandbox(t)
	d := NewDispatcher(context.Background(), sb.mgr, quiet, nil)
	req := newExec("/bin/true")
	req.SandboxId = sb.id
	f := execFrame(sb.id, req)
	g := GrantTarget{SandboxID: sb.id, Kind: ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC, OpID: req.ExecId, Digest: sdkdp.ExecDigest(req)}
	if err := d.CheckGrant(g, f); err != nil {
		t.Fatalf("matching grant: %v", err)
	}
	bad := g
	bad.Digest = make([]byte, 32)
	if connect.CodeOf(d.CheckGrant(bad, f)) != connect.CodePermissionDenied {
		t.Fatal("digest mismatch accepted")
	}
	bad = g
	bad.OpID = sdkdp.NewID()
	if connect.CodeOf(d.CheckGrant(bad, f)) != connect.CodePermissionDenied {
		t.Fatal("operation ID mismatch accepted")
	}
	attach := sdkdp.NewFrame(1, sb.id, func(f *ownsandv1.DataFrame) {
		f.Payload = &ownsandv1.DataFrame_ExecAttachRequest{ExecAttachRequest: &ownsandv1.AttachExecRequest{SandboxId: sb.id, ExecId: req.ExecId}}
	})
	// An AttachExec under the EXEC grant needs the recorded exec.
	if connect.CodeOf(d.CheckGrant(g, attach)) != connect.CodeNotFound {
		t.Fatal("attach to an unknown exec under an EXEC grant must be not_found")
	}
	d.Serve(sb.id, f, newSink())
	if err := d.CheckGrant(g, attach); err != nil {
		t.Fatalf("attach under the EXEC grant: %v", err)
	}
	ag := GrantTarget{SandboxID: sb.id, Kind: ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_ATTACH_EXEC, OpID: req.ExecId}
	if err := d.CheckGrant(ag, attach); err != nil {
		t.Fatalf("attach under an ATTACH_EXEC grant: %v", err)
	}
	if connect.CodeOf(d.CheckGrant(ag, f)) != connect.CodePermissionDenied {
		t.Fatal("an ATTACH_EXEC grant accepted an ExecRequest")
	}
	w := &ownsandv1.WriteFileRequest{SandboxId: sb.id, WriteId: sdkdp.NewID(), Path: "/tmp/x", BindingSalt: sdkdp.NewBindingSalt()}
	wf := sdkdp.NewFrame(1, sb.id, func(f *ownsandv1.DataFrame) { f.Payload = &ownsandv1.DataFrame_FileWriteChunk{FileWriteChunk: w} })
	wg := GrantTarget{SandboxID: sb.id, Kind: ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_WRITE_FILE, OpID: w.WriteId, Digest: sdkdp.WriteDigest(w)}
	if err := d.CheckGrant(wg, wf); err != nil {
		t.Fatalf("write grant: %v", err)
	}
	if connect.CodeOf(d.CheckGrant(GrantTarget{SandboxID: sb.id, Kind: ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_READ_FILE, OpID: w.WriteId, Digest: wg.Digest}, wf)) != connect.CodePermissionDenied {
		t.Fatal("a READ_FILE grant accepted a write")
	}
}

func TestRing(t *testing.T) {
	r := ring{limit: 8}
	r.write([]byte("0123456789"))
	if r.first() != 2 || string(r.read(2, 100)) != "23456789" {
		t.Fatalf("first %d read %q", r.first(), r.read(2, 100))
	}
	r.skipTo(20)
	r.write([]byte("abcdefghij"))
	if r.first() != 22 || string(r.read(22, 100)) != "cdefghij" || string(r.read(25, 2)) != "fg" {
		t.Fatalf("after skip: first %d read %q", r.first(), r.read(22, 100))
	}
}
