// SPDX-License-Identifier: Apache-2.0

package ownsand

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/delinoio/mono/go/ownsand/relaybinding"
	"github.com/delinoio/mono/go/ownsand/validate"
	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// ExecEventKind is the kind of an exec event that the SDK reports. The
// relay accepted and keepalive events, and the started event that an
// AttachExec continuation repeats, never reach the caller.
type ExecEventKind int

const (
	ExecEventStarted ExecEventKind = iota + 1
	ExecEventStdout
	ExecEventStderr
	// ExecEventOutputTruncated reports output that a continuation lost.
	// The result is then marked incomplete.
	ExecEventOutputTruncated
)

// String returns the stable identifier of k, as the CLI JSON output uses it.
func (k ExecEventKind) String() string {
	switch k {
	case ExecEventStarted:
		return "started"
	case ExecEventStdout:
		return "stdout"
	case ExecEventStderr:
		return "stderr"
	case ExecEventOutputTruncated:
		return "output_truncated"
	}
	return fmt.Sprintf("ExecEventKind(%d)", int(k))
}

// ByteRange is the half-open byte range [From, To) of an output stream.
type ByteRange struct {
	From int64
	To   int64
}

// Empty reports whether the range holds no byte.
func (r ByteRange) Empty() bool { return r.To <= r.From }

// ExecEvent is one exec event.
type ExecEvent struct {
	Kind   ExecEventKind
	ExecID string
	// StartTime is the guest time of an ExecEventStarted, if available.
	StartTime *time.Time
	// Offset and Data of a stdout or stderr chunk. Data is sandbox content.
	Offset int64
	Data   []byte
	// MissingStdout and MissingStderr of an output_truncated event.
	MissingStdout ByteRange
	MissingStderr ByteRange
}

// ExecResultKind is the kind of an exec result.
type ExecResultKind int

const (
	// ExecResultExited: the command ended with an exit code or a signal.
	ExecResultExited ExecResultKind = iota + 1
	// ExecResultFailed: the command could not start.
	ExecResultFailed
	// ExecResultSandboxEnded: the sandbox ended before the exec result.
	ExecResultSandboxEnded
)

// String returns the stable identifier of k.
func (k ExecResultKind) String() string {
	switch k {
	case ExecResultExited:
		return "exited"
	case ExecResultFailed:
		return "failed"
	case ExecResultSandboxEnded:
		return "sandbox_ended"
	}
	return fmt.Sprintf("ExecResultKind(%d)", int(k))
}

// ExecResult is the result of an exec. A data path failure is never a
// result; it is a DataPathError.
type ExecResult struct {
	ExecID string
	Kind   ExecResultKind
	// ExitCode or Signal is set for ExecResultExited.
	ExitCode *int64
	Signal   *int32
	// EndTime is the guest time of an ExecResultExited, if available.
	EndTime *time.Time
	// TimedOut is true when the exec timeout stopped the command.
	TimedOut bool
	// FailureReason and FailureMessage are set for ExecResultFailed.
	FailureReason  ownsandv1.ExecFailureReason
	FailureMessage string
	// EndReason is set for ExecResultSandboxEnded.
	EndReason ownsandv1.SandboxEndReason
	// Incomplete is true when output was lost (output_truncated); the
	// output that the caller received is not the complete output.
	Incomplete bool
	// Transport is the transport that delivered the result.
	Transport ownsandv1.DataPathTransport
}

// ExecParams describes a command.
type ExecParams struct {
	// ExecID is the exec_id (a UUID v7). Empty generates one.
	ExecID string
	// Command is the argument array; the first element is the program. A
	// shell runs only when the caller passes one.
	Command          []string
	Env              map[string]string
	WorkingDirectory string
	// Timeout stops the command; zero means no timeout.
	Timeout time.Duration
	// OnEvent receives the events in order. An error from it cancels the
	// exec stream (the command keeps running on the agent) and Exec returns
	// that error.
	OnEvent func(ExecEvent) error
}

// AttachParams describes an AttachExec of an exec that the caller did not
// start or whose request it no longer has.
type AttachParams struct {
	StdoutOffset int64
	StderrOffset int64
	OnEvent      func(ExecEvent) error
}

// callbackError wraps an error from OnEvent, so it ends the operation.
type callbackError struct{ err error }

func (e *callbackError) Error() string { return e.err.Error() }
func (e *callbackError) Unwrap() error { return e.err }

// errNotAccepted reports a relay stream that ended without an accepted
// event and without a final event; it is not a rotation.
var errNotAccepted = errors.New("ownsand: relay stream ended before the agent accepted it")

// Exec runs a command and streams its events to p.OnEvent. The SDK
// generates the exec_id before it sends the request, never runs a command
// twice and continues an interrupted exec with AttachExec on a new session
// or on the relay.
func (s *Sandbox) Exec(ctx context.Context, p ExecParams) (*ExecResult, error) {
	execID := p.ExecID
	if execID == "" {
		id, err := uuidv7.NewString()
		if err != nil {
			return nil, err
		}
		execID = id
	}
	salt, err := relaybinding.NewSalt()
	if err != nil {
		return nil, err
	}
	req := &ownsandv1.ExecRequest{
		OrganizationId:   s.org,
		SandboxId:        s.id,
		ExecId:           execID,
		Command:          p.Command,
		Env:              p.Env,
		WorkingDirectory: p.WorkingDirectory,
		BindingSalt:      salt,
	}
	if p.Timeout != 0 {
		req.Timeout = durationpb.New(p.Timeout)
	}
	if err := validate.ExecRequest(req); err != nil {
		return nil, err
	}
	op := &execOp{
		s:        s,
		req:      req,
		execID:   execID,
		needExec: true,
		onEvent:  p.OnEvent,
		rec:      newRecovery(s.c.timing.recoveryWindow),
		grant:    newRelayGrant(s, ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC, execID, relaybinding.ExecDigest(req)),
	}
	return op.run(ctx)
}

// AttachExec continues the event stream of an exec from the given offsets.
// On the relay it uses an ATTACH_EXEC grant, which needs no request fields.
func (s *Sandbox) AttachExec(ctx context.Context, execID string, p AttachParams) (*ExecResult, error) {
	req := &ownsandv1.AttachExecRequest{OrganizationId: s.org, SandboxId: s.id, ExecId: execID, StdoutOffset: p.StdoutOffset, StderrOffset: p.StderrOffset}
	if err := validate.AttachExecRequest(req); err != nil {
		return nil, err
	}
	op := &execOp{
		s:       s,
		execID:  execID,
		outOff:  p.StdoutOffset,
		errOff:  p.StderrOffset,
		onEvent: p.OnEvent,
		rec:     newRecovery(s.c.timing.recoveryWindow),
		grant:   newRelayGrant(s, ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_ATTACH_EXEC, execID, nil),
	}
	return op.run(ctx)
}

type execOp struct {
	s       *Sandbox
	req     *ownsandv1.ExecRequest // nil for an attach-only caller
	execID  string
	onEvent func(ExecEvent) error

	needExec   bool // the next attempt sends the identical ExecRequest
	started    bool
	outOff     int64
	errOff     int64
	incomplete bool

	rec   *recovery
	grant *relayGrant
}

// attempt is the outcome of one stream.
type attempt struct {
	result   *ExecResult
	err      error // io.EOF for a stream that ended cleanly without a result
	accepted bool
	// served reports that the agent served the request on a peer-to-peer
	// session: it sent the accepted frame or any exec event. The agent
	// sends no accepted frame on a session, only on the relay, so an exec
	// event is the evidence of an accepted continuation there.
	served   bool
	data     bool
	paused   bool
	sentExec bool
	authErr  bool
}

func (op *execOp) opName() string {
	if op.req == nil {
		return "attach-exec"
	}
	return "exec"
}

func (op *execOp) dpErr(err error, unknown bool) error {
	var cb *callbackError
	if errors.As(err, &cb) {
		return cb.err
	}
	if !errors.Is(err, errRecoveryDeadline) && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return err
	}
	return &DataPathError{Operation: op.opName(), OperationID: op.execID, OutcomeUnknown: unknown, Err: err}
}

func (op *execOp) sandboxEnded(e *ErrSandboxEnded) *ExecResult {
	return &ExecResult{ExecID: op.execID, Kind: ExecResultSandboxEnded, EndReason: e.Reason, Incomplete: op.incomplete, Transport: op.s.dp.current()}
}

func (op *execOp) run(ctx context.Context) (*ExecResult, error) {
	s := op.s
	sessionErrors := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		sess, err := s.dp.route(ctx)
		if err != nil {
			return nil, op.dpErr(err, false)
		}
		var at attempt
		if sess != nil {
			at = op.p2pAttempt(ctx, sess)
		} else {
			at = op.relayAttempt(ctx)
		}
		if at.sentExec {
			op.needExec = false
		}
		if at.result != nil {
			at.result.Incomplete = op.incomplete
			return at.result, nil
		}
		if at.data {
			sessionErrors = 0
		}
		if at.paused {
			// A paused event ends the recovery window; wait for the
			// sandbox to resume, end, or the caller to cancel.
			op.rec.reset()
			if err := s.waitForResume(ctx); err != nil {
				var ended *ErrSandboxEnded
				if errors.As(err, &ended) {
					return op.sandboxEnded(ended), nil
				}
				return nil, op.dpErr(err, false)
			}
			continue
		}
		err = at.err
		var cb *callbackError
		if errors.As(err, &cb) {
			return nil, cb.err
		}
		if errors.Is(err, io.EOF) {
			if at.accepted {
				// A routine stream rotation: continue at once.
				op.rec.reset()
				continue
			}
			err = errNotAccepted
		}
		if sess != nil && s.dp.handleSessionError(sess, err) {
			if (at.accepted || at.served) && isRoutineSessionEnd(err) {
				// A routine session end (idle, the 1-hour expiry) or a
				// retirement (another request's interrupted send tore a
				// frame) of a continuation that the agent accepted shows
				// that the operation recovered on its new session. It is
				// not a failure, so a quiet exec can outlive any number of
				// session expiries and retirements. Only failed sessions
				// (a data channel failure, an end or retirement before the
				// agent accepted) count.
				sessionErrors = 0
				continue
			}
			sessionErrors++
			if sessionErrors > 5 {
				return nil, op.dpErr(err, false)
			}
			continue
		}
		// The caller's context stays authoritative. It can end while the
		// attempt fails, so the attempt can report a status that the server
		// answered for the caller's deadline (Connect sends the deadline as a
		// timeout header, and the server's timer can fire first) or the
		// stream reset of the cancellation. classify would take such an
		// error for a terminal failure of the operation.
		if cerr := callerDeadlineEnded(ctx, err, s.c.timing.callerDeadlineSkew); cerr != nil {
			return nil, cerr
		}
		if at.authErr && connect.CodeOf(err) != connect.CodeFailedPrecondition {
			return nil, op.dpErr(err, false)
		}
		kind, delay := classify(ctx, err)
		if at.authErr || kind == failTerminal {
			switch connect.CodeOf(err) {
			case connect.CodeNotFound:
				if !at.sentExec && !op.started && op.req != nil {
					// AttachExec before started found no exec: send the
					// identical ExecRequest (the same exec_id makes it safe).
					op.needExec = true
					continue
				}
				return nil, op.dpErr(err, true)
			case connect.CodeFailedPrecondition:
				sb, gerr := op.lookupSandbox(ctx)
				if gerr != nil {
					return nil, op.dpErr(gerr, false)
				}
				if IsTerminal(sb.GetState()) {
					return op.sandboxEnded(&ErrSandboxEnded{State: sb.GetState(), Reason: sb.GetEndReason()}), nil
				}
				if !at.authErr && isPaused(sb) && op.req != nil && !op.started {
					// A paused sandbox without an exec record: end the
					// recovery window and send the identical request after
					// the resume.
					op.rec.reset()
					if werr := s.waitForResume(ctx); werr != nil {
						var ended *ErrSandboxEnded
						if errors.As(werr, &ended) {
							return op.sandboxEnded(ended), nil
						}
						return nil, op.dpErr(werr, false)
					}
					op.needExec = true
					continue
				}
			}
			return nil, op.dpErr(err, false)
		}
		if kind == failDeadline {
			// Only WriteFile retries deadline_exceeded, because its retry
			// is idempotent by write_id. For an exec, the status is the
			// control plane's final answer; unless the caller's own
			// deadline caused it (handled above), the operation ends with
			// it.
			return nil, op.dpErr(err, false)
		}
		op.rec.fail(err)
		if sess == nil && kind != failExhausted {
			op.grant.discard()
		}
		if berr := op.rec.backoff(ctx, s.c.timing, delay); berr != nil {
			return nil, op.dpErr(berr, false)
		}
	}
}

// lookupSandbox checks the sandbox after failed_precondition. A lookup
// failure leaves the original status in effect unless the caller or the
// recovery window ends the lookup.
func (op *execOp) lookupSandbox(ctx context.Context) (*ownsandv1.Sandbox, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s := op.s
	lookupCtx := ctx
	var deadline time.Time
	if op.rec.inRecovery() {
		deadline = time.Now().Add(op.rec.remaining())
		// Cancel locally so Connect sends only the caller's timeout.
		var cancel context.CancelCauseFunc
		lookupCtx, cancel = context.WithCancelCause(ctx)
		defer cancel(nil)
		left := time.Until(deadline)
		if left <= 0 {
			return nil, op.rec.deadlineErr()
		}
		timer := time.AfterFunc(left, func() { cancel(errRecoveryDeadline) })
		defer timer.Stop()
	}
	sb, err := s.getSandbox(lookupCtx)
	callerDeadline, hasCallerDeadline := ctx.Deadline()
	callerDeadlineFirst := hasCallerDeadline && (deadline.IsZero() || !deadline.Before(callerDeadline))
	if context.Cause(lookupCtx) == errRecoveryDeadline && !callerDeadlineFirst {
		return nil, op.rec.deadlineErr()
	}
	if cerr := ctx.Err(); cerr != nil {
		return nil, cerr
	}
	// A reply can arrive after a deadline before its timer runs.
	now := time.Now()
	if callerDeadlineFirst && !now.Before(callerDeadline) {
		return nil, context.DeadlineExceeded
	}
	if !deadline.IsZero() && !now.Before(deadline) {
		return nil, op.rec.deadlineErr()
	}
	if err != nil {
		if callerDeadlineFirst {
			return nil, callerDeadlineEnded(ctx, err, s.c.timing.callerDeadlineSkew)
		}
		return nil, nil
	}
	sb, _ = s.accept("GetSandbox", sb)
	return sb, nil
}

// request returns the payload of the next attempt.
func (op *execOp) request() (proto.Message, bool) {
	if op.needExec {
		return proto.Clone(op.req), true
	}
	return &ownsandv1.AttachExecRequest{
		OrganizationId: op.s.org,
		SandboxId:      op.s.id,
		ExecId:         op.execID,
		StdoutOffset:   op.outOff,
		StderrOffset:   op.errOff,
	}, false
}

func (op *execOp) p2pAttempt(ctx context.Context, sess *p2pSession) attempt {
	payload, isExec := op.request()
	at := attempt{sentExec: isExec}
	st, err := sess.openStream(ctx, payload)
	if err != nil {
		at.err = err
		return at
	}
	defer st.close()
	for {
		f, err := st.recv(ctx)
		if err != nil {
			at.err = err
			return at
		}
		switch f.GetKind() {
		case ownsandv1.DataFrameKind_DATA_FRAME_KIND_EXEC_EVENT:
			at.served = true
			if done := op.handleEvent(f.GetExecEvent(), &at, ownsandv1.DataPathTransport_DATA_PATH_TRANSPORT_PEER_TO_PEER); done {
				return at
			}
		case ownsandv1.DataFrameKind_DATA_FRAME_KIND_REQUEST_ACCEPTED:
			at.accepted = true
		case ownsandv1.DataFrameKind_DATA_FRAME_KIND_ERROR:
			at.err = errorFromDetail(f.GetError())
			return at
		}
	}
}

// serverStream is the common part of the relay server streams.
type serverStream interface {
	Receive() bool
	Err() error
	Close() error
}

func (op *execOp) relayAttempt(ctx context.Context) attempt {
	c := op.s.c
	t := c.timing
	op.s.dp.setInUse(ownsandv1.DataPathTransport_DATA_PATH_TRANSPORT_RELAY)
	payload, isExec := op.request()
	at := attempt{}
	if err := op.grant.ensure(ctx, t.relayStreamLimit+t.streamMargin, op.rec); err != nil {
		at.err = err
		at.authErr = true
		return at
	}
	at.sentExec = isExec
	budget := op.rec.streamBudget()
	if budget < 0 {
		// The recovery window ended while the grant was renewed: start no
		// continuation.
		at.err = op.rec.deadlineErr()
		return at
	}
	actx, disarm, stop := c.stallContext(ctx, budget)
	defer stop()
	defer op.grant.streamEnded()
	var stream serverStream
	var next func() *ownsandv1.ExecEvent
	if isExec {
		s, err := c.relay.Exec(actx, relayRequest(op.grant, payload.(*ownsandv1.ExecRequest)))
		if err != nil {
			at.err = streamErr(actx, err, op.rec)
			return at
		}
		stream, next = s, func() *ownsandv1.ExecEvent { return s.Msg().GetEvent() }
	} else {
		s, err := c.relay.AttachExec(actx, relayRequest(op.grant, payload.(*ownsandv1.AttachExecRequest)))
		if err != nil {
			at.err = streamErr(actx, err, op.rec)
			return at
		}
		stream, next = s, func() *ownsandv1.ExecEvent { return s.Msg().GetEvent() }
	}
	defer stream.Close()
	for stream.Receive() {
		done := op.handleEvent(next(), &at, ownsandv1.DataPathTransport_DATA_PATH_TRANSPORT_RELAY)
		if at.accepted || !op.rec.inRecovery() {
			// The agent accepted the continuation, or progress ended the
			// window: the recovery budget no longer bounds the stream.
			disarm()
		}
		if done {
			return at
		}
	}
	if err := stream.Err(); err != nil {
		at.err = streamErr(actx, err, op.rec)
		return at
	}
	at.err = io.EOF
	return at
}

func guestExecTime(ts *timestamppb.Timestamp) *time.Time {
	if !ts.IsValid() {
		return nil
	}
	t := ts.AsTime()
	return &t
}

// handleEvent applies one exec event and reports whether the stream is done.
func (op *execOp) handleEvent(ev *ownsandv1.ExecEvent, at *attempt, transport ownsandv1.DataPathTransport) bool {
	emit := func(e ExecEvent) bool {
		if op.onEvent == nil {
			return true
		}
		e.ExecID = op.execID
		if err := op.onEvent(e); err != nil {
			at.err = &callbackError{err: err}
			return false
		}
		return true
	}
	switch e := ev.GetEvent().(type) {
	case *ownsandv1.ExecEvent_Accepted:
		at.accepted = true
		op.rec.pauseClock()
	case *ownsandv1.ExecEvent_Keepalive:
	case *ownsandv1.ExecEvent_Started:
		if !op.started {
			op.started = true
			if !emit(ExecEvent{Kind: ExecEventStarted, StartTime: guestExecTime(e.Started.GetStartTime())}) {
				return true
			}
		}
	case *ownsandv1.ExecEvent_OutputTruncated:
		op.started = true
		var ev ExecEvent
		ev.Kind = ExecEventOutputTruncated
		if o := e.OutputTruncated.GetStdoutOffset(); o > op.outOff {
			ev.MissingStdout = ByteRange{From: op.outOff, To: o}
			op.outOff = o
		}
		if o := e.OutputTruncated.GetStderrOffset(); o > op.errOff {
			ev.MissingStderr = ByteRange{From: op.errOff, To: o}
			op.errOff = o
		}
		if !ev.MissingStdout.Empty() || !ev.MissingStderr.Empty() {
			op.incomplete = true
			if !emit(ev) {
				return true
			}
		}
	case *ownsandv1.ExecEvent_Stdout:
		op.started = true
		if !op.deliver(ExecEventStdout, &op.outOff, e.Stdout, at, emit) {
			return true
		}
	case *ownsandv1.ExecEvent_Stderr:
		op.started = true
		if !op.deliver(ExecEventStderr, &op.errOff, e.Stderr, at, emit) {
			return true
		}
	case *ownsandv1.ExecEvent_Exited:
		op.rec.reset()
		r := &ExecResult{
			ExecID: op.execID, Kind: ExecResultExited, TimedOut: e.Exited.GetTimedOut(),
			EndTime: guestExecTime(e.Exited.GetEndTime()), Transport: transport,
		}
		switch st := e.Exited.GetStatus().(type) {
		case *ownsandv1.ExecExited_ExitCode:
			code := st.ExitCode
			r.ExitCode = &code
		case *ownsandv1.ExecExited_Signal:
			sig := st.Signal
			r.Signal = &sig
		}
		at.result = r
		return true
	case *ownsandv1.ExecEvent_Failed:
		op.rec.reset()
		at.result = &ExecResult{ExecID: op.execID, Kind: ExecResultFailed, FailureReason: e.Failed.GetReason(), FailureMessage: e.Failed.GetMessage(), Transport: transport}
		return true
	case *ownsandv1.ExecEvent_SandboxEnded:
		op.rec.reset()
		at.result = &ExecResult{ExecID: op.execID, Kind: ExecResultSandboxEnded, EndReason: e.SandboxEnded.GetReason(), Transport: transport}
		return true
	case *ownsandv1.ExecEvent_Paused:
		at.paused = true
		return true
	}
	return false
}

// deliver passes the new bytes of one output chunk to the caller and
// advances the offset. Bytes before the offset were delivered already; a
// gap after it is lost output.
func (op *execOp) deliver(kind ExecEventKind, cur *int64, out *ownsandv1.ExecOutput, at *attempt, emit func(ExecEvent) bool) bool {
	off, data := out.GetOffset(), out.GetData()
	end := off + int64(len(data))
	if end <= *cur {
		return true
	}
	if off > *cur {
		ev := ExecEvent{Kind: ExecEventOutputTruncated}
		if kind == ExecEventStdout {
			ev.MissingStdout = ByteRange{From: *cur, To: off}
		} else {
			ev.MissingStderr = ByteRange{From: *cur, To: off}
		}
		op.incomplete = true
		*cur = off
		if !emit(ev) {
			return false
		}
	}
	data = data[*cur-off:]
	*cur = end
	at.data = true
	op.rec.reset()
	return emit(ExecEvent{Kind: kind, Offset: end - int64(len(data)), Data: data})
}
