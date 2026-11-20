package datapath

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"google.golang.org/protobuf/proto"
)

const (
	// MaxFileChunk is the largest file chunk payload.
	MaxFileChunk = 1 << 20
	// MaxExecOutputChunk is the largest exec stdout or stderr chunk.
	MaxExecOutputChunk = 64 << 10
	// MaxFrameSize is the largest encoded DataFrame: one maximum file chunk
	// plus 64 KiB for its envelope.
	MaxFrameSize = MaxFileChunk + 64<<10
	// DataChannelMessageSize is the largest WebRTC data channel message that
	// a sender writes (below the RFC 8841 64 KiB default receive limit).
	DataChannelMessageSize = 16 << 10
	// MaxExecRequestFields bounds the encoded command, env and working directory.
	MaxExecRequestFields = 256 << 10
	// RelayGrantHeader carries the relay grant ID on RelayService requests.
	RelayGrantHeader = "Ownsand-Relay-Grant-Id"
)

// ErrFrameTooLarge reports a declared or encoded frame above MaxFrameSize.
var ErrFrameTooLarge = errors.New("data frame exceeds the maximum frame size")

// EncodeFrame returns the varint length prefix followed by the encoded message.
func EncodeFrame(m proto.Message) ([]byte, error) {
	body, err := proto.Marshal(m)
	if err != nil {
		return nil, err
	}
	if len(body) > MaxFrameSize {
		return nil, ErrFrameTooLarge
	}
	out := make([]byte, 0, binary.MaxVarintLen64+len(body))
	out = binary.AppendUvarint(out, uint64(len(body)))
	return append(out, body...), nil
}

// FrameWriter writes length-prefixed messages to w.
type FrameWriter struct {
	w io.Writer
}

// NewFrameWriter returns a FrameWriter.
func NewFrameWriter(w io.Writer) *FrameWriter { return &FrameWriter{w: w} }

// Write encodes and writes one message.
func (fw *FrameWriter) Write(m proto.Message) error {
	b, err := EncodeFrame(m)
	if err != nil {
		return err
	}
	_, err = fw.w.Write(b)
	return err
}

// FrameReader reads length-prefixed messages. It checks the declared length
// before it buffers the frame.
type FrameReader struct {
	r *bufio.Reader
}

// NewFrameReader returns a FrameReader.
func NewFrameReader(r io.Reader) *FrameReader {
	if br, ok := r.(*bufio.Reader); ok {
		return &FrameReader{r: br}
	}
	return &FrameReader{r: bufio.NewReaderSize(r, 64<<10)}
}

// ReadRaw reads the next frame body. It returns io.EOF at a clean end and
// io.ErrUnexpectedEOF for a truncated frame.
func (fr *FrameReader) ReadRaw() ([]byte, error) {
	n, err := binary.ReadUvarint(fr.r)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, io.EOF
		}
		return nil, fmt.Errorf("read frame length: %w", err)
	}
	if n > MaxFrameSize {
		return nil, ErrFrameTooLarge
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(fr.r, body); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return body, nil
}

// Read reads the next frame into m.
func (fr *FrameReader) Read(m proto.Message) error {
	body, err := fr.ReadRaw()
	if err != nil {
		return err
	}
	return proto.Unmarshal(body, m)
}

// ReadDataFrame reads the next DataFrame.
func (fr *FrameReader) ReadDataFrame() (*ownsandv1.DataFrame, error) {
	f := &ownsandv1.DataFrame{}
	if err := fr.Read(f); err != nil {
		return nil, err
	}
	return f, nil
}

// SplitMessages splits an encoded byte stream into data channel messages of
// at most DataChannelMessageSize bytes.
func SplitMessages(stream []byte) [][]byte {
	var out [][]byte
	for len(stream) > 0 {
		n := min(len(stream), DataChannelMessageSize)
		out = append(out, stream[:n])
		stream = stream[n:]
	}
	return out
}

// PayloadUsesCredit reports whether a frame payload consumes relay credit.
// Only payload frames use credit; control frames do not.
func PayloadUsesCredit(f *ownsandv1.DataFrame) bool {
	switch f.GetPayload().(type) {
	case *ownsandv1.DataFrame_ExecRequest,
		*ownsandv1.DataFrame_ExecAttachRequest,
		*ownsandv1.DataFrame_FileReadRequest,
		*ownsandv1.DataFrame_ExecEvent,
		*ownsandv1.DataFrame_FileChunk,
		*ownsandv1.DataFrame_FileReadFinished,
		*ownsandv1.DataFrame_FileWriteChunk,
		*ownsandv1.DataFrame_FileWriteResult:
		return true
	default:
		return false
	}
}

// FrameSize returns the encoded size of f, which credit windows count.
func FrameSize(f *ownsandv1.DataFrame) int64 {
	return int64(proto.Size(f))
}

// KindOf returns the DataFrameKind that matches the payload of f.
func KindOf(f *ownsandv1.DataFrame) ownsandv1.DataFrameKind {
	switch f.GetPayload().(type) {
	case *ownsandv1.DataFrame_RelayStreamOpen:
		return ownsandv1.DataFrameKind_DATA_FRAME_KIND_RELAY_STREAM_OPEN
	case *ownsandv1.DataFrame_RelayStreamClose:
		return ownsandv1.DataFrameKind_DATA_FRAME_KIND_RELAY_STREAM_CLOSE
	case *ownsandv1.DataFrame_RelayWindowUpdate:
		return ownsandv1.DataFrameKind_DATA_FRAME_KIND_RELAY_WINDOW_UPDATE
	case *ownsandv1.DataFrame_Keepalive:
		return ownsandv1.DataFrameKind_DATA_FRAME_KIND_KEEPALIVE
	case *ownsandv1.DataFrame_SupportedKinds:
		return ownsandv1.DataFrameKind_DATA_FRAME_KIND_SUPPORTED_KINDS
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
	case *ownsandv1.DataFrame_FileChunk:
		return ownsandv1.DataFrameKind_DATA_FRAME_KIND_FILE_CHUNK
	case *ownsandv1.DataFrame_FileReadFinished:
		return ownsandv1.DataFrameKind_DATA_FRAME_KIND_FILE_READ_FINISHED
	case *ownsandv1.DataFrame_FileWriteChunk:
		return ownsandv1.DataFrameKind_DATA_FRAME_KIND_FILE_WRITE_CHUNK
	case *ownsandv1.DataFrame_FileWriteResult:
		return ownsandv1.DataFrameKind_DATA_FRAME_KIND_FILE_WRITE_RESULT
	case *ownsandv1.DataFrame_Error:
		return ownsandv1.DataFrameKind_DATA_FRAME_KIND_ERROR
	default:
		return ownsandv1.DataFrameKind_DATA_FRAME_KIND_UNSPECIFIED
	}
}

// SupportedKinds returns the first-version set of frame kinds.
func SupportedKinds() []ownsandv1.DataFrameKind {
	out := make([]ownsandv1.DataFrameKind, 0, 16)
	for k := ownsandv1.DataFrameKind_DATA_FRAME_KIND_RELAY_STREAM_OPEN; k <= ownsandv1.DataFrameKind_DATA_FRAME_KIND_FILE_READ_FINISHED; k++ {
		out = append(out, k)
	}
	return out
}

// NewFrame builds a DataFrame and sets its kind from the payload.
func NewFrame(streamID uint64, sandboxID string, set func(f *ownsandv1.DataFrame)) *ownsandv1.DataFrame {
	f := &ownsandv1.DataFrame{StreamId: streamID, SandboxId: sandboxID}
	set(f)
	f.Kind = KindOf(f)
	return f
}
