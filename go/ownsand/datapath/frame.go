// SPDX-License-Identifier: Apache-2.0

// Package datapath holds the OwnSand data path primitives that the control
// plane, the device agent, the guest helper, the CLI and the Go SDK share:
// DataFrame kinds and validation, the length-prefixed byte stream codec of
// the WebRTC data channel, credit-window flow control and the admission
// window of caller-generated operation IDs. The owning contracts are
// docs/protos-ownsand-api-contract.md (data path framing, file transfer)
// and docs/packages-ownsand-sdk-foundation.md.
//
// Frames carry sandbox content. Nothing in this package logs frames, and
// callers must not log payloads either.
package datapath

import (
	"errors"
	"fmt"
	"reflect"

	"google.golang.org/protobuf/proto"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// Size limits of the data path framing.
const (
	// MaxFrameSize is the largest encoded DataFrame: 1 MiB plus 64 KiB,
	// which holds the largest file chunk and its envelope.
	MaxFrameSize = 1<<20 + 64<<10
	// MaxDataChannelMessageSize is the largest WebRTC data channel message
	// that a sender writes (16 KiB, below the 64 KiB default receive limit
	// of RFC 8841).
	MaxDataChannelMessageSize = 16 << 10
	// MaxFileChunkSize is the largest data of one FileChunk or one
	// WriteFileRequest.
	MaxFileChunkSize = 1 << 20
	// MaxExecOutputChunkSize is the largest data of one stdout or stderr
	// ExecOutput.
	MaxExecOutputChunkSize = 64 << 10
	// MinWindowSize is the smallest initial credit window and per-stream
	// buffer: one maximum frame, so that a maximum file chunk always fits.
	MinWindowSize = MaxFrameSize
	// ConnectionStreamID is the stream_id of connection-level relay frames.
	ConnectionStreamID uint64 = 0
)

// ErrMalformedFrame reports a frame whose kind and payload do not match, or
// that breaks a framing rule.
var ErrMalformedFrame = errors.New("datapath: malformed frame")

// ErrFrameTooLarge reports an encoded DataFrame larger than MaxFrameSize.
var ErrFrameTooLarge = errors.New("datapath: frame larger than 1 MiB plus 64 KiB")

// AllKinds returns every DataFrameKind of this schema version, in number
// order. A relay side that receives no supported-kinds list assumes this
// first-version set.
func AllKinds() []ownsandv1.DataFrameKind {
	return []ownsandv1.DataFrameKind{
		ownsandv1.DataFrameKind_DATA_FRAME_KIND_RELAY_STREAM_OPEN,
		ownsandv1.DataFrameKind_DATA_FRAME_KIND_RELAY_STREAM_CLOSE,
		ownsandv1.DataFrameKind_DATA_FRAME_KIND_RELAY_WINDOW_UPDATE,
		ownsandv1.DataFrameKind_DATA_FRAME_KIND_RELAY_KEEPALIVE,
		ownsandv1.DataFrameKind_DATA_FRAME_KIND_SUPPORTED_FRAME_KINDS,
		ownsandv1.DataFrameKind_DATA_FRAME_KIND_SESSION_END,
		ownsandv1.DataFrameKind_DATA_FRAME_KIND_REQUEST_ACCEPTED,
		ownsandv1.DataFrameKind_DATA_FRAME_KIND_EXEC_REQUEST,
		ownsandv1.DataFrameKind_DATA_FRAME_KIND_EXEC_ATTACH_REQUEST,
		ownsandv1.DataFrameKind_DATA_FRAME_KIND_EXEC_EVENT,
		ownsandv1.DataFrameKind_DATA_FRAME_KIND_FILE_READ_REQUEST,
		ownsandv1.DataFrameKind_DATA_FRAME_KIND_FILE_READ_EVENT,
		ownsandv1.DataFrameKind_DATA_FRAME_KIND_FILE_WRITE_CHUNK,
		ownsandv1.DataFrameKind_DATA_FRAME_KIND_FILE_WRITE_RESULT,
		ownsandv1.DataFrameKind_DATA_FRAME_KIND_ERROR,
	}
}

// IsKnownKind reports whether kind is a DataFrameKind of this schema
// version other than UNSPECIFIED.
func IsKnownKind(kind ownsandv1.DataFrameKind) bool {
	return kind >= ownsandv1.DataFrameKind_DATA_FRAME_KIND_RELAY_STREAM_OPEN &&
		kind <= ownsandv1.DataFrameKind_DATA_FRAME_KIND_ERROR
}

// ConsumesCredit reports whether frames of kind use flow-control credit.
// Only payload frames do: exec requests, exec attach requests, file read
// requests, exec events, file read events, file write chunks and file write
// results.
func ConsumesCredit(kind ownsandv1.DataFrameKind) bool {
	switch kind {
	case ownsandv1.DataFrameKind_DATA_FRAME_KIND_EXEC_REQUEST,
		ownsandv1.DataFrameKind_DATA_FRAME_KIND_EXEC_ATTACH_REQUEST,
		ownsandv1.DataFrameKind_DATA_FRAME_KIND_FILE_READ_REQUEST,
		ownsandv1.DataFrameKind_DATA_FRAME_KIND_EXEC_EVENT,
		ownsandv1.DataFrameKind_DATA_FRAME_KIND_FILE_READ_EVENT,
		ownsandv1.DataFrameKind_DATA_FRAME_KIND_FILE_WRITE_CHUNK,
		ownsandv1.DataFrameKind_DATA_FRAME_KIND_FILE_WRITE_RESULT:
		return true
	}
	return false
}

// IsRequestKind reports whether kind is an operation request that a caller
// (or the control plane on the relay) sends toward the agent.
func IsRequestKind(kind ownsandv1.DataFrameKind) bool {
	switch kind {
	case ownsandv1.DataFrameKind_DATA_FRAME_KIND_EXEC_REQUEST,
		ownsandv1.DataFrameKind_DATA_FRAME_KIND_EXEC_ATTACH_REQUEST,
		ownsandv1.DataFrameKind_DATA_FRAME_KIND_FILE_READ_REQUEST,
		ownsandv1.DataFrameKind_DATA_FRAME_KIND_FILE_WRITE_CHUNK:
		return true
	}
	return false
}

// IsConnectionKind reports whether kind is sent only on stream_id 0 of a
// relay connection.
func IsConnectionKind(kind ownsandv1.DataFrameKind) bool {
	return kind == ownsandv1.DataFrameKind_DATA_FRAME_KIND_RELAY_KEEPALIVE ||
		kind == ownsandv1.DataFrameKind_DATA_FRAME_KIND_SUPPORTED_FRAME_KINDS
}

// PayloadKind returns the kind that the payload of f names, or UNSPECIFIED
// when no payload is set, the payload is a typed nil wrapper (a nil
// pointer held in the oneof interface, which decoding never produces but a
// hand-built frame can), or the payload is unknown to this schema version.
func PayloadKind(f *ownsandv1.DataFrame) ownsandv1.DataFrameKind {
	payload := f.GetPayload()
	if payload == nil {
		return ownsandv1.DataFrameKind_DATA_FRAME_KIND_UNSPECIFIED
	}
	// Every oneof wrapper is a pointer to a struct, so a typed nil is a nil
	// pointer value; the type switch below would otherwise match it.
	if v := reflect.ValueOf(payload); v.Kind() == reflect.Pointer && v.IsNil() {
		return ownsandv1.DataFrameKind_DATA_FRAME_KIND_UNSPECIFIED
	}
	switch payload.(type) {
	case *ownsandv1.DataFrame_RelayStreamOpen:
		return ownsandv1.DataFrameKind_DATA_FRAME_KIND_RELAY_STREAM_OPEN
	case *ownsandv1.DataFrame_RelayStreamClose:
		return ownsandv1.DataFrameKind_DATA_FRAME_KIND_RELAY_STREAM_CLOSE
	case *ownsandv1.DataFrame_RelayWindowUpdate:
		return ownsandv1.DataFrameKind_DATA_FRAME_KIND_RELAY_WINDOW_UPDATE
	case *ownsandv1.DataFrame_RelayKeepalive:
		return ownsandv1.DataFrameKind_DATA_FRAME_KIND_RELAY_KEEPALIVE
	case *ownsandv1.DataFrame_SupportedFrameKinds:
		return ownsandv1.DataFrameKind_DATA_FRAME_KIND_SUPPORTED_FRAME_KINDS
	case *ownsandv1.DataFrame_SessionEnd:
		return ownsandv1.DataFrameKind_DATA_FRAME_KIND_SESSION_END
	case *ownsandv1.DataFrame_RequestAccepted:
		return ownsandv1.DataFrameKind_DATA_FRAME_KIND_REQUEST_ACCEPTED
	case *ownsandv1.DataFrame_ExecRequest:
		return ownsandv1.DataFrameKind_DATA_FRAME_KIND_EXEC_REQUEST
	case *ownsandv1.DataFrame_ExecAttachRequest:
		return ownsandv1.DataFrameKind_DATA_FRAME_KIND_EXEC_ATTACH_REQUEST
	case *ownsandv1.DataFrame_ExecEvent:
		return ownsandv1.DataFrameKind_DATA_FRAME_KIND_EXEC_EVENT
	case *ownsandv1.DataFrame_FileReadRequest:
		return ownsandv1.DataFrameKind_DATA_FRAME_KIND_FILE_READ_REQUEST
	case *ownsandv1.DataFrame_FileReadEvent:
		return ownsandv1.DataFrameKind_DATA_FRAME_KIND_FILE_READ_EVENT
	case *ownsandv1.DataFrame_FileWriteChunk:
		return ownsandv1.DataFrameKind_DATA_FRAME_KIND_FILE_WRITE_CHUNK
	case *ownsandv1.DataFrame_FileWriteResult:
		return ownsandv1.DataFrameKind_DATA_FRAME_KIND_FILE_WRITE_RESULT
	case *ownsandv1.DataFrame_Error:
		return ownsandv1.DataFrameKind_DATA_FRAME_KIND_ERROR
	}
	return ownsandv1.DataFrameKind_DATA_FRAME_KIND_UNSPECIFIED
}

// NewFrame returns a DataFrame with kind and payload set from payload, which
// must be one of the DataFrame payload message types (for example
// *ownsandv1.ExecEvent or *ownsandv1.RelayWindowUpdate).
func NewFrame(streamID uint64, sandboxID string, payload proto.Message) (*ownsandv1.DataFrame, error) {
	f := &ownsandv1.DataFrame{StreamId: streamID, SandboxId: sandboxID}
	switch p := payload.(type) {
	case *ownsandv1.RelayStreamOpen:
		f.Payload = &ownsandv1.DataFrame_RelayStreamOpen{RelayStreamOpen: p}
	case *ownsandv1.RelayStreamClose:
		f.Payload = &ownsandv1.DataFrame_RelayStreamClose{RelayStreamClose: p}
	case *ownsandv1.RelayWindowUpdate:
		f.Payload = &ownsandv1.DataFrame_RelayWindowUpdate{RelayWindowUpdate: p}
	case *ownsandv1.RelayKeepalive:
		f.Payload = &ownsandv1.DataFrame_RelayKeepalive{RelayKeepalive: p}
	case *ownsandv1.SupportedFrameKinds:
		f.Payload = &ownsandv1.DataFrame_SupportedFrameKinds{SupportedFrameKinds: p}
	case *ownsandv1.SessionEnd:
		f.Payload = &ownsandv1.DataFrame_SessionEnd{SessionEnd: p}
	case *ownsandv1.RequestAccepted:
		f.Payload = &ownsandv1.DataFrame_RequestAccepted{RequestAccepted: p}
	case *ownsandv1.ExecRequest:
		f.Payload = &ownsandv1.DataFrame_ExecRequest{ExecRequest: p}
	case *ownsandv1.AttachExecRequest:
		f.Payload = &ownsandv1.DataFrame_ExecAttachRequest{ExecAttachRequest: p}
	case *ownsandv1.ExecEvent:
		f.Payload = &ownsandv1.DataFrame_ExecEvent{ExecEvent: p}
	case *ownsandv1.ReadFileRequest:
		f.Payload = &ownsandv1.DataFrame_FileReadRequest{FileReadRequest: p}
	case *ownsandv1.FileReadEvent:
		f.Payload = &ownsandv1.DataFrame_FileReadEvent{FileReadEvent: p}
	case *ownsandv1.WriteFileRequest:
		f.Payload = &ownsandv1.DataFrame_FileWriteChunk{FileWriteChunk: p}
	case *ownsandv1.WriteFileResponse:
		f.Payload = &ownsandv1.DataFrame_FileWriteResult{FileWriteResult: p}
	case *ownsandv1.ErrorDetail:
		f.Payload = &ownsandv1.DataFrame_Error{Error: p}
	default:
		return nil, fmt.Errorf("%w: %T is not a DataFrame payload", ErrMalformedFrame, payload)
	}
	f.Kind = PayloadKind(f)
	return f, nil
}

// MustNewFrame is NewFrame for payload types that are known at compile
// time. It panics on a type that is not a DataFrame payload.
func MustNewFrame(streamID uint64, sandboxID string, payload proto.Message) *ownsandv1.DataFrame {
	f, err := NewFrame(streamID, sandboxID, payload)
	if err != nil {
		panic(err)
	}
	return f
}

// UnknownKindError reports a frame whose kind this schema version does not
// know. On stream_id 0 a receiver ignores such a frame; on any other stream
// it ends the stream with an error frame.
type UnknownKindError struct {
	Kind     ownsandv1.DataFrameKind
	StreamID uint64
}

func (e *UnknownKindError) Error() string {
	return fmt.Sprintf("datapath: unknown frame kind %d on stream %d", int32(e.Kind), e.StreamID)
}

// ValidateFrame checks the framing rules that do not depend on the
// transport: the kind is set, known and equal to the kind of the payload,
// connection-level kinds use stream_id 0 with an empty sandbox_id, and other
// kinds use a stream_id other than 0. It returns *UnknownKindError for a kind
// that this schema version does not know, and an error that wraps
// ErrMalformedFrame otherwise.
func ValidateFrame(f *ownsandv1.DataFrame) error {
	kind := f.GetKind()
	if kind == ownsandv1.DataFrameKind_DATA_FRAME_KIND_UNSPECIFIED {
		return fmt.Errorf("%w: kind is unspecified", ErrMalformedFrame)
	}
	if !IsKnownKind(kind) {
		return &UnknownKindError{Kind: kind, StreamID: f.GetStreamId()}
	}
	if got := PayloadKind(f); got != kind {
		return fmt.Errorf("%w: kind %s carries payload %s", ErrMalformedFrame, kind, got)
	}
	if IsConnectionKind(kind) {
		if f.GetStreamId() != ConnectionStreamID || f.GetSandboxId() != "" {
			return fmt.Errorf("%w: %s must use stream_id 0 without a sandbox_id", ErrMalformedFrame, kind)
		}
		return nil
	}
	if f.GetStreamId() == ConnectionStreamID {
		return fmt.Errorf("%w: %s must not use stream_id 0", ErrMalformedFrame, kind)
	}
	return nil
}

// EncodedSize returns the size of the protobuf encoding of f, without the
// length prefix of the data channel byte stream.
func EncodedSize(f *ownsandv1.DataFrame) int {
	return proto.Size(f)
}

// CheckFrameSize returns ErrFrameTooLarge when the encoding of f is larger
// than MaxFrameSize.
func CheckFrameSize(f *ownsandv1.DataFrame) error {
	if size := proto.Size(f); size > MaxFrameSize {
		return fmt.Errorf("%w: %d bytes", ErrFrameTooLarge, size)
	}
	return nil
}

// FrameCost returns the flow-control credit that f uses: the size of its
// protobuf encoding for payload kinds and zero for every other kind.
func FrameCost(f *ownsandv1.DataFrame) int64 {
	if !ConsumesCredit(f.GetKind()) {
		return 0
	}
	return int64(proto.Size(f))
}
