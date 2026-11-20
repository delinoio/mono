// SPDX-License-Identifier: Apache-2.0

package datapath

import (
	"errors"
	"fmt"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// maxVarintLen is the longest varint encoding of a uint64.
const maxVarintLen = 10

// MarshalFrame encodes f without a length prefix, as one relay or guest
// stream message carries it. It fails with ErrFrameTooLarge when the
// encoding is larger than MaxFrameSize.
func MarshalFrame(f *ownsandv1.DataFrame) ([]byte, error) {
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(f)
	if err != nil {
		return nil, fmt.Errorf("datapath: encode frame: %w", err)
	}
	if len(b) > MaxFrameSize {
		return nil, fmt.Errorf("%w: %d bytes", ErrFrameTooLarge, len(b))
	}
	return b, nil
}

// UnmarshalFrame decodes one encoded DataFrame. It checks the size before it
// decodes.
func UnmarshalFrame(b []byte) (*ownsandv1.DataFrame, error) {
	if len(b) > MaxFrameSize {
		return nil, fmt.Errorf("%w: %d bytes", ErrFrameTooLarge, len(b))
	}
	f := &ownsandv1.DataFrame{}
	if err := proto.Unmarshal(b, f); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformedFrame, err)
	}
	return f, nil
}

// AppendFrame appends the data channel encoding of f to dst: a varint length
// followed by the encoded DataFrame.
func AppendFrame(dst []byte, f *ownsandv1.DataFrame) ([]byte, error) {
	b, err := MarshalFrame(f)
	if err != nil {
		return dst, err
	}
	dst = protowire.AppendVarint(dst, uint64(len(b)))
	return append(dst, b...), nil
}

// SplitMessages splits a data channel byte stream into messages of at most
// maxSize bytes. The messages share the memory of stream.
func SplitMessages(stream []byte, maxSize int) [][]byte {
	if maxSize <= 0 {
		panic("datapath: SplitMessages needs a positive maxSize")
	}
	messages := make([][]byte, 0, (len(stream)+maxSize-1)/maxSize)
	for len(stream) > 0 {
		n := min(len(stream), maxSize)
		messages = append(messages, stream[:n:n])
		stream = stream[n:]
	}
	return messages
}

// EncodeMessages encodes f for the data channel and splits the bytes into
// messages of at most MaxDataChannelMessageSize bytes. Sending the messages
// in order on the ordered, reliable data channel transmits the frame.
func EncodeMessages(f *ownsandv1.DataFrame) ([][]byte, error) {
	stream, err := AppendFrame(nil, f)
	if err != nil {
		return nil, err
	}
	return SplitMessages(stream, MaxDataChannelMessageSize), nil
}

// ErrMalformedStream reports a data channel byte stream with an invalid
// length prefix.
var ErrMalformedStream = errors.New("datapath: malformed length prefix")

// Decoder reassembles DataFrame messages from the data channel byte stream.
// It reads the varint length of each frame and rejects a declared length
// larger than MaxFrameSize before it buffers the frame. After an error the
// decoder stays failed and returns the same error; the receiver then closes
// the data channel. A Decoder is not safe for concurrent use.
type Decoder struct {
	header  [maxVarintLen]byte
	hlen    int
	reading bool // true while reading the body of a frame
	body    []byte
	need    int
	err     error
}

// NewDecoder returns an empty decoder.
func NewDecoder() *Decoder {
	return &Decoder{}
}

// Push adds one received data channel message and returns the frames that it
// completed, in order.
func (d *Decoder) Push(message []byte) ([]*ownsandv1.DataFrame, error) {
	if d.err != nil {
		return nil, d.err
	}
	var frames []*ownsandv1.DataFrame
	for len(message) > 0 {
		if !d.reading {
			d.header[d.hlen] = message[0]
			d.hlen++
			message = message[1:]
			if d.header[d.hlen-1]&0x80 != 0 {
				if d.hlen == maxVarintLen {
					d.err = fmt.Errorf("%w: length varint longer than %d bytes", ErrMalformedStream, maxVarintLen)
					return frames, d.err
				}
				continue
			}
			length, n := protowire.ConsumeVarint(d.header[:d.hlen])
			if n < 0 {
				d.err = fmt.Errorf("%w: %v", ErrMalformedStream, protowire.ParseError(n))
				return frames, d.err
			}
			d.hlen = 0
			if length > MaxFrameSize {
				d.err = fmt.Errorf("%w: declared length %d", ErrFrameTooLarge, length)
				return frames, d.err
			}
			d.need = int(length)
			d.body = make([]byte, 0, d.need)
			d.reading = true
		}
		n := min(d.need-len(d.body), len(message))
		d.body = append(d.body, message[:n]...)
		message = message[n:]
		if len(d.body) == d.need {
			f := &ownsandv1.DataFrame{}
			if err := proto.Unmarshal(d.body, f); err != nil {
				d.err = fmt.Errorf("%w: %v", ErrMalformedFrame, err)
				return frames, d.err
			}
			frames = append(frames, f)
			d.reading = false
			d.body = nil
			d.need = 0
		}
	}
	return frames, nil
}

// Buffered returns the number of bytes of an incomplete frame that the
// decoder holds, including a partial length prefix.
func (d *Decoder) Buffered() int {
	return d.hlen + len(d.body)
}

// Err returns the error that stopped the decoder, if any.
func (d *Decoder) Err() error {
	return d.err
}
