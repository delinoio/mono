package ownsand

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/delinoio/mono/packages/ownsand-sdk-go/clock"
	"github.com/delinoio/mono/packages/ownsand-sdk-go/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
)

// ExecOptions describe a command. Stdin is not part of v1.
type ExecOptions struct {
	// Command is the argument array; the first element is the program. A
	// shell runs only when the caller passes one.
	Command          []string
	Env              map[string]string
	WorkingDirectory string
	// Timeout stops the command when it passes; zero means no timeout.
	Timeout time.Duration
}

// ExecEventKind is the kind of an ExecEvent.
type ExecEventKind int

const (
	// ExecEventStarted reports that the command started. The SDK reports it
	// once per exec, also after continuations.
	ExecEventStarted ExecEventKind = iota + 1
	// ExecEventStdout carries a stdout chunk.
	ExecEventStdout
	// ExecEventStderr carries a stderr chunk.
	ExecEventStderr
	// ExecEventOutputTruncated reports output that the SDK could not
	// receive. The result is then incomplete.
	ExecEventOutputTruncated
	// ExecEventExited carries the exit code or the signal.
	ExecEventExited
	// ExecEventFailed reports a command that could not start.
	ExecEventFailed
	// ExecEventSandboxEnded reports that the sandbox ended before the result.
	ExecEventSandboxEnded
)

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
	case ExecEventExited:
		return "exited"
	case ExecEventFailed:
		return "failed"
	case ExecEventSandboxEnded:
		return "sandbox_ended"
	}
	return "unknown"
}

// OutputGap is a byte range [From, To) of one output stream that the SDK did
// not receive.
type OutputGap struct {
	Stream   ownsandv1.OutputStream
	From, To int64
}

// ExecEvent is one event of an exec. The SDK consumes the relay accepted
// and keepalive events and the repeated started events internally.
type ExecEvent struct {
	Kind ExecEventKind
	// Offset and Data are set for stdout and stderr chunks.
	Offset int64
	Data   []byte
	// Gaps is set for ExecEventOutputTruncated.
	Gaps []OutputGap
	// ExitCode or Signal is set for ExecEventExited.
	ExitCode *int64
	Signal   *int32
	TimedOut bool
	// Reason is set for ExecEventFailed.
	Reason string
	// EndReason is set for ExecEventSandboxEnded.
	EndReason ownsandv1.SandboxEndReason
}

// ExecOutcome is the kind of an exec result.
type ExecOutcome int

const (
	// ExecExited means the command ended with an exit code or a signal.
	ExecExited ExecOutcome = iota + 1
	// ExecFailed means the command could not start.
	ExecFailed
	// ExecSandboxEnded means the sandbox ended before the result.
	ExecSandboxEnded
)

// ExecResult is the result of an exec.
type ExecResult struct {
	ExecID   string
	Outcome  ExecOutcome
	ExitCode *int64
	Signal   *int32
	TimedOut bool
	// FailureReason is set for ExecFailed.
	FailureReason string
	// EndReason is set for ExecSandboxEnded.
	EndReason ownsandv1.SandboxEndReason
	// Incomplete is true when output was lost (output_truncated). The SDK
	// never presents truncated output as complete.
	Incomplete bool
	// Transport is the transport that delivered the result.
	Transport ownsandv1.DataPathTransport
}

// ExecStream is a running exec. Read its events with Recv until io.EOF, then
// call Result. A data path failure is returned as a *DataPathError, never
// as an exit code.
type ExecStream struct {
	execID string
	events chan *ExecEvent
	done   chan struct{}
	cancel context.CancelFunc

	mu     sync.Mutex
	result *ExecResult
	err    error
}

// ExecID returns the caller-generated exec ID.
func (s *ExecStream) ExecID() string { return s.execID }

// Recv returns the next event, or io.EOF after the last one, or the data
// path error.
func (s *ExecStream) Recv() (*ExecEvent, error) {
	ev, ok := <-s.events
	if ok {
		return ev, nil
	}
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	return nil, io.EOF
}

// Wait drains the remaining events and returns the result.
func (s *ExecStream) Wait() (*ExecResult, error) {
	for range s.events {
	}
	return s.Result()
}

// Result returns the result after Recv returned io.EOF.
func (s *ExecStream) Result() (*ExecResult, error) {
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.result, s.err
}

// Close stops reading the exec. It does not stop the command.
func (s *ExecStream) Close() {
	s.cancel()
	for range s.events {
	}
}

// Exec runs a command in the sandbox. The SDK generates the exec ID before
// it sends the request and never runs the command twice: it continues an
// interrupted exec with AttachExec.
func (s *Sandbox) Exec(ctx context.Context, o ExecOptions) (*ExecStream, error) {
	if len(o.Command) == 0 {
		return nil, invalid("command is required")
	}
	if o.Timeout < 0 {
		return nil, invalid("timeout must not be negative")
	}
	fields := &ownsandv1.ExecRequest{Command: o.Command, Env: o.Env, WorkingDirectory: o.WorkingDirectory}
	if proto.Size(fields) > datapath.MaxExecRequestFields {
		return nil, invalid("command, env and working directory exceed 256 KiB")
	}
	req := &ownsandv1.ExecRequest{
		OrganizationId:   s.org,
		SandboxId:        s.id,
		ExecId:           datapath.NewUUIDv7At(s.c.clk.Now()).String(),
		Command:          append([]string(nil), o.Command...),
		Env:              o.Env,
		WorkingDirectory: o.WorkingDirectory,
		BindingSalt:      datapath.NewBindingSalt(),
	}
	if o.Timeout > 0 {
		req.Timeout = durationpb.New(o.Timeout)
	}
	op := &execOp{s: s, execID: req.GetExecId(), req: req}
	op.auth = &opAuth{dp: s.dp, kind: ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC, opID: req.GetExecId(), digest: datapath.ExecDigest(req)}
	return op.start(ctx), nil
}

// AttachExec continues the events of an exec that this program did not
// start (or whose request it no longer has) from offset zero. It uses an
// ATTACH_EXEC relay grant without a request digest.
func (s *Sandbox) AttachExec(ctx context.Context, execID string) (*ExecStream, error) {
	if !datapath.IsUUIDv7(execID) {
		return nil, invalid("exec ID must be a UUID v7")
	}
	op := &execOp{s: s, execID: execID, sentFresh: true}
	op.auth = &opAuth{dp: s.dp, kind: ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_ATTACH_EXEC, opID: execID}
	return op.start(ctx), nil
}

type execOp struct {
	s      *Sandbox
	execID string
	// req is nil for an attach-only caller.
	req  *ownsandv1.ExecRequest
	auth *opAuth

	// sentFresh is true after the first ExecRequest was sent.
	sentFresh bool
	// needResend is true after AttachExec failed with not_found before started.
	needResend bool
	started    bool
	off        [3]int64 // indexed by OutputStream
	incomplete bool
	attempts   int
	// resultTemplate is the result of the final event.
	resultTemplate ExecResult

	stream *ExecStream
	ctx    context.Context
}

func (op *execOp) start(ctx context.Context) *ExecStream {
	ctx, cancel := context.WithCancel(ctx)
	st := &ExecStream{execID: op.execID, events: make(chan *ExecEvent, 16), done: make(chan struct{}), cancel: cancel}
	op.stream = st
	op.ctx = ctx
	go func() {
		res, err := op.run(ctx)
		st.mu.Lock()
		st.result = res
		if err != nil {
			st.err = op.s.c.redact(err)
		}
		st.mu.Unlock()
		close(st.events)
		close(st.done)
		cancel()
	}()
	return st
}

func (op *execOp) emit(ev *ExecEvent) error {
	select {
	case op.stream.events <- ev:
		return nil
	case <-op.ctx.Done():
		return op.ctx.Err()
	}
}

func (op *execOp) dpErr(err error, unknown bool) error {
	var de *DataPathError
	if errors.As(err, &de) {
		return err
	}
	return &DataPathError{Op: "exec", Err: err, OutcomeUnknown: unknown}
}

// attemptOutcome is how one stream of an operation ended.
type attemptOutcome struct {
	accepted  bool
	delivered bool
	final     bool
	paused    bool
	err       error
	attempt   context.Context
	transport ownsandv1.DataPathTransport
}

// requestKind is the next request of an exec.
type requestKind int

const (
	reqExec requestKind = iota
	reqAttach
)

func (op *execOp) nextKind() requestKind {
	if op.req != nil && (!op.sentFresh || op.needResend) {
		return reqExec
	}
	return reqAttach
}

func (op *execOp) attachRequest() *ownsandv1.AttachExecRequest {
	return &ownsandv1.AttachExecRequest{
		OrganizationId: op.s.org,
		SandboxId:      op.s.id,
		ExecId:         op.execID,
		StdoutOffset:   op.off[ownsandv1.OutputStream_OUTPUT_STREAM_STDOUT],
		StderrOffset:   op.off[ownsandv1.OutputStream_OUTPUT_STREAM_STDERR],
	}
}

func (op *execOp) run(ctx context.Context) (*ExecResult, error) {
	c := op.s.c
	w := newWindow(c.clk)
	b := newBackoff()
	for {
		if ctx.Err() != nil {
			return nil, op.dpErr(ctx.Err(), op.started)
		}
		if !w.active {
			b = newBackoff()
		}
		kind := op.nextKind()
		if kind == reqExec && op.sentFresh {
			// A resend of the identical ExecRequest is safe only while the
			// agent still accepts the exec ID.
			id, _ := datapath.ParseUUIDv7(op.execID)
			if datapath.ClassifyOperationID(id, c.clk.Now()) != datapath.OperationIDFresh {
				return nil, op.dpErr(errors.New("the exec ID expired before the command started"), true)
			}
		}
		sess, err := op.s.dp.session(ctx)
		if err != nil {
			return nil, op.dpErr(err, op.sentFresh)
		}
		var out attemptOutcome
		if sess != nil {
			out = op.p2pAttempt(ctx, sess, kind, w)
		} else {
			if err := op.auth.ensure(ctx, serverStream, w); err != nil {
				return nil, op.dpErr(err, op.sentFresh)
			}
			out = op.relayAttempt(ctx, kind, w)
			op.auth.streamEnded()
		}
		op.attempts++
		if kind == reqExec {
			op.sentFresh = true
			op.needResend = false
		}
		if out.final {
			return op.finalResult(out.transport), nil
		}
		if out.paused {
			w.end()
			if done, err := op.waitResume(ctx); err != nil || done {
				if err != nil {
					return nil, op.dpErr(err, true)
				}
				return op.finalResult(op.s.dp.current()), nil
			}
			continue
		}
		if sess != nil {
			if handled := op.s.dp.handleSessionError(sess, out.err); handled {
				continue
			}
		}
		if out.err == nil {
			if out.accepted || sess != nil {
				// A routine stream rotation: continue at once.
				w.end()
				continue
			}
			// A stream that ends before the agent accepted it is a failure,
			// not a rotation.
			out.err = connect.NewError(connect.CodeUnavailable, errors.New("relay stream ended before the agent accepted it"))
			out.attempt = nil
		}
		f := classifyAttempt(ctx, out.attempt, out.err)
		if f.remote && f.code == connect.CodeNotFound && kind == reqAttach {
			if !op.started && op.req != nil {
				op.needResend = true
				continue
			}
			return nil, op.dpErr(out.err, true)
		}
		if f.remote && f.code == connect.CodeFailedPrecondition && op.attempts > 1 {
			info, gerr := op.s.Refresh(ctx)
			if gerr == nil && info.GetState() == ownsandv1.SandboxState_SANDBOX_STATE_PAUSED {
				w.end()
				done, err := op.waitResume(ctx)
				if err != nil {
					return nil, op.dpErr(err, true)
				}
				if done {
					return op.finalResult(op.s.dp.current()), nil
				}
				continue
			}
			if gerr == nil && isEnded(info.GetState()) {
				if err := op.sandboxEnded(info.GetEndReason()); err != nil {
					return nil, op.dpErr(err, true)
				}
				return op.finalResult(op.s.dp.current()), nil
			}
		}
		if !f.retryable(false) {
			return nil, op.dpErr(out.err, op.started || (op.sentFresh && kind == reqAttach))
		}
		c.log.Debug("exec continuation scheduled", slog.String("sandbox_id", op.s.id), slog.String("exec_id", op.execID), slog.String("code", f.code.String()), slog.Bool("transport_interruption", f.transport))
		if sess == nil {
			op.auth.discard()
		}
		w.start()
		if w.expired() {
			return nil, op.dpErr(out.err, op.started)
		}
		ok, serr := w.waitRetry(ctx, b, f)
		if serr != nil {
			return nil, op.dpErr(serr, op.started)
		}
		if !ok {
			return nil, op.dpErr(out.err, op.started)
		}
	}
}

func isEnded(s ownsandv1.SandboxState) bool {
	return s == ownsandv1.SandboxState_SANDBOX_STATE_DELETED || s == ownsandv1.SandboxState_SANDBOX_STATE_FAILED
}

// waitResume watches the sandbox until it resumes (false), ends (true, with
// the sandbox_ended result emitted) or the caller cancels.
func (op *execOp) waitResume(ctx context.Context) (bool, error) {
	reason, err := op.s.waitRunning(ctx)
	if err != nil {
		return false, err
	}
	if reason == ownsandv1.SandboxEndReason_SANDBOX_END_REASON_UNSPECIFIED {
		return false, nil
	}
	return true, op.sandboxEnded(reason)
}

// waitRunning polls the sandbox until it is RUNNING (it returns UNSPECIFIED)
// or ended (it returns the end reason).
func (s *Sandbox) waitRunning(ctx context.Context) (ownsandv1.SandboxEndReason, error) {
	b := &backoff{cur: 500 * time.Millisecond}
	for {
		info, err := s.Refresh(ctx)
		if err != nil {
			f := classify(ctx, err)
			if !f.retryable(false) {
				return 0, err
			}
		} else {
			switch {
			case info.GetState() == ownsandv1.SandboxState_SANDBOX_STATE_RUNNING:
				return ownsandv1.SandboxEndReason_SANDBOX_END_REASON_UNSPECIFIED, nil
			case isEnded(info.GetState()):
				r := info.GetEndReason()
				if r == ownsandv1.SandboxEndReason_SANDBOX_END_REASON_UNSPECIFIED {
					r = ownsandv1.SandboxEndReason_SANDBOX_END_REASON_LOST_ON_DEVICE
				}
				return r, nil
			}
		}
		if err := clock.Sleep(ctx, s.c.clk, b.next(0)); err != nil {
			return 0, err
		}
	}
}

func (op *execOp) sandboxEnded(reason ownsandv1.SandboxEndReason) error {
	return op.handleEvent(&ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_SandboxEnded_{SandboxEnded: &ownsandv1.ExecEvent_SandboxEnded{Reason: reason}}}, newWindow(op.s.c.clk), &attemptOutcome{})
}

func (op *execOp) finalResult(t ownsandv1.DataPathTransport) *ExecResult {
	r := op.resultTemplate
	r.ExecID = op.execID
	r.Incomplete = op.incomplete
	r.Transport = t
	return &r
}

// handleEvent applies one agent event: it hides accepted, keepalive and a
// repeated started, trims output that the caller already received, and
// reports truncation.
func (op *execOp) handleEvent(ev *ownsandv1.ExecEvent, w *window, out *attemptOutcome) error {
	switch e := ev.GetEvent().(type) {
	case *ownsandv1.ExecEvent_Accepted_:
		out.accepted = true
		w.pause()
	case *ownsandv1.ExecEvent_Keepalive_:
	case *ownsandv1.ExecEvent_Started_:
		op.needResend = false
		if !op.started {
			op.started = true
			return op.emit(&ExecEvent{Kind: ExecEventStarted})
		}
	case *ownsandv1.ExecEvent_OutputTruncated_:
		gaps := op.gapTo(ownsandv1.OutputStream_OUTPUT_STREAM_STDOUT, e.OutputTruncated.GetFirstStdoutOffset(), nil)
		gaps = op.gapTo(ownsandv1.OutputStream_OUTPUT_STREAM_STDERR, e.OutputTruncated.GetFirstStderrOffset(), gaps)
		out.delivered = true
		w.end()
		if len(gaps) > 0 {
			op.incomplete = true
			return op.emit(&ExecEvent{Kind: ExecEventOutputTruncated, Gaps: gaps})
		}
	case *ownsandv1.ExecEvent_Output_:
		s := e.Output.GetStream()
		if s != ownsandv1.OutputStream_OUTPUT_STREAM_STDOUT && s != ownsandv1.OutputStream_OUTPUT_STREAM_STDERR {
			return nil
		}
		out.delivered = true
		w.end()
		data, at := e.Output.GetData(), e.Output.GetOffset()
		if at > op.off[s] {
			// Output between the received offset and this chunk is missing.
			gaps := op.gapTo(s, at, nil)
			op.incomplete = true
			if err := op.emit(&ExecEvent{Kind: ExecEventOutputTruncated, Gaps: gaps}); err != nil {
				return err
			}
		}
		if skip := op.off[s] - at; skip > 0 {
			if skip >= int64(len(data)) {
				return nil
			}
			data, at = data[skip:], at+skip
		}
		op.off[s] = at + int64(len(data))
		kind := ExecEventStdout
		if s == ownsandv1.OutputStream_OUTPUT_STREAM_STDERR {
			kind = ExecEventStderr
		}
		if len(data) == 0 {
			return nil
		}
		return op.emit(&ExecEvent{Kind: kind, Offset: at, Data: data})
	case *ownsandv1.ExecEvent_Exited_:
		out.final = true
		w.end()
		x := e.Exited
		r := ExecResult{Outcome: ExecExited, TimedOut: x.GetTimedOut()}
		ev := &ExecEvent{Kind: ExecEventExited, TimedOut: x.GetTimedOut()}
		switch v := x.GetResult().(type) {
		case *ownsandv1.ExecEvent_Exited_ExitCode:
			code := v.ExitCode
			r.ExitCode, ev.ExitCode = &code, &code
		case *ownsandv1.ExecEvent_Exited_Signal:
			sig := v.Signal
			r.Signal, ev.Signal = &sig, &sig
		}
		op.resultTemplate = r
		return op.emit(ev)
	case *ownsandv1.ExecEvent_Failed_:
		out.final = true
		w.end()
		op.resultTemplate = ExecResult{Outcome: ExecFailed, FailureReason: e.Failed.GetReason()}
		return op.emit(&ExecEvent{Kind: ExecEventFailed, Reason: e.Failed.GetReason()})
	case *ownsandv1.ExecEvent_SandboxEnded_:
		out.final = true
		w.end()
		op.resultTemplate = ExecResult{Outcome: ExecSandboxEnded, EndReason: e.SandboxEnded.GetReason()}
		return op.emit(&ExecEvent{Kind: ExecEventSandboxEnded, EndReason: e.SandboxEnded.GetReason()})
	case *ownsandv1.ExecEvent_Paused_:
		out.paused = true
	}
	return nil
}

// gapTo advances the stream offset to first and records the skipped range.
func (op *execOp) gapTo(s ownsandv1.OutputStream, first int64, gaps []OutputGap) []OutputGap {
	if first > op.off[s] {
		gaps = append(gaps, OutputGap{Stream: s, From: op.off[s], To: first})
		op.off[s] = first
	}
	return gaps
}

// relayAttempt runs one relay stream of the exec.
func (op *execOp) relayAttempt(ctx context.Context, kind requestKind, w *window) attemptOutcome {
	c := op.s.c
	actx, cancel := watchIdle(ctx, c.clk)
	defer cancel(nil)
	out := attemptOutcome{attempt: actx, transport: ownsandv1.DataPathTransport_DATA_PATH_TRANSPORT_RELAY}
	var stream *connect.ServerStreamForClient[ownsandv1.ExecEvent]
	var err error
	if kind == reqExec {
		req := connect.NewRequest(op.req)
		req.Header().Set(datapath.RelayGrantHeader, op.auth.g.id)
		stream, err = c.relay.Exec(actx, req)
	} else {
		req := connect.NewRequest(op.attachRequest())
		req.Header().Set(datapath.RelayGrantHeader, op.auth.g.id)
		stream, err = c.relay.AttachExec(actx, req)
	}
	c.log.Debug("relay exec stream", slog.String("sandbox_id", op.s.id), slog.String("exec_id", op.execID), slog.Bool("attach", kind == reqAttach), slog.String("grant_id", op.auth.g.id))
	if err != nil {
		out.err = err
		return out
	}
	defer stream.Close()
	for stream.Receive() {
		if herr := op.handleEvent(stream.Msg(), w, &out); herr != nil {
			out.err = herr
			return out
		}
		if out.final || out.paused {
			return out
		}
	}
	out.err = stream.Err()
	return out
}

// p2pAttempt runs one data channel request of the exec.
func (op *execOp) p2pAttempt(ctx context.Context, sess *p2pSession, kind requestKind, w *window) attemptOutcome {
	out := attemptOutcome{transport: ownsandv1.DataPathTransport_DATA_PATH_TRANSPORT_PEER_TO_PEER}
	var st *p2pStream
	var err error
	if kind == reqExec {
		st, err = sess.open(func(f *ownsandv1.DataFrame) {
			f.Payload = &ownsandv1.DataFrame_ExecRequest{ExecRequest: op.req}
		})
	} else {
		st, err = sess.open(func(f *ownsandv1.DataFrame) {
			f.Payload = &ownsandv1.DataFrame_ExecAttachRequest{ExecAttachRequest: op.attachRequest()}
		})
	}
	if err != nil {
		out.err = err
		return out
	}
	defer st.close()
	for {
		f, err := st.recv(ctx)
		if err != nil {
			out.err = err
			return out
		}
		switch p := f.GetPayload().(type) {
		case *ownsandv1.DataFrame_ExecEvent:
			if herr := op.handleEvent(p.ExecEvent, w, &out); herr != nil {
				out.err = herr
				return out
			}
			if out.final || out.paused {
				return out
			}
		case *ownsandv1.DataFrame_Error:
			out.err = frameError(p.Error)
			return out
		}
	}
}
