// SPDX-License-Identifier: FSL-1.1-ALv2

package ops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	"github.com/delinoio/mono/cmds/ownsand-guest/execlog"
	"github.com/delinoio/mono/go/ownsand/datapath"
	"github.com/delinoio/mono/go/ownsand/relaybinding"
	"github.com/delinoio/mono/go/ownsand/validate"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// guestStreamID is the stream_id that the agent uses on its own Serve
// streams to the guest helper; each operation has its own Serve call.
const guestStreamID = 1

type opErr struct {
	detail *ownsandv1.ErrorDetail
}

func (e *opErr) Error() string { return e.detail.GetMessage() }

func fail(code ownsandv1.ErrorCode, format string, args ...any) *opErr {
	return &opErr{detail: &ownsandv1.ErrorDetail{Code: code, Message: fmt.Sprintf(format, args...)}}
}

func asDetail(err error) *ownsandv1.ErrorDetail {
	var oe *opErr
	if errors.As(err, &oe) {
		return oe.detail
	}
	var ve *validate.Error
	if errors.As(err, &ve) {
		return ve.Detail()
	}
	var ae *datapath.AdmissionError
	if errors.As(err, &ae) {
		return ae.Detail()
	}
	return &ownsandv1.ErrorDetail{Code: ownsandv1.ErrorCode_ERROR_CODE_INTERNAL, Message: "internal agent error"}
}

// terminalCode reports whether an error leaves no operation to continue.
func terminalCode(code ownsandv1.ErrorCode) bool {
	switch code {
	case ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND,
		ownsandv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED,
		ownsandv1.ErrorCode_ERROR_CODE_ALREADY_EXISTS,
		ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION,
		ownsandv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT:
		return true
	}
	return false
}

// Handle serves the first request frame of a stream until the response of
// the operation ends on this stream (or the stream's context ends). It
// sends every response frame, including an error frame, to sink.
func (d *Dispatcher) Handle(ctx context.Context, req Request, sink Sink) Outcome {
	h := &handler{d: d, req: req, sink: sink}
	out, err := h.serve(ctx)
	if err != nil {
		detail := asDetail(err)
		out.Error = detail
		if !h.notTerminal {
			out.Terminal = terminalCode(detail.GetCode())
		}
		_ = sink.Send(ctx, datapath.MustNewFrame(req.Frame.GetStreamId(), req.Frame.GetSandboxId(), detail))
	}
	return out
}

type handler struct {
	d    *Dispatcher
	req  Request
	sink Sink
	// notTerminal keeps an error from releasing the grant (AttachExec
	// not_found under an EXEC grant).
	notTerminal bool
}

func (h *handler) send(ctx context.Context, payload proto.Message) error {
	return h.sink.Send(ctx, datapath.MustNewFrame(h.req.Frame.GetStreamId(), h.req.Frame.GetSandboxId(), payload))
}

func (h *handler) accepted() {
	if h.req.OnAccepted != nil {
		h.req.OnAccepted()
	}
}

func (h *handler) serve(ctx context.Context) (Outcome, error) {
	f := h.req.Frame
	if f.GetSandboxId() != h.req.SandboxID {
		return Outcome{}, fail(ownsandv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED, "frame names another sandbox than the session or grant")
	}
	switch p := f.GetPayload().(type) {
	case *ownsandv1.DataFrame_ExecRequest:
		return h.exec(ctx, p.ExecRequest)
	case *ownsandv1.DataFrame_ExecAttachRequest:
		return h.attach(ctx, p.ExecAttachRequest)
	case *ownsandv1.DataFrame_FileReadRequest:
		return h.read(ctx, p.FileReadRequest)
	case *ownsandv1.DataFrame_FileWriteChunk:
		return h.write(ctx, p.FileWriteChunk)
	}
	return Outcome{}, fail(ownsandv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "frame kind %s is not an operation request", f.GetKind())
}

func (h *handler) checkSandbox(sandboxID string) error {
	if sandboxID != h.req.SandboxID {
		return fail(ownsandv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED, "request names another sandbox than the session or grant")
	}
	return nil
}

// checkState applies the sandbox state rule of the request.
func (h *handler) checkState(allowPaused bool) (ownsandv1.SandboxState, error) {
	state, ok := h.d.sb.State(h.req.SandboxID)
	if ok && (state == ownsandv1.SandboxState_SANDBOX_STATE_RUNNING || (allowPaused && state == ownsandv1.SandboxState_SANDBOX_STATE_PAUSED)) {
		return state, nil
	}
	return state, fail(ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION, "sandbox is not in a state that allows this request")
}

func (h *handler) checkGrant(kinds []ownsandv1.RelayOperationKind, opID string) error {
	g := h.req.Grant
	if g == nil {
		return nil
	}
	for _, k := range kinds {
		if g.Kind == k {
			if g.OperationID != opID {
				return fail(ownsandv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED, "request operation ID does not match the relay grant")
			}
			return nil
		}
	}
	return fail(ownsandv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED, "request kind does not match the relay grant")
}

func (h *handler) checkGrantDigest(digest []byte) error {
	if g := h.req.Grant; g != nil && g.Kind != ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_ATTACH_EXEC {
		if !relaybinding.Equal(g.Digest, digest) {
			return fail(ownsandv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED, "request digest does not match the relay grant")
		}
	}
	return nil
}

// admitLocked checks an unknown operation ID: the 30-minute memory of
// ended IDs and the admission window. The caller holds d.mu.
func (h *handler) admitLocked(id string) error {
	if _, ok := h.d.ended[id]; ok {
		return fail(ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND, "operation expired, or the caller's clock may be behind")
	}
	return datapath.CheckOperationID(id, h.d.now())
}

func (h *handler) exec(ctx context.Context, req *ownsandv1.ExecRequest) (Outcome, error) {
	if err := validate.ExecRequest(req); err != nil {
		return Outcome{}, err
	}
	if err := h.checkSandbox(req.GetSandboxId()); err != nil {
		return Outcome{}, err
	}
	digest := relaybinding.ExecDigest(req)
	if err := h.checkGrant([]ownsandv1.RelayOperationKind{ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC}, req.GetExecId()); err != nil {
		return Outcome{}, err
	}
	if err := h.checkGrantDigest(digest); err != nil {
		return Outcome{}, err
	}
	if _, err := h.checkState(false); err != nil {
		return Outcome{}, err
	}
	d := h.d
	d.mu.Lock()
	op, known := d.execs[req.GetExecId()]
	if known {
		d.mu.Unlock()
		if !bytes.Equal(op.digest, digest) {
			return Outcome{}, fail(ownsandv1.ErrorCode_ERROR_CODE_ALREADY_EXISTS, "exec_id is already used by another request")
		}
		// A retry of the same request attaches from offset 0.
		h.accepted()
		return h.streamExec(ctx, op, 0, 0, false)
	}
	if lost, ok := d.lost[req.GetExecId()]; ok {
		d.mu.Unlock()
		if !bytes.Equal(lost.Digest, digest) {
			return Outcome{}, fail(ownsandv1.ErrorCode_ERROR_CODE_ALREADY_EXISTS, "exec_id is already used by another request")
		}
		return Outcome{}, fail(ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND, "the exec result is no longer kept; its outcome is unknown")
	}
	if err := h.admitLocked(req.GetExecId()); err != nil {
		d.mu.Unlock()
		return Outcome{}, err
	}
	execCtx, cancel := context.WithCancel(d.ctx)
	op = &execOp{
		sandboxID: req.GetSandboxId(),
		digest:    digest,
		idTime:    idTime(req.GetExecId()),
		log:       execlog.New(req.GetExecId(), execlog.KeptOutputBytes),
		ctx:       execCtx,
		cancel:    cancel,
	}
	d.execs[req.GetExecId()] = op
	if err := d.persistLocked(); err != nil {
		// The at-most-once rule needs the record on disk before the
		// command runs.
		delete(d.execs, req.GetExecId())
		d.mu.Unlock()
		cancel()
		d.logger.Error("persist exec record failed", "exec_id", req.GetExecId(), "error", err)
		return Outcome{}, fail(ownsandv1.ErrorCode_ERROR_CODE_UNAVAILABLE, "agent could not record the exec")
	}
	d.mu.Unlock()
	d.logger.Info("exec accepted", "exec_id", req.GetExecId(), "sandbox_id", req.GetSandboxId())
	d.sb.Touch(req.GetSandboxId())
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		// Release the child context from d.ctx once forwarding is over (final
		// event, guest error final or d.ctx end). The kept result in op.log
		// stays available until Sweep.
		defer cancel()
		d.forwardExec(execCtx, op, req)
	}()
	h.accepted()
	return h.streamExec(ctx, op, 0, 0, false)
}

func (h *handler) attach(ctx context.Context, req *ownsandv1.AttachExecRequest) (Outcome, error) {
	if err := validate.AttachExecRequest(req); err != nil {
		return Outcome{}, err
	}
	if err := h.checkSandbox(req.GetSandboxId()); err != nil {
		return Outcome{}, err
	}
	if err := h.checkGrant([]ownsandv1.RelayOperationKind{
		ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC,
		ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_ATTACH_EXEC,
	}, req.GetExecId()); err != nil {
		return Outcome{}, err
	}
	state, err := h.checkState(true)
	if err != nil {
		return Outcome{}, err
	}
	d := h.d
	d.mu.Lock()
	op, ok := d.execs[req.GetExecId()]
	d.mu.Unlock()
	if !ok || op.sandboxID != req.GetSandboxId() {
		if h.req.Grant != nil && h.req.Grant.Kind == ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC {
			// The caller may follow with the identical ExecRequest.
			h.notTerminal = true
		}
		return Outcome{}, fail(ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND, "exec is unknown or its result is no longer kept")
	}
	if err := h.checkGrantDigest(op.digest); err != nil {
		return Outcome{}, err
	}
	h.accepted()
	return h.streamExec(ctx, op, req.GetStdoutOffset(), req.GetStderrOffset(), state == ownsandv1.SandboxState_SANDBOX_STATE_PAUSED)
}

// streamExec sends the event sequence of op from the offsets. On a paused
// sandbox it sends only the kept output and result, and a paused event
// when the exec has not ended.
func (h *handler) streamExec(ctx context.Context, op *execOp, stdoutOff, stderrOff int64, paused bool) (Outcome, error) {
	c := op.log.Cursor(stdoutOff, stderrOff)
	for {
		var ev *ownsandv1.ExecEvent
		if paused {
			var changed <-chan struct{}
			ev, changed = c.TryNext()
			if ev == nil && changed != nil {
				ev = &ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Paused{Paused: &ownsandv1.ExecPaused{}}}
				return Outcome{}, h.send(ctx, ev)
			}
			if ev == nil {
				return Outcome{Finished: true}, nil
			}
		} else {
			var err error
			ev, err = c.Next(ctx, KeepaliveInterval)
			if errors.Is(err, io.EOF) {
				return Outcome{Finished: true}, nil
			}
			if err != nil {
				return Outcome{}, nil
			}
		}
		if err := h.send(ctx, ev); err != nil {
			return Outcome{}, nil
		}
		if execlog.IsFinal(ev) {
			return Outcome{Finished: true}, nil
		}
	}
}

// forwardExec runs the exec in the guest helper and applies its events to
// the exec log. It attaches again from the kept offsets after a guest
// connection loss.
func (d *Dispatcher) forwardExec(ctx context.Context, op *execOp, req *ownsandv1.ExecRequest) {
	defer d.sb.Touch(op.sandboxID)
	logger := d.logger.With("exec_id", req.GetExecId(), "sandbox_id", op.sandboxID)
	for attempt := 0; ; attempt++ {
		if op.log.Final() != nil || ctx.Err() != nil {
			return
		}
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
		guest, ok := d.sb.Guest(op.sandboxID)
		if !ok {
			continue
		}
		var first *ownsandv1.DataFrame
		if attempt == 0 {
			first = datapath.MustNewFrame(guestStreamID, op.sandboxID, req)
		} else {
			so, se := op.log.Offsets()
			first = datapath.MustNewFrame(guestStreamID, op.sandboxID, &ownsandv1.AttachExecRequest{
				OrganizationId: req.GetOrganizationId(), SandboxId: op.sandboxID, ExecId: req.GetExecId(),
				StdoutOffset: so, StderrOffset: se,
			})
		}
		done, err := d.runGuestExec(ctx, guest.Serve(ctx), first, op)
		if done {
			logger.Info("exec ended in the guest")
			return
		}
		if err != nil && ctx.Err() == nil {
			logger.Warn("guest exec stream lost; attaching again", "error", err, "attempt", attempt)
		}
	}
}

type guestStream interface {
	Send(*ownsandv1.ServeRequest) error
	Receive() (*ownsandv1.ServeResponse, error)
	CloseRequest() error
	CloseResponse() error
}

func (d *Dispatcher) runGuestExec(ctx context.Context, s guestStream, first *ownsandv1.DataFrame, op *execOp) (bool, error) {
	defer s.CloseResponse()
	if err := s.Send(&ownsandv1.ServeRequest{Frame: first}); err != nil {
		return false, err
	}
	for {
		resp, err := s.Receive()
		if err != nil {
			return false, err
		}
		f := resp.GetFrame()
		switch p := f.GetPayload().(type) {
		case *ownsandv1.DataFrame_ExecEvent:
			if p.ExecEvent.GetKeepalive() != nil {
				continue
			}
			op.log.Apply(p.ExecEvent, d.now())
			if execlog.IsFinal(p.ExecEvent) {
				return true, nil
			}
		case *ownsandv1.DataFrame_Error:
			// The guest rejected the request or no longer knows the exec.
			op.log.Finish(&ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Failed{Failed: &ownsandv1.ExecFailed{
				Reason:  ownsandv1.ExecFailureReason_EXEC_FAILURE_REASON_OTHER,
				Message: "guest helper: " + p.Error.GetMessage(),
			}}}, d.now())
			return true, nil
		}
	}
}

// fileOpFor returns the record of a read or write and registers a new one
// after the admission checks. digest is the digest of the request, or nil
// for a write continuation without bound fields.
func (h *handler) fileOpFor(m map[string]*fileOp, id, sandboxID string, digest []byte, mayCreate bool) (*fileOp, error) {
	d := h.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if op, ok := m[id]; ok {
		if op.sandboxID != sandboxID {
			return nil, fail(ownsandv1.ErrorCode_ERROR_CODE_ALREADY_EXISTS, "operation ID is already used by another sandbox")
		}
		if digest != nil && !bytes.Equal(op.digest, digest) {
			return nil, fail(ownsandv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED, "request does not match the recorded operation")
		}
		if err := h.checkGrantDigest(op.digest); err != nil {
			return nil, err
		}
		op.open++
		op.lastUse = d.now()
		return op, nil
	}
	if !mayCreate {
		if _, ok := d.ended[id]; ok {
			return nil, fail(ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND, "operation expired, or the caller's clock may be behind")
		}
		return nil, fail(ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND, "unknown operation; the first call must carry the path and the binding salt")
	}
	if err := h.checkGrantDigest(digest); err != nil {
		return nil, err
	}
	if err := h.admitLocked(id); err != nil {
		return nil, err
	}
	op := &fileOp{sandboxID: sandboxID, digest: digest, lastUse: d.now(), open: 1}
	m[id] = op
	return op, nil
}

func (d *Dispatcher) releaseFileOp(op *fileOp) {
	d.mu.Lock()
	defer d.mu.Unlock()
	op.open--
	op.lastUse = d.now()
}

func (h *handler) read(ctx context.Context, req *ownsandv1.ReadFileRequest) (Outcome, error) {
	if err := validate.ReadFileRequest(req); err != nil {
		return Outcome{}, err
	}
	if err := h.checkSandbox(req.GetSandboxId()); err != nil {
		return Outcome{}, err
	}
	if err := h.checkGrant([]ownsandv1.RelayOperationKind{ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_READ_FILE}, req.GetReadId()); err != nil {
		return Outcome{}, err
	}
	if _, err := h.checkState(false); err != nil {
		return Outcome{}, err
	}
	guest, ok := h.d.sb.Guest(req.GetSandboxId())
	if !ok {
		return Outcome{}, fail(ownsandv1.ErrorCode_ERROR_CODE_UNAVAILABLE, "guest helper is not reachable")
	}
	op, err := h.fileOpFor(h.d.reads, req.GetReadId(), req.GetSandboxId(), relaybinding.ReadFileDigest(req), true)
	if err != nil {
		return Outcome{}, err
	}
	defer h.d.releaseFileOp(op)
	h.d.sb.Touch(req.GetSandboxId())
	defer h.d.sb.Touch(req.GetSandboxId())
	h.accepted()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s := guest.Serve(ctx)
	defer s.CloseResponse()
	if err := s.Send(&ownsandv1.ServeRequest{Frame: datapath.MustNewFrame(guestStreamID, req.GetSandboxId(), req)}); err != nil {
		return Outcome{}, guestError(err)
	}
	for {
		resp, err := s.Receive()
		if err != nil {
			return Outcome{}, guestError(err)
		}
		switch p := resp.GetFrame().GetPayload().(type) {
		case *ownsandv1.DataFrame_FileReadEvent:
			if err := h.send(ctx, p.FileReadEvent); err != nil {
				return Outcome{}, nil
			}
			if p.FileReadEvent.GetEnd() != nil {
				return Outcome{Finished: true}, nil
			}
		case *ownsandv1.DataFrame_Error:
			return Outcome{}, &opErr{detail: p.Error}
		}
	}
}

func (h *handler) write(ctx context.Context, req *ownsandv1.WriteFileRequest) (Outcome, error) {
	if err := h.checkSandbox(req.GetSandboxId()); err != nil {
		return Outcome{}, err
	}
	if err := h.checkGrant([]ownsandv1.RelayOperationKind{ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_WRITE_FILE}, req.GetWriteId()); err != nil {
		return Outcome{}, err
	}
	bound := req.GetPath() != "" || len(req.GetBindingSalt()) > 0
	if err := validate.WriteFileRequest(req, false); err != nil {
		return Outcome{}, err
	}
	if _, err := h.checkState(false); err != nil {
		return Outcome{}, err
	}
	guest, ok := h.d.sb.Guest(req.GetSandboxId())
	if !ok {
		return Outcome{}, fail(ownsandv1.ErrorCode_ERROR_CODE_UNAVAILABLE, "guest helper is not reachable")
	}
	var digest []byte
	if bound {
		digest = relaybinding.WriteFileDigest(req)
	}
	op, err := h.fileOpFor(h.d.writes, req.GetWriteId(), req.GetSandboxId(), digest, bound)
	if err != nil {
		return Outcome{}, err
	}
	defer h.d.releaseFileOp(op)
	h.d.sb.Touch(req.GetSandboxId())
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s := guest.Serve(ctx)
	defer s.CloseResponse()
	if err := s.Send(&ownsandv1.ServeRequest{Frame: datapath.MustNewFrame(guestStreamID, req.GetSandboxId(), req)}); err != nil {
		return Outcome{}, guestError(err)
	}
	for {
		resp, err := s.Receive()
		if err != nil {
			return Outcome{}, guestError(err)
		}
		switch p := resp.GetFrame().GetPayload().(type) {
		case *ownsandv1.DataFrame_FileWriteResult:
			if err := h.send(ctx, p.FileWriteResult); err != nil {
				return Outcome{}, nil
			}
			return Outcome{Finished: p.FileWriteResult.GetFinished()}, nil
		case *ownsandv1.DataFrame_Error:
			return Outcome{}, &opErr{detail: p.Error}
		}
	}
}

func guestError(err error) error {
	if connect.CodeOf(err) == connect.CodeCanceled || errors.Is(err, context.Canceled) {
		return fail(ownsandv1.ErrorCode_ERROR_CODE_CANCELED, "request canceled")
	}
	return fail(ownsandv1.ErrorCode_ERROR_CODE_UNAVAILABLE, "guest helper is not reachable")
}
