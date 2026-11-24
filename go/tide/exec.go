// SPDX-License-Identifier: Apache-2.0

package tide

import (
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/delinoio/mono/go/uuidv7"
	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
)

// ExecEventKind is the kind of an exec event that the SDK reports.
// Keepalive events and the started event that a continuation repeats never
// reach the caller.
type ExecEventKind int

const (
	ExecEventStarted ExecEventKind = iota + 1
	ExecEventStdout
	ExecEventStderr
	// ExecEventOutputTruncated reports output that the caller did not
	// receive: the server no longer keeps it (an output_truncated event) or
	// a stream skipped it. The result is then marked incomplete.
	ExecEventOutputTruncated
)

// String returns the stable identifier of k.
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
	From int64 `json:"from"`
	To   int64 `json:"to"`
}

// Empty reports whether the range holds no byte.
func (r ByteRange) Empty() bool { return r.To <= r.From }

// ExecEvent is one exec event.
type ExecEvent struct {
	Kind   ExecEventKind
	ExecID string
	// Offset and Data of a stdout or stderr chunk. Data is guest content:
	// never log it.
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

// ExecResult is the final result of an exec.
type ExecResult struct {
	ExecID string
	Kind   ExecResultKind
	// ExitCode or Signal is set for ExecResultExited.
	ExitCode *int64
	Signal   *int32
	// TimedOut is true when the exec timeout stopped the command.
	TimedOut bool
	// FailureReason and FailureMessage are set for ExecResultFailed.
	FailureReason  tidev1.ExecFailureReason
	FailureMessage string
	// SandboxEndReason is the OwnSand end reason of ExecResultSandboxEnded.
	SandboxEndReason string
	// Incomplete is true when output was lost; the output that the caller
	// received is not the complete output.
	Incomplete bool
	// StdoutOffset and StderrOffset are the offsets after the last byte
	// that the caller received.
	StdoutOffset int64
	StderrOffset int64
}

// ExecParams describes a command.
type ExecParams struct {
	OrganizationID string
	DevSessionID   string
	// ExecID is the exec_id (a UUID v7). Empty generates one.
	ExecID string
	// Command is the argument array; the first element is the program.
	Command []string
	// Env is added to the command's environment. Never logged.
	Env map[string]string
	// WorkingDirectory is an absolute guest path or a path relative to the
	// workspace path. Empty means the workspace path.
	WorkingDirectory string
	// Timeout stops the command; zero means no timeout.
	Timeout time.Duration
	// OnEvent receives the events in order. An error from it ends the
	// stream (the command keeps running in the guest) and Exec returns that
	// error inside an *ExecStreamError.
	OnEvent func(ExecEvent) error
}

// AttachParams describes a continuation of an exec from given offsets.
type AttachParams struct {
	OrganizationID string
	DevSessionID   string
	ExecID         string
	StdoutOffset   int64
	StderrOffset   int64
	OnEvent        func(ExecEvent) error
}

// ExecStreamError reports that the SDK could not deliver the result of an
// exec whose request it sent: ctx ended, OnEvent failed, the server
// rejected a continuation, or transient failures lasted longer than the
// retry budget. The command may still run in the guest; StdoutOffset and
// StderrOffset tell a later AttachExec where to continue.
type ExecStreamError struct {
	ExecID       string
	StdoutOffset int64
	StderrOffset int64
	// Started is true when the SDK received the started event.
	Started bool
	// OutcomeUnknown is true when the server does not know the exec
	// (AttachDevSessionExec returned not_found) and the SDK cannot tell
	// whether it ran.
	OutcomeUnknown bool
	Err            error
}

func (e *ExecStreamError) Error() string {
	msg := "tide: exec stream ended before the result for exec " + e.ExecID
	if e.OutcomeUnknown {
		msg += " (outcome unknown)"
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *ExecStreamError) Unwrap() error { return e.Err }

// Exec runs a command in a running dev session and reports its events to
// p.OnEvent. The SDK generates the exec_id when p.ExecID is empty, never
// sends ExecDevSession twice, and continues with AttachDevSessionExec from
// the last offsets after each stream end without a final event (the server
// ends every stream before 14 minutes) and after transient failures, with
// exponential backoff. Output that the continuation can no longer read is
// reported with an ExecEventOutputTruncated event and marks the result
// incomplete.
//
// For stdout and stderr, a nil callback or a nil return from the callback
// acknowledges the entire data range. If the callback returns an error, the
// ExecStreamError offset for that stream points to the first byte offered
// to the callback. AttachExec can replay that range while it is retained.
// A sink that saves a prefix before returning an error can receive that
// prefix again. To avoid duplicate writes, use atomic chunk writes or track
// durable bytes by stream and offset.
//
// A server error of the ExecDevSession call itself (for example
// failed_precondition for a dev session that is not RUNNING) is returned
// unchanged: the command did not run. Failures after the request was sent
// are returned as *ExecStreamError.
func (c *Client) Exec(ctx context.Context, p ExecParams) (*ExecResult, error) {
	execID := p.ExecID
	if execID == "" {
		id, err := uuidv7.NewString()
		if err != nil {
			return nil, err
		}
		execID = id
	}
	if len(p.Command) == 0 {
		return nil, localError(connect.CodeInvalidArgument, errors.New("tide: exec needs a command"))
	}
	req := &tidev1.ExecDevSessionRequest{
		OrganizationId:   p.OrganizationID,
		DevSessionId:     p.DevSessionID,
		ExecId:           execID,
		Command:          p.Command,
		Env:              p.Env,
		WorkingDirectory: p.WorkingDirectory,
	}
	if p.Timeout > 0 {
		req.Timeout = durationpb.New(p.Timeout)
	}
	op := &execOp{c: c, org: p.OrganizationID, session: p.DevSessionID, execID: execID, onEvent: p.OnEvent, start: req}
	return op.run(ctx)
}

// AttachExec continues the event stream of an exec from the given offsets,
// with the same continuation rules as Exec. It does not report the started
// event again.
//
// The callback acknowledges stdout and stderr chunks as described in
// [Client.Exec]. A callback error returns an *ExecStreamError with the offset
// of the first byte in the rejected range, which a later AttachExec can replay.
func (c *Client) AttachExec(ctx context.Context, p AttachParams) (*ExecResult, error) {
	op := &execOp{
		c: c, org: p.OrganizationID, session: p.DevSessionID, execID: p.ExecID, onEvent: p.OnEvent,
		stdout: p.StdoutOffset, stderr: p.StderrOffset, started: true,
	}
	return op.run(ctx)
}

type execOp struct {
	c                   *Client
	org, session        string
	execID              string
	onEvent             func(ExecEvent) error
	start               *tidev1.ExecDevSessionRequest // nil after the first call
	stdout, stderr      int64
	started, incomplete bool
}

func (o *execOp) emit(ev ExecEvent) error {
	if o.onEvent == nil {
		return nil
	}
	ev.ExecID = o.execID
	if err := o.onEvent(ev); err != nil {
		return &handlerError{err: err}
	}
	return nil
}

func (o *execOp) streamErr(err error, unknown bool) error {
	var he *handlerError
	if errors.As(err, &he) {
		err = he.err
	}
	return &ExecStreamError{
		ExecID: o.execID, StdoutOffset: o.stdout, StderrOffset: o.stderr,
		Started: o.started, OutcomeUnknown: unknown, Err: err,
	}
}

// eventStream is the common shape of the exec and attach streams.
type eventStream interface {
	Receive() bool
	Err() error
	Close() error
}

type execStream struct {
	*connect.ServerStreamForClient[tidev1.ExecDevSessionResponse]
}

func (s execStream) event() *tidev1.DevSessionExecEvent { return s.Msg().GetEvent() }

type attachStream struct {
	*connect.ServerStreamForClient[tidev1.AttachDevSessionExecResponse]
}

func (s attachStream) event() *tidev1.DevSessionExecEvent { return s.Msg().GetEvent() }

func (o *execOp) run(ctx context.Context) (*ExecResult, error) {
	bo := backoff{min: o.c.timing.backoffMin, max: o.c.timing.backoffMax}
	lastProgress := time.Now()
	for attempt := 0; ; attempt++ {
		var (
			stream eventStream
			next   func() *tidev1.DevSessionExecEvent
			err    error
		)
		first := o.start != nil
		if first {
			var s *connect.ServerStreamForClient[tidev1.ExecDevSessionResponse]
			s, err = o.c.devSessions.ExecDevSession(ctx, connect.NewRequest(o.start))
			if s != nil {
				es := execStream{s}
				stream, next = es, es.event
			}
		} else {
			var s *connect.ServerStreamForClient[tidev1.AttachDevSessionExecResponse]
			s, err = o.c.devSessions.AttachDevSessionExec(ctx, connect.NewRequest(&tidev1.AttachDevSessionExecRequest{
				OrganizationId: o.org, DevSessionId: o.session, ExecId: o.execID,
				StdoutOffset: o.stdout, StderrOffset: o.stderr,
			}))
			if s != nil {
				as := attachStream{s}
				stream, next = as, as.event
			}
		}
		progressed := false
		var res *ExecResult
		if err == nil {
			for stream.Receive() {
				progressed = true
				r, herr := o.handle(next())
				if herr != nil {
					_ = stream.Close()
					return nil, o.streamErr(herr, false)
				}
				if r != nil {
					res = r
					break
				}
			}
			if res == nil {
				err = stream.Err()
			}
			_ = stream.Close()
		}
		if res != nil {
			return res, nil
		}
		if first {
			// Only an explicit server status before any event proves that
			// the command did not run. Any other failure continues with
			// AttachDevSessionExec, so the command never runs twice.
			if err != nil && !progressed && connect.IsWireError(err) && connect.CodeOf(err) != connect.CodeUnavailable {
				return nil, err
			}
			o.start = nil
		}
		if ctx.Err() != nil {
			return nil, o.streamErr(ctx.Err(), false)
		}
		if progressed {
			lastProgress = time.Now()
			bo.reset()
		}
		if err == nil {
			// The server ended the stream without a final event: continue
			// at once after a stream that delivered events.
			if progressed {
				o.c.logger.DebugContext(ctx, "tide exec stream rotated", "exec_id", o.execID)
				continue
			}
		} else if !transient(ctx, err) {
			unknown := connect.CodeOf(err) == connect.CodeNotFound && !o.started
			return nil, o.streamErr(err, unknown)
		}
		if time.Since(lastProgress) > o.c.timing.retryBudget {
			if err == nil {
				err = errors.New("the exec streams ended repeatedly without events")
			}
			return nil, o.streamErr(err, false)
		}
		delay := bo.next()
		o.c.logger.DebugContext(ctx, "tide exec continues", "exec_id", o.execID, "attempt", attempt+1, "delay", delay, "code", codeText(err))
		if serr := sleep(ctx, delay); serr != nil {
			return nil, o.streamErr(serr, false)
		}
	}
}

// handle processes one event and returns the result for a final event.
func (o *execOp) handle(ev *tidev1.DevSessionExecEvent) (*ExecResult, error) {
	switch {
	case ev.GetKeepalive() != nil:
		return nil, nil
	case ev.GetStarted() != nil:
		if o.started {
			return nil, nil
		}
		o.started = true
		return nil, o.emit(ExecEvent{Kind: ExecEventStarted})
	case ev.GetOutputTruncated() != nil:
		t := ev.GetOutputTruncated()
		missOut := ByteRange{From: o.stdout, To: t.GetStdoutOffset()}
		missErr := ByteRange{From: o.stderr, To: t.GetStderrOffset()}
		if missOut.Empty() && missErr.Empty() {
			return nil, nil
		}
		return nil, o.lose(missOut, missErr)
	case ev.GetStdout() != nil:
		return nil, o.output(ExecEventStdout, &o.stdout, ev.GetStdout())
	case ev.GetStderr() != nil:
		return nil, o.output(ExecEventStderr, &o.stderr, ev.GetStderr())
	case ev.GetExited() != nil:
		x := ev.GetExited()
		r := o.result(ExecResultExited)
		r.TimedOut = x.GetTimedOut()
		switch s := x.GetStatus().(type) {
		case *tidev1.DevSessionExecExited_ExitCode:
			v := s.ExitCode
			r.ExitCode = &v
		case *tidev1.DevSessionExecExited_Signal:
			v := s.Signal
			r.Signal = &v
		}
		return r, nil
	case ev.GetFailed() != nil:
		r := o.result(ExecResultFailed)
		r.FailureReason = ev.GetFailed().GetReason()
		r.FailureMessage = ev.GetFailed().GetMessage()
		return r, nil
	case ev.GetSandboxEnded() != nil:
		r := o.result(ExecResultSandboxEnded)
		r.SandboxEndReason = ev.GetSandboxEnded().GetReason()
		return r, nil
	}
	return nil, nil
}

func (o *execOp) lose(out, errRange ByteRange) error {
	o.incomplete = true
	if !out.Empty() {
		o.stdout = out.To
	} else {
		out = ByteRange{}
	}
	if !errRange.Empty() {
		o.stderr = errRange.To
	} else {
		errRange = ByteRange{}
	}
	return o.emit(ExecEvent{Kind: ExecEventOutputTruncated, MissingStdout: out, MissingStderr: errRange})
}

// output reports one chunk. It drops previously acknowledged bytes,
// reports skipped bytes as lost output, and advances past the chunk
// when the callback accepts it.
func (o *execOp) output(kind ExecEventKind, next *int64, ch *tidev1.DevSessionExecOutput) error {
	off, data := ch.GetOffset(), ch.GetData()
	if off > *next {
		missing := ByteRange{From: *next, To: off}
		var err error
		if kind == ExecEventStdout {
			err = o.lose(missing, ByteRange{})
		} else {
			err = o.lose(ByteRange{}, missing)
		}
		if err != nil {
			return err
		}
	}
	end := off + int64(len(data))
	if end <= *next {
		return nil
	}
	if off < *next {
		data = data[*next-off:]
		off = *next
	}
	if err := o.emit(ExecEvent{Kind: kind, Offset: off, Data: data}); err != nil {
		return err
	}
	*next = end
	return nil
}

func (o *execOp) result(kind ExecResultKind) *ExecResult {
	return &ExecResult{
		ExecID: o.execID, Kind: kind, Incomplete: o.incomplete,
		StdoutOffset: o.stdout, StderrOffset: o.stderr,
	}
}
