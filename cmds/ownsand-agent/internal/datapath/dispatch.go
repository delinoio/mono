package datapath

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/guestconn"
	sdkdp "github.com/delinoio/mono/packages/ownsand-sdk-go/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// keepaliveInterval is the longest silence on an exec stream; the contract
// requires at least one keepalive every 60 seconds.
const keepaliveInterval = 30 * time.Second

// SandboxAccess is what the data path needs from the sandbox manager.
type SandboxAccess interface {
	SandboxState(sandboxID string) ownsandv1.SandboxState
	Report(sandboxID string) *ownsandv1.SandboxReport
	Guest(ctx context.Context, sandboxID string) (*guestconn.Client, error)
	Touch(sandboxID string)
	BeginActivity(sandboxID string) (end func())
}

// Sink is the transport side of one request stream: a stream of a
// peer-to-peer session or a relay stream.
type Sink interface {
	// Payload sends a payload frame toward the caller, within the credit
	// window of the transport.
	Payload(set func(*ownsandv1.DataFrame)) error
	// Error sends an error frame; it uses no credit.
	Error(code connect.Code, msg string) error
	// Context ends when the stream ends.
	Context() context.Context
}

// Outcome is the result of one served request, which the relay grant
// release rules use.
type Outcome struct {
	// Finished: the operation finished and its final message was sent (the
	// exec result, the read's final message or the write's finish result).
	Finished bool
	// Code is the error code that ended the stream, if any.
	Code connect.Code
	// Started: the request reached the guest, so the operation may continue
	// under a new grant.
	Started bool
}

// Dispatcher serves exec, attach, read and write requests from both
// transports with one implementation.
type Dispatcher struct {
	sb  SandboxAccess
	log *slog.Logger
	reg *registry
	ctx context.Context
}

// NewDispatcher returns a Dispatcher. ctx bounds the exec mirrors.
func NewDispatcher(ctx context.Context, sb SandboxAccess, log *slog.Logger, now func() time.Time) *Dispatcher {
	if now == nil {
		now = time.Now
	}
	return &Dispatcher{sb: sb, log: log, reg: newRegistry(now), ctx: ctx}
}

// Sweep drops expired operation state. The data path calls it periodically.
func (d *Dispatcher) Sweep() { d.reg.sweep() }

func errCode(err error) connect.Code {
	c := connect.CodeOf(err)
	if c == connect.CodeUnknown {
		return connect.CodeInternal
	}
	return c
}

func message(err error) string {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return ce.Message()
	}
	return "internal error"
}

func fail(sink Sink, err error) Outcome {
	code := errCode(err)
	_ = sink.Error(code, message(err))
	return Outcome{Code: code}
}

// IsRequest reports whether f is an operation request frame.
func IsRequest(f *ownsandv1.DataFrame) bool {
	switch f.GetPayload().(type) {
	case *ownsandv1.DataFrame_ExecRequest, *ownsandv1.DataFrame_ExecAttachRequest,
		*ownsandv1.DataFrame_FileReadRequest, *ownsandv1.DataFrame_FileWriteChunk:
		return true
	}
	return false
}

// requestSandbox returns the sandbox ID inside a request.
func requestSandbox(f *ownsandv1.DataFrame) string {
	switch p := f.GetPayload().(type) {
	case *ownsandv1.DataFrame_ExecRequest:
		return p.ExecRequest.GetSandboxId()
	case *ownsandv1.DataFrame_ExecAttachRequest:
		return p.ExecAttachRequest.GetSandboxId()
	case *ownsandv1.DataFrame_FileReadRequest:
		return p.FileReadRequest.GetSandboxId()
	case *ownsandv1.DataFrame_FileWriteChunk:
		return p.FileWriteChunk.GetSandboxId()
	}
	return ""
}

func stateError(st ownsandv1.SandboxState, allowPaused bool) error {
	if st == ownsandv1.SandboxState_SANDBOX_STATE_RUNNING || (allowPaused && st == ownsandv1.SandboxState_SANDBOX_STATE_PAUSED) {
		return nil
	}
	return connect.NewError(connect.CodeFailedPrecondition, errors.New("the sandbox is not in a state that allows this request"))
}

// Serve serves one request frame for the sandbox that the session or grant
// is bound to, and sends the response frames to sink.
func (d *Dispatcher) Serve(sandboxID string, req *ownsandv1.DataFrame, sink Sink) Outcome {
	if !IsRequest(req) {
		return fail(sink, connect.NewError(connect.CodeInvalidArgument, errors.New("the first frame of a stream must be a request")))
	}
	// The request names the sandbox that the session or grant authorized;
	// the frame's sandbox_id and the request field must both match it.
	if (req.GetSandboxId() != "" && req.GetSandboxId() != sandboxID) || (requestSandbox(req) != "" && requestSandbox(req) != sandboxID) {
		return fail(sink, connect.NewError(connect.CodePermissionDenied, errors.New("the request names another sandbox")))
	}
	switch p := req.GetPayload().(type) {
	case *ownsandv1.DataFrame_ExecRequest:
		return d.serveExec(sandboxID, p.ExecRequest, sink)
	case *ownsandv1.DataFrame_ExecAttachRequest:
		return d.serveAttach(sandboxID, p.ExecAttachRequest, sink)
	case *ownsandv1.DataFrame_FileReadRequest:
		return d.serveRead(sandboxID, p.FileReadRequest, sink)
	case *ownsandv1.DataFrame_FileWriteChunk:
		return d.serveWrite(sandboxID, p.FileWriteChunk, sink)
	}
	return Outcome{}
}

func execFieldsSize(req *ownsandv1.ExecRequest) int {
	n := len(req.GetWorkingDirectory())
	for _, a := range req.GetCommand() {
		n += len(a) + 2
	}
	for k, v := range req.GetEnv() {
		n += len(k) + len(v) + 4
	}
	return n
}

func (d *Dispatcher) serveExec(sid string, req *ownsandv1.ExecRequest, sink Sink) Outcome {
	if execFieldsSize(req) > sdkdp.MaxExecRequestFields {
		return fail(sink, connect.NewError(connect.CodeInvalidArgument, errors.New("command, env and working directory exceed 256 KiB")))
	}
	if len(req.GetCommand()) == 0 {
		return fail(sink, connect.NewError(connect.CodeInvalidArgument, errors.New("command must name a program")))
	}
	if err := stateError(d.sb.SandboxState(sid), false); err != nil {
		return fail(sink, err)
	}
	digest := sdkdp.ExecDigest(req)
	d.reg.mu.Lock()
	m := d.reg.execs[req.GetExecId()]
	if m != nil {
		d.reg.mu.Unlock()
		if !bytes.Equal(m.digest, digest) {
			return fail(sink, connect.NewError(connect.CodeAlreadyExists, errors.New("the exec_id is already used by another request")))
		}
		// An identical request attaches from offset 0 and does not run the
		// command again.
		return d.subscribe(sid, m, 0, 0, sink)
	}
	if err := d.reg.checkNewIDLocked(req.GetExecId()); err != nil {
		d.reg.mu.Unlock()
		return fail(sink, err)
	}
	m = newExecMirror(req.GetExecId(), sid, digest)
	d.reg.execs[m.id] = m
	d.reg.mu.Unlock()
	d.log.Info("exec accepted", "sandbox_id", sid, "exec_id", m.id)
	go d.runMirror(m, req)
	return d.subscribe(sid, m, 0, 0, sink)
}

func (d *Dispatcher) serveAttach(sid string, req *ownsandv1.AttachExecRequest, sink Sink) Outcome {
	if err := stateError(d.sb.SandboxState(sid), true); err != nil {
		return fail(sink, err)
	}
	if req.GetStdoutOffset() < 0 || req.GetStderrOffset() < 0 {
		return fail(sink, connect.NewError(connect.CodeInvalidArgument, errors.New("offsets must not be negative")))
	}
	d.reg.mu.Lock()
	m := d.reg.execs[req.GetExecId()]
	d.reg.mu.Unlock()
	if m == nil || m.sandboxID != sid {
		return fail(sink, connect.NewError(connect.CodeNotFound, errors.New("the exec is unknown or its result is no longer kept")))
	}
	return d.subscribe(sid, m, req.GetStdoutOffset(), req.GetStderrOffset(), sink)
}

// forward opens a guest stream for req and relays its frames to sink until
// a final frame. It returns the last frame.
func (d *Dispatcher) forward(sid string, req *ownsandv1.DataFrame, sink Sink, final func(*ownsandv1.DataFrame) bool) (*ownsandv1.DataFrame, error) {
	ctx := sink.Context()
	gc, err := d.sb.Guest(ctx, sid)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("the guest helper is not reachable"))
	}
	gs, err := gc.Open(req)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("the guest helper is not reachable"))
	}
	defer gs.Close()
	for {
		f, err := gs.Recv(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, connect.NewError(connect.CodeUnavailable, errors.New("the guest connection ended"))
		}
		if e := f.GetError(); e != nil {
			return f, connect.NewError(codeFromName(e.GetCode()), errors.New(e.GetMessage()))
		}
		if err := sink.Payload(func(out *ownsandv1.DataFrame) { out.Payload = f.GetPayload() }); err != nil {
			return nil, err
		}
		if final(f) {
			return f, nil
		}
	}
}

// codeFromName maps a Connect code name to the code.
func codeFromName(name string) connect.Code {
	var c connect.Code
	if err := c.UnmarshalText([]byte(name)); err != nil {
		return connect.CodeInternal
	}
	return c
}

func (d *Dispatcher) serveRead(sid string, req *ownsandv1.ReadFileRequest, sink Sink) Outcome {
	if err := stateError(d.sb.SandboxState(sid), false); err != nil {
		return fail(sink, err)
	}
	op, err := d.reg.beginFile(opRead, req.GetReadId(), sdkdp.ReadDigest(req), true)
	if err != nil {
		return fail(sink, err)
	}
	d.sb.Touch(sid)
	defer d.sb.Touch(sid)
	frame := sdkdp.NewFrame(0, sid, func(f *ownsandv1.DataFrame) { f.Payload = &ownsandv1.DataFrame_FileReadRequest{FileReadRequest: req} })
	last, err := d.forward(sid, frame, sink, func(f *ownsandv1.DataFrame) bool { return f.GetFileReadFinished() != nil })
	finished := err == nil && last.GetFileReadFinished() != nil
	d.reg.endFile(op, finished)
	if err != nil {
		if sink.Context().Err() != nil {
			return Outcome{Started: true}
		}
		o := fail(sink, err)
		o.Started = last != nil || connect.CodeOf(err) == connect.CodeUnavailable
		return o
	}
	return Outcome{Finished: true, Started: true}
}

func (d *Dispatcher) serveWrite(sid string, req *ownsandv1.WriteFileRequest, sink Sink) Outcome {
	if err := stateError(d.sb.SandboxState(sid), false); err != nil {
		return fail(sink, err)
	}
	if c := req.GetChunk(); c != nil && len(c.GetData()) > sdkdp.MaxFileChunk {
		return fail(sink, connect.NewError(connect.CodeInvalidArgument, errors.New("a chunk holds at most 1 MiB")))
	}
	first := req.GetPath() != ""
	var digest []byte
	if first {
		digest = sdkdp.WriteDigest(req)
	}
	op, err := d.reg.beginFile(opWrite, req.GetWriteId(), digest, first)
	if err != nil {
		return fail(sink, err)
	}
	d.sb.Touch(sid)
	defer d.sb.Touch(sid)
	frame := sdkdp.NewFrame(0, sid, func(f *ownsandv1.DataFrame) { f.Payload = &ownsandv1.DataFrame_FileWriteChunk{FileWriteChunk: req} })
	last, err := d.forward(sid, frame, sink, func(f *ownsandv1.DataFrame) bool { return f.GetFileWriteResult() != nil })
	finished := err == nil && last.GetFileWriteResult().GetFinished()
	d.reg.endFile(op, finished)
	if err != nil {
		if sink.Context().Err() != nil {
			return Outcome{Started: true}
		}
		o := fail(sink, err)
		o.Started = true
		return o
	}
	return Outcome{Finished: finished, Started: true}
}

// GrantTarget is what CheckGrant needs from a relay grant.
type GrantTarget struct {
	SandboxID string
	Kind      ownsandv1.RelayOperationKind
	OpID      string
	Digest    []byte
}

// CheckGrant checks the first request frame of a relay stream against its
// grant: the request kind, the operation ID and the request digest. A
// continuation (AttachExec under an EXEC grant, a later WriteFile chunk) is
// checked against the recorded digest of its operation.
func (d *Dispatcher) CheckGrant(g GrantTarget, req *ownsandv1.DataFrame) error {
	deny := connect.NewError(connect.CodePermissionDenied, errors.New("the request does not match its relay grant"))
	if requestSandbox(req) != g.SandboxID {
		return deny
	}
	switch p := req.GetPayload().(type) {
	case *ownsandv1.DataFrame_ExecRequest:
		if g.Kind != ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC || p.ExecRequest.GetExecId() != g.OpID || !bytes.Equal(sdkdp.ExecDigest(p.ExecRequest), g.Digest) {
			return deny
		}
	case *ownsandv1.DataFrame_ExecAttachRequest:
		if p.ExecAttachRequest.GetExecId() != g.OpID {
			return deny
		}
		switch g.Kind {
		case ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_ATTACH_EXEC:
		case ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC:
			rec, ok := d.reg.recordedDigest(opExec, g.OpID)
			if !ok {
				return connect.NewError(connect.CodeNotFound, errors.New("the exec is unknown or its result is no longer kept"))
			}
			if !bytes.Equal(rec, g.Digest) {
				return deny
			}
		default:
			return deny
		}
	case *ownsandv1.DataFrame_FileReadRequest:
		if g.Kind != ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_READ_FILE || p.FileReadRequest.GetReadId() != g.OpID || !bytes.Equal(sdkdp.ReadDigest(p.FileReadRequest), g.Digest) {
			return deny
		}
		if rec, ok := d.reg.recordedDigest(opRead, g.OpID); ok && !bytes.Equal(rec, g.Digest) {
			return deny
		}
	case *ownsandv1.DataFrame_FileWriteChunk:
		w := p.FileWriteChunk
		if g.Kind != ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_WRITE_FILE || w.GetWriteId() != g.OpID {
			return deny
		}
		if w.GetPath() != "" {
			if !bytes.Equal(sdkdp.WriteDigest(w), g.Digest) {
				return deny
			}
		} else {
			rec, ok := d.reg.recordedDigest(opWrite, g.OpID)
			if !ok {
				return connect.NewError(connect.CodeNotFound, errors.New("the write is unknown or expired"))
			}
			if !bytes.Equal(rec, g.Digest) {
				return deny
			}
		}
	default:
		return deny
	}
	return nil
}
