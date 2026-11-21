// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build linux || darwin

package ops_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/ops"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/sandboxes"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime/processrt"
	"github.com/delinoio/mono/go/ownsand/datapath"
	"github.com/delinoio/mono/go/ownsand/relaybinding"
	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

var guestBinary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "ownsand-ops-test-")
	if err != nil {
		panic(err)
	}
	guestBinary = filepath.Join(dir, "ownsand-guest")
	out, err := exec.Command("go", "build", "-o", guestBinary, "github.com/delinoio/mono/cmds/ownsand-guest").CombinedOutput()
	if err != nil {
		panic(fmt.Sprintf("build guest helper: %v\n%s", err, out))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

const orgID = "0192f4b6-7c3a-7def-8abc-0123456789ab"

type clock struct {
	mu  sync.Mutex
	off time.Duration
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return time.Now().Add(c.off) }
func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.off += d
	c.mu.Unlock()
}

type env struct {
	t       *testing.T
	st      *store.Store
	m       *sandboxes.Manager
	d       *ops.Dispatcher
	clk     *clock
	sandbox string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	rt, err := processrt.New(guestBinary, logger)
	if err != nil {
		t.Fatal(err)
	}
	m, err := sandboxes.New(context.Background(), sandboxes.Options{Store: st, Runtime: rt, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, st: st, m: m, clk: &clock{}}
	e.d, err = ops.New(m, st, ops.Options{Now: e.clk.Now, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	m.SetExecTracker(e.d)
	t.Cleanup(func() {
		e.d.Close()
		m.Close()
		rt.Close()
	})
	e.sandbox = newID(t)
	now := time.Now()
	if _, err := m.Create(context.Background(), &ownsandv1.CreateSandboxCommand{
		SandboxId:   e.sandbox,
		Source:      &ownsandv1.CreateSandboxCommand_BaseImage{BaseImage: ownsandv1.BaseImage_BASE_IMAGE_LINUX},
		Resources:   &ownsandv1.Resources{VcpuCount: 1, MemoryMib: 256, DiskGib: 1},
		NetworkMode: ownsandv1.NetworkMode_NETWORK_MODE_NONE,
		GuestOs:     hostGuestOS(),
		IdleTtl:     durationpb.New(time.Hour),
		MaxTtl:      durationpb.New(time.Hour),
		CreateTime:  timestamppb.New(now),
		MaxDeadline: timestamppb.New(now.Add(time.Hour)),
		Limits:      &ownsandv1.DeviceLimits{VcpuCount: 8, MemoryMib: 8192, DiskGib: 1, VcpuOvercommitPercent: 100},
	}); err != nil {
		t.Fatal(err)
	}
	return e
}

func hostGuestOS() ownsandv1.GuestOs {
	if strings.Contains(strings.ToLower(os.Getenv("GOOS")), "darwin") {
		return ownsandv1.GuestOs_GUEST_OS_MACOS
	}
	if _, err := os.Stat("/System/Library"); err == nil {
		return ownsandv1.GuestOs_GUEST_OS_MACOS
	}
	return ownsandv1.GuestOs_GUEST_OS_LINUX
}

func newID(t *testing.T) string {
	id, err := uuidv7.NewString()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func salt() []byte {
	b := make([]byte, 32)
	rand.Read(b)
	return b
}

type sink struct {
	mu     sync.Mutex
	frames []*ownsandv1.DataFrame
}

func (s *sink) Send(_ context.Context, f *ownsandv1.DataFrame) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frames = append(s.frames, proto.Clone(f).(*ownsandv1.DataFrame))
	return nil
}

func (e *env) handle(payload proto.Message, grant *ops.GrantCheck) (ops.Outcome, []*ownsandv1.DataFrame, bool) {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s := &sink{}
	accepted := false
	out := e.d.Handle(ctx, ops.Request{
		Frame:      datapath.MustNewFrame(7, e.sandbox, payload),
		SandboxID:  e.sandbox,
		Grant:      grant,
		OnAccepted: func() { accepted = true },
	}, s)
	for _, f := range s.frames {
		if f.GetStreamId() != 7 || f.GetSandboxId() != e.sandbox {
			e.t.Fatalf("frame on stream %d sandbox %s", f.GetStreamId(), f.GetSandboxId())
		}
	}
	return out, s.frames, accepted
}

func (e *env) execReq(cmd ...string) *ownsandv1.ExecRequest {
	return &ownsandv1.ExecRequest{OrganizationId: orgID, SandboxId: e.sandbox, ExecId: newID(e.t), Command: cmd, BindingSalt: salt()}
}

func stdout(frames []*ownsandv1.DataFrame) string {
	var b strings.Builder
	for _, f := range frames {
		b.Write(f.GetExecEvent().GetStdout().GetData())
	}
	return b.String()
}

// started reports whether a frame says that a command started.
func started(frames []*ownsandv1.DataFrame) bool {
	for _, f := range frames {
		if f.GetExecEvent().GetStarted() != nil {
			return true
		}
	}
	return false
}

func last(frames []*ownsandv1.DataFrame) *ownsandv1.DataFrame { return frames[len(frames)-1] }

func TestExecAtMostOnce(t *testing.T) {
	e := newEnv(t)
	req := e.execReq("/bin/sh", "-c", "echo run >> count; cat count")
	out, frames, accepted := e.handle(req, nil)
	if !out.Finished || !accepted || frames[0].GetExecEvent().GetStarted() == nil {
		t.Fatalf("outcome %+v accepted %v first %v", out, accepted, frames[0])
	}
	if stdout(frames) != "run\n" || last(frames).GetExecEvent().GetExited().GetExitCode() != 0 {
		t.Fatalf("stdout %q last %v", stdout(frames), last(frames))
	}
	// The identical request attaches and does not run the command again.
	out, frames, _ = e.handle(req, nil)
	if !out.Finished || stdout(frames) != "run\n" {
		t.Fatalf("repeat: %+v %q", out, stdout(frames))
	}
	other := proto.Clone(req).(*ownsandv1.ExecRequest)
	other.Command = []string{"/bin/true"}
	out, _, _ = e.handle(other, nil)
	if out.Error.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_ALREADY_EXISTS || !out.Terminal {
		t.Fatalf("other request with the same exec_id: %+v", out)
	}
	// Another spelling of the same exec_id is rejected before admission, so
	// the command does not run a second time: its output would be "run\nrun\n".
	upper := proto.Clone(req).(*ownsandv1.ExecRequest)
	upper.ExecId = strings.ToUpper(req.GetExecId())
	if upper.GetExecId() == req.GetExecId() {
		t.Fatal("test exec_id has no letter to change the case of")
	}
	out, frames, accepted = e.handle(upper, nil)
	if out.Error.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT || !out.Terminal || accepted || started(frames) {
		t.Fatalf("uppercase exec_id: %+v accepted %v started %v", out, accepted, started(frames))
	}
	attachUpper := &ownsandv1.AttachExecRequest{OrganizationId: orgID, SandboxId: e.sandbox, ExecId: upper.GetExecId()}
	if out, _, _ = e.handle(attachUpper, nil); out.Error.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT {
		t.Fatalf("attach with an uppercase exec_id: %+v", out)
	}
	if out, frames, _ = e.handle(req, nil); !out.Finished || stdout(frames) != "run\n" {
		t.Fatalf("lowercase retry after the uppercase request: %+v %q", out, stdout(frames))
	}
	// The record survives an agent restart; the command never runs again.
	d2, err := ops.New(e.m, e.st, ops.Options{Now: e.clk.Now, Logger: slog.New(slog.NewJSONHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	s := &sink{}
	out = d2.Handle(context.Background(), ops.Request{Frame: datapath.MustNewFrame(1, e.sandbox, req), SandboxID: e.sandbox}, s)
	if out.Error.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND {
		t.Fatalf("after restart: %+v", out)
	}
	out = d2.Handle(context.Background(), ops.Request{Frame: datapath.MustNewFrame(1, e.sandbox, other), SandboxID: e.sandbox}, s)
	if out.Error.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_ALREADY_EXISTS {
		t.Fatalf("other after restart: %+v", out)
	}
}

func TestExecAdmissionWindow(t *testing.T) {
	e := newEnv(t)
	old, _ := uuidv7.FromTime(time.Now().Add(-16*time.Minute), rand.Reader)
	req := e.execReq("/bin/true")
	req.ExecId = old.String()
	if out, _, _ := e.handle(req, nil); out.Error.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND {
		t.Fatalf("old exec_id: %+v", out)
	}
	future, _ := uuidv7.FromTime(time.Now().Add(6*time.Minute), rand.Reader)
	req.ExecId = future.String()
	if out, _, _ := e.handle(req, nil); out.Error.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION {
		t.Fatalf("future exec_id: %+v", out)
	}
}

func TestExecRetentionAndEndedMemory(t *testing.T) {
	e := newEnv(t)
	req := e.execReq("/bin/true")
	e.handle(req, nil)
	attach := &ownsandv1.AttachExecRequest{OrganizationId: orgID, SandboxId: e.sandbox, ExecId: req.GetExecId()}
	e.clk.Advance(9 * time.Minute)
	e.d.Sweep()
	if out, _, _ := e.handle(attach, nil); !out.Finished {
		t.Fatalf("attach within 10 minutes: %+v", out)
	}
	e.clk.Advance(2 * time.Minute)
	e.d.Sweep()
	if out, _, _ := e.handle(attach, nil); out.Error.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND {
		t.Fatalf("attach after 10 minutes: %+v", out)
	}
	// The ID is remembered: the same request never runs again.
	if out, _, _ := e.handle(req, nil); out.Error.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND {
		t.Fatalf("exec after retention: %+v", out)
	}
}

func TestAttachOffsetsAndPaused(t *testing.T) {
	e := newEnv(t)
	req := e.execReq("/bin/sh", "-c", "printf 0123456789; sleep 60")
	ctx, cancel := context.WithCancel(context.Background())
	s := &sink{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.d.Handle(ctx, ops.Request{Frame: datapath.MustNewFrame(1, e.sandbox, req), SandboxID: e.sandbox}, s)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		s.mu.Lock()
		got := stdout(s.frames)
		s.mu.Unlock()
		if got == "0123456789" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no output: %q", got)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	attach := &ownsandv1.AttachExecRequest{OrganizationId: orgID, SandboxId: e.sandbox, ExecId: req.GetExecId(), StdoutOffset: 4}
	if _, err := e.m.Pause(context.Background(), &ownsandv1.PauseSandboxCommand{SandboxId: e.sandbox}); err != nil {
		t.Fatal(err)
	}
	out, frames, accepted := e.handle(attach, nil)
	if !accepted || out.Finished || stdout(frames) != "456789" || last(frames).GetExecEvent().GetPaused() == nil {
		t.Fatalf("paused attach: %+v %v", out, frames)
	}
	// Exec on a paused sandbox fails the state rule.
	if out, _, _ := e.handle(e.execReq("/bin/true"), nil); out.Error.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION {
		t.Fatalf("exec on paused: %+v", out)
	}
	if _, err := e.m.Resume(context.Background(), &ownsandv1.ResumeSandboxCommand{SandboxId: e.sandbox}); err != nil {
		t.Fatal(err)
	}
	if !e.d.HasRunningExec(e.sandbox) {
		t.Fatal("running exec not reported")
	}
	attach.StdoutOffset = 10
	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	s2 := &sink{}
	e.d.Handle(ctx2, ops.Request{Frame: datapath.MustNewFrame(2, e.sandbox, attach), SandboxID: e.sandbox}, s2)
	if len(s2.frames) == 0 || s2.frames[0].GetExecEvent().GetStarted() == nil {
		t.Fatalf("live attach after resume: %v", s2.frames)
	}
}

func TestExecSandboxEnded(t *testing.T) {
	e := newEnv(t)
	req := e.execReq("/bin/sh", "-c", "sleep 60")
	s := &sink{}
	done := make(chan ops.Outcome, 1)
	go func() {
		done <- e.d.Handle(context.Background(), ops.Request{Frame: datapath.MustNewFrame(1, e.sandbox, req), SandboxID: e.sandbox}, s)
	}()
	for !e.d.HasRunningExec(e.sandbox) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	if _, err := e.m.Delete(context.Background(), &ownsandv1.DeleteSandboxCommand{SandboxId: e.sandbox}); err != nil {
		t.Fatal(err)
	}
	out := <-done
	if !out.Finished || last(s.frames).GetExecEvent().GetSandboxEnded().GetReason() != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DELETED_BY_CALLER {
		t.Fatalf("outcome %+v last %v", out, last(s.frames))
	}
}

func TestGrantChecks(t *testing.T) {
	e := newEnv(t)
	req := e.execReq("/bin/echo", "hi")
	digest := relaybinding.ExecDigest(req)
	execGrant := &ops.GrantCheck{Kind: ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC, OperationID: req.GetExecId(), Digest: digest}
	bad := &ops.GrantCheck{Kind: ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC, OperationID: req.GetExecId(), Digest: make([]byte, 32)}
	if out, _, accepted := e.handle(req, bad); out.Error.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED || accepted || !out.Terminal {
		t.Fatalf("digest mismatch: %+v", out)
	}
	wrongKind := &ops.GrantCheck{Kind: ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_ATTACH_EXEC, OperationID: req.GetExecId()}
	if out, _, _ := e.handle(req, wrongKind); out.Error.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED {
		t.Fatalf("exec under attach grant: %+v", out)
	}
	attach := &ownsandv1.AttachExecRequest{OrganizationId: orgID, SandboxId: e.sandbox, ExecId: req.GetExecId()}
	// AttachExec not_found under an EXEC grant keeps the grant.
	if out, _, _ := e.handle(attach, execGrant); out.Error.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND || out.Terminal {
		t.Fatalf("attach before exec: %+v", out)
	}
	if out, frames, _ := e.handle(req, execGrant); !out.Finished || stdout(frames) != "hi\n" {
		t.Fatalf("exec under grant: %+v", out)
	}
	if out, _, _ := e.handle(attach, execGrant); !out.Finished {
		t.Fatalf("attach under exec grant: %+v", out)
	}
	if out, _, _ := e.handle(attach, bad); out.Error.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED {
		t.Fatalf("attach with another digest: %+v", out)
	}
	if out, _, _ := e.handle(attach, wrongKind); !out.Finished {
		t.Fatalf("attach under attach grant: %+v", out)
	}
	otherOp := &ops.GrantCheck{Kind: ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_ATTACH_EXEC, OperationID: newID(t)}
	if out, _, _ := e.handle(attach, otherOp); out.Error.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED {
		t.Fatalf("attach of another exec_id: %+v", out)
	}
	// A frame for another sandbox.
	s := &sink{}
	out := e.d.Handle(context.Background(), ops.Request{Frame: datapath.MustNewFrame(1, newID(t), req), SandboxID: e.sandbox}, s)
	if out.Error.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED {
		t.Fatalf("other sandbox: %+v", out)
	}
}

func TestReadAndWrite(t *testing.T) {
	e := newEnv(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "f.txt")
	w := &ownsandv1.WriteFileRequest{OrganizationId: orgID, SandboxId: e.sandbox, WriteId: newID(t), Path: target, BindingSalt: salt(), Data: []byte("hello ")}
	wdigest := relaybinding.WriteFileDigest(w)
	wgrant := &ops.GrantCheck{Kind: ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_WRITE_FILE, OperationID: w.GetWriteId(), Digest: wdigest}
	out, frames, accepted := e.handle(w, wgrant)
	if out.Error != nil || accepted || frames[0].GetFileWriteResult().GetLength() != 6 {
		t.Fatalf("first chunk: %+v %v", out, frames)
	}
	// A continuation without bound fields is checked against the recorded
	// digest.
	cont := &ownsandv1.WriteFileRequest{OrganizationId: orgID, SandboxId: e.sandbox, WriteId: w.GetWriteId(), Offset: 6, Data: []byte("world"), Finish: &ownsandv1.WriteFileFinish{SizeBytes: 11, Mode: 0o644}}
	badGrant := &ops.GrantCheck{Kind: ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_WRITE_FILE, OperationID: w.GetWriteId(), Digest: make([]byte, 32)}
	if out, _, _ := e.handle(cont, badGrant); out.Error.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED {
		t.Fatalf("continuation with another digest: %+v", out)
	}
	out, frames, _ = e.handle(cont, wgrant)
	if !out.Finished || !frames[0].GetFileWriteResult().GetFinished() {
		t.Fatalf("finish: %+v %v", out, frames)
	}
	if b, _ := os.ReadFile(target); string(b) != "hello world" {
		t.Fatalf("target %q", b)
	}
	unknown := &ownsandv1.WriteFileRequest{OrganizationId: orgID, SandboxId: e.sandbox, WriteId: newID(t), Data: []byte("x")}
	if out, _, _ := e.handle(unknown, nil); out.Error.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND {
		t.Fatalf("unknown write: %+v", out)
	}

	r := &ownsandv1.ReadFileRequest{OrganizationId: orgID, SandboxId: e.sandbox, ReadId: newID(t), Path: target, BindingSalt: salt()}
	rgrant := &ops.GrantCheck{Kind: ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_READ_FILE, OperationID: r.GetReadId(), Digest: relaybinding.ReadFileDigest(r)}
	out, frames, accepted = e.handle(r, rgrant)
	var data []byte
	for _, f := range frames {
		data = append(data, f.GetFileReadEvent().GetChunk().GetData()...)
	}
	if !out.Finished || !accepted || !bytes.Equal(data, []byte("hello world")) || last(frames).GetFileReadEvent().GetEnd().GetSizeBytes() != 11 {
		t.Fatalf("read: %+v %q", out, data)
	}
	other := proto.Clone(r).(*ownsandv1.ReadFileRequest)
	other.Path = filepath.Join(dir, "other")
	if out, _, _ := e.handle(other, nil); out.Error.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED {
		t.Fatalf("read_id with another path: %+v", out)
	}
	missing := &ownsandv1.ReadFileRequest{OrganizationId: orgID, SandboxId: e.sandbox, ReadId: newID(t), Path: filepath.Join(dir, "missing"), BindingSalt: salt()}
	if out, _, _ := e.handle(missing, nil); out.Error.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND || !out.Terminal {
		t.Fatalf("missing file: %+v", out)
	}
	// File operation IDs expire 15 minutes after their last use and are
	// remembered for 30 minutes.
	e.clk.Advance(16 * time.Minute)
	e.d.Sweep()
	r.Offset = 0
	if out, _, _ := e.handle(r, nil); out.Error.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND {
		t.Fatalf("expired read: %+v", out)
	}
}

func TestExecContextCanceledWhenForwardingEnds(t *testing.T) {
	e := newEnv(t)
	// Control: a running exec keeps its context live.
	running := e.execReq("/bin/sh", "-c", "sleep 60")
	rs := &sink{}
	rdone := make(chan ops.Outcome, 1)
	go func() {
		rdone <- e.d.Handle(context.Background(), ops.Request{Frame: datapath.MustNewFrame(1, e.sandbox, running), SandboxID: e.sandbox}, rs)
	}()
	for !e.d.HasRunningExec(e.sandbox) {
		time.Sleep(10 * time.Millisecond)
	}
	if err, kept := e.d.ExecContextErr(running.GetExecId()); !kept || err != nil {
		t.Fatalf("running exec context: kept %v err %v", kept, err)
	}

	// A finished exec cancels its context and keeps its result.
	req := e.execReq("/bin/sh", "-c", "echo done")
	if out, _, _ := e.handle(req, nil); !out.Finished {
		t.Fatalf("exec: %+v", out)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		err, kept := e.d.ExecContextErr(req.GetExecId())
		if !kept {
			t.Fatal("finished exec is not kept")
		}
		if err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("exec context not canceled after forwarding ended")
		}
		time.Sleep(10 * time.Millisecond)
	}
	attach := &ownsandv1.AttachExecRequest{OrganizationId: orgID, SandboxId: e.sandbox, ExecId: req.GetExecId()}
	out, frames, _ := e.handle(attach, nil)
	if !out.Finished || stdout(frames) != "done\n" {
		t.Fatalf("attach after cancel: %+v %q", out, stdout(frames))
	}
	e.clk.Advance(11 * time.Minute)
	e.d.Sweep()
	if out, _, _ := e.handle(attach, nil); out.Error.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND {
		t.Fatalf("attach after sweep: %+v", out)
	}

	// The running exec is still live, and SandboxEnded cancels it.
	if err, _ := e.d.ExecContextErr(running.GetExecId()); err != nil {
		t.Fatalf("running exec context canceled early: %v", err)
	}
	if _, err := e.m.Delete(context.Background(), &ownsandv1.DeleteSandboxCommand{SandboxId: e.sandbox}); err != nil {
		t.Fatal(err)
	}
	<-rdone
}
