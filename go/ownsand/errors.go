// SPDX-License-Identifier: Apache-2.0

package ownsand

import (
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// DataPathError reports that the data path failed, so the SDK could not
// deliver the complete result of an operation. It is never an exit code: an
// exec that ends with a DataPathError may have run, and OutcomeUnknown says
// when the SDK cannot tell.
type DataPathError struct {
	// Operation is "exec", "attach-exec", "read-file" or "write-file".
	Operation string
	// OperationID is the exec_id, read_id or write_id.
	OperationID string
	// OutcomeUnknown is true when the exec result is no longer available
	// (for example AttachExec returned not_found after the exec started).
	OutcomeUnknown bool
	Err            error
}

func (e *DataPathError) Error() string {
	msg := fmt.Sprintf("ownsand: data path failed for %s %s", e.Operation, e.OperationID)
	if e.OutcomeUnknown {
		msg += " (outcome unknown)"
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *DataPathError) Unwrap() error { return e.Err }

// frameError is an error that the agent reported explicitly in an error
// frame on the peer-to-peer data channel. Like a Connect wire error it is a
// valid terminal status, not a transport interruption; the marker keeps
// that classification. It unwraps to a *connect.Error with the frame's code,
// message and ErrorDetail, so callers see the same public error shape on
// both transports: connect.CodeOf and (*connect.Error).Details work as on
// the relay.
type frameError struct {
	ce *connect.Error
}

func (e *frameError) Error() string { return e.ce.Error() }

func (e *frameError) Unwrap() error { return e.ce }

// errorFromDetail returns the error of an error frame.
func errorFromDetail(d *ownsandv1.ErrorDetail) error {
	code := connect.Code(d.GetCode())
	if d.GetCode() == ownsandv1.ErrorCode_ERROR_CODE_UNSPECIFIED {
		code = connect.CodeUnknown
	}
	ce := connect.NewError(code, errors.New(d.GetMessage()))
	if ed, err := connect.NewErrorDetail(d); err == nil {
		ce.AddDetail(ed)
	}
	return &frameError{ce: ce}
}

// errorDetail returns the ErrorDetail of err, if it carries one.
func errorDetail(err error) *ownsandv1.ErrorDetail {
	var ce *connect.Error
	if errors.As(err, &ce) {
		for _, d := range ce.Details() {
			v, derr := d.Value()
			if derr != nil {
				continue
			}
			if ed, ok := v.(*ownsandv1.ErrorDetail); ok {
				return ed
			}
		}
	}
	return nil
}

// failureKind classifies a failed data path attempt.
type failureKind int

const (
	// failTerminal ends the operation with the error.
	failTerminal failureKind = iota
	// failInterrupted is a transport interruption without a valid terminal
	// Connect status (for example a connection that closed during a
	// control plane restart, or a stream without its end-of-stream message).
	failInterrupted
	// failUnavailable is an explicit unavailable status, for example with
	// AGENT_CHANNEL_LOST or GRANT_INVALID.
	failUnavailable
	// failExhausted is an explicit resource_exhausted status.
	failExhausted
	// failDeadline is an explicit deadline_exceeded status.
	failDeadline
)

// errStall reports a server stream that received no byte for the stall
// timeout; the SDK cancels it and treats it as a transport interruption.
var errStall = errors.New("ownsand: relay stream received nothing for 90 seconds")

// errAttemptDeadline reports a WriteFile attempt that passed its SDK-side
// deadline; it is a transport interruption.
var errAttemptDeadline = errors.New("ownsand: WriteFile attempt passed its deadline")

// errAuthorizeDeadline reports an AuthorizeRelay attempt that got no
// response before its retry window ended; it is a transport interruption,
// so the window then ends the operation with it.
var errAuthorizeDeadline = errors.New("ownsand: AuthorizeRelay got no response within the recovery window")

// errCreateDeadline is the cancellation cause of a CreateSandbox retry that
// was still in flight when the retry window ended. CreateSandbox then returns
// the last failure of the window; the cause only tells the SDK deadline apart
// from a caller cancellation.
var errCreateDeadline = errors.New("ownsand: CreateSandbox retry got no response within the retry window")

// classify returns the failure kind of err and the retry delay of a
// resource_exhausted error, if the error detail carries one. ctx is the
// caller context: a cancelled caller is always terminal.
func classify(ctx context.Context, err error) (failureKind, time.Duration) {
	if ctx.Err() != nil {
		return failTerminal, 0
	}
	if errors.Is(err, errStall) || errors.Is(err, errAttemptDeadline) || errors.Is(err, errStreamOverrun) || errors.Is(err, errAuthorizeDeadline) {
		return failInterrupted, 0
	}
	var fe *frameError
	explicit := errors.As(err, &fe) || connect.IsWireError(err)
	if !explicit {
		var ce *connect.Error
		if errors.As(err, &ce) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || isNetError(err) {
			return failInterrupted, 0
		}
		return failTerminal, 0
	}
	switch connect.CodeOf(err) {
	case connect.CodeUnavailable:
		return failUnavailable, 0
	case connect.CodeResourceExhausted:
		var delay time.Duration
		if d := errorDetail(err); d != nil && d.GetRetryAfter() != nil {
			delay = d.GetRetryAfter().AsDuration()
		}
		return failExhausted, delay
	case connect.CodeDeadlineExceeded:
		return failDeadline, 0
	}
	return failTerminal, 0
}

// isNetError reports transport-level errors that are not Connect errors.
func isNetError(err error) bool {
	type netErr interface{ Timeout() bool }
	var ne netErr
	return errors.As(err, &ne)
}

// sessionEndReason returns the SessionEndReason of err, if any.
func sessionEndReason(err error) ownsandv1.SessionEndReason {
	if d := errorDetail(err); d != nil {
		return d.GetSessionEndReason()
	}
	return ownsandv1.SessionEndReason_SESSION_END_REASON_UNSPECIFIED
}
