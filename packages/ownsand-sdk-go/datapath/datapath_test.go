// SPDX-License-Identifier: Apache-2.0

package datapath

import (
	"bytes"
	"context"
	"errors"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/delinoio/mono/packages/ownsand-sdk-go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

const sandboxID = "0192f4b6-7c3a-7def-8abc-0123456789ab"

// samplePayloads returns one payload of every kind.
func samplePayloads() []proto.Message {
	return []proto.Message{
		&ownsandv1.RelayStreamOpen{GrantId: sandboxID, InitialWindowBytes: MinWindowSize},
		&ownsandv1.RelayStreamClose{},
		&ownsandv1.RelayWindowUpdate{IncrementBytes: 1024},
		&ownsandv1.RelayKeepalive{EchoRequested: true},
		&ownsandv1.SupportedFrameKinds{Kinds: AllKinds()},
		&ownsandv1.SessionEnd{Reason: ownsandv1.SessionEndReason_SESSION_END_REASON_IDLE},
		&ownsandv1.RequestAccepted{},
		&ownsandv1.ExecRequest{SandboxId: sandboxID, ExecId: sandboxID, Command: []string{"/bin/true"}, Timeout: durationpb.New(time.Second)},
		&ownsandv1.AttachExecRequest{ExecId: sandboxID, StdoutOffset: 10, StderrOffset: 20},
		&ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Stdout{Stdout: &ownsandv1.ExecOutput{Offset: 5, Data: []byte("hi")}}},
		&ownsandv1.ReadFileRequest{ReadId: sandboxID, Path: "/etc/hosts"},
		&ownsandv1.FileReadEvent{Event: &ownsandv1.FileReadEvent_Chunk{Chunk: &ownsandv1.FileChunk{Data: []byte("x")}}},
		&ownsandv1.WriteFileRequest{WriteId: sandboxID, Data: []byte("abc")},
		&ownsandv1.WriteFileResponse{WriteId: sandboxID, Length: 3},
		&ownsandv1.ErrorDetail{Code: ownsandv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED},
	}
}

func frameFor(t testing.TB, payload proto.Message) *ownsandv1.DataFrame {
	t.Helper()
	f, err := NewFrame(1, sandboxID, payload)
	if err != nil {
		t.Fatal(err)
	}
	if IsConnectionKind(f.GetKind()) {
		f.StreamId = 0
		f.SandboxId = ""
	}
	return f
}

func TestNewFrameCoversEveryKind(t *testing.T) {
	seen := map[ownsandv1.DataFrameKind]bool{}
	for _, payload := range samplePayloads() {
		f := frameFor(t, payload)
		if f.GetKind() == ownsandv1.DataFrameKind_DATA_FRAME_KIND_UNSPECIFIED {
			t.Fatalf("%T gave no kind", payload)
		}
		if err := ValidateFrame(f); err != nil {
			t.Fatalf("%T: %v", payload, err)
		}
		seen[f.GetKind()] = true
	}
	for _, kind := range AllKinds() {
		if !seen[kind] {
			t.Errorf("kind %s not produced", kind)
		}
	}
	// AllKinds matches the enum descriptor.
	if got, want := len(AllKinds()), ownsandv1.DataFrameKind(0).Descriptor().Values().Len()-1; got != want {
		t.Fatalf("AllKinds has %d kinds, enum has %d", got, want)
	}
	if _, err := NewFrame(1, sandboxID, &ownsandv1.Sandbox{}); !errors.Is(err, ErrMalformedFrame) {
		t.Fatalf("non-payload type: %v", err)
	}
}

func TestValidateFrame(t *testing.T) {
	cases := []struct {
		name  string
		frame *ownsandv1.DataFrame
		want  error
	}{
		{"unspecified", &ownsandv1.DataFrame{StreamId: 1}, ErrMalformedFrame},
		{"kind without payload", &ownsandv1.DataFrame{StreamId: 1, Kind: ownsandv1.DataFrameKind_DATA_FRAME_KIND_EXEC_EVENT}, ErrMalformedFrame},
		{"kind mismatch", &ownsandv1.DataFrame{StreamId: 1, Kind: ownsandv1.DataFrameKind_DATA_FRAME_KIND_EXEC_EVENT, Payload: &ownsandv1.DataFrame_RequestAccepted{RequestAccepted: &ownsandv1.RequestAccepted{}}}, ErrMalformedFrame},
		{"keepalive on a stream", MustNewFrame(3, "", &ownsandv1.RelayKeepalive{}), ErrMalformedFrame},
		{"keepalive with sandbox", MustNewFrame(0, sandboxID, &ownsandv1.RelayKeepalive{}), ErrMalformedFrame},
		{"payload on stream 0", MustNewFrame(0, sandboxID, &ownsandv1.ExecEvent{}), ErrMalformedFrame},
	}
	for _, tc := range cases {
		if err := ValidateFrame(tc.frame); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
	var unknown *UnknownKindError
	err := ValidateFrame(&ownsandv1.DataFrame{StreamId: 4, Kind: ownsandv1.DataFrameKind(99)})
	if !errors.As(err, &unknown) || unknown.StreamID != 4 {
		t.Fatalf("unknown kind: %v", err)
	}
}

func TestConsumesCredit(t *testing.T) {
	credit := map[ownsandv1.DataFrameKind]bool{}
	for _, kind := range AllKinds() {
		credit[kind] = ConsumesCredit(kind)
	}
	want := []ownsandv1.DataFrameKind{
		ownsandv1.DataFrameKind_DATA_FRAME_KIND_EXEC_REQUEST,
		ownsandv1.DataFrameKind_DATA_FRAME_KIND_EXEC_ATTACH_REQUEST,
		ownsandv1.DataFrameKind_DATA_FRAME_KIND_FILE_READ_REQUEST,
		ownsandv1.DataFrameKind_DATA_FRAME_KIND_EXEC_EVENT,
		ownsandv1.DataFrameKind_DATA_FRAME_KIND_FILE_READ_EVENT,
		ownsandv1.DataFrameKind_DATA_FRAME_KIND_FILE_WRITE_CHUNK,
		ownsandv1.DataFrameKind_DATA_FRAME_KIND_FILE_WRITE_RESULT,
	}
	for _, kind := range want {
		if !credit[kind] {
			t.Errorf("%s does not consume credit", kind)
		}
		delete(credit, kind)
	}
	for kind, uses := range credit {
		if uses {
			t.Errorf("%s consumes credit", kind)
		}
	}
	accepted := MustNewFrame(1, sandboxID, &ownsandv1.RequestAccepted{})
	if FrameCost(accepted) != 0 {
		t.Fatal("request accepted frame has a cost")
	}
	event := MustNewFrame(1, sandboxID, &ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Keepalive{Keepalive: &ownsandv1.Keepalive{}}})
	if FrameCost(event) != int64(proto.Size(event)) {
		t.Fatal("exec event cost is not its encoded size")
	}
}

// maxChunkFrame returns a frame with the largest file chunk.
func maxChunkFrame() *ownsandv1.DataFrame {
	return MustNewFrame(7, sandboxID, &ownsandv1.FileReadEvent{Event: &ownsandv1.FileReadEvent_Chunk{
		Chunk: &ownsandv1.FileChunk{Offset: 1 << 40, Data: bytes.Repeat([]byte{0xab}, MaxFileChunkSize)},
	}})
}

func TestMaxFileChunkFitsInFrame(t *testing.T) {
	f := maxChunkFrame()
	if err := CheckFrameSize(f); err != nil {
		t.Fatal(err)
	}
	write := MustNewFrame(7, sandboxID, &ownsandv1.WriteFileRequest{
		OrganizationId: sandboxID, SandboxId: sandboxID, WriteId: sandboxID,
		Path: "/" + strings.Repeat("p", 4096), BindingSalt: make([]byte, 32), Offset: 1 << 40,
		Data:   bytes.Repeat([]byte{1}, MaxFileChunkSize),
		Finish: &ownsandv1.WriteFileFinish{SizeBytes: 1 << 40, Mode: 0o644},
	})
	if err := CheckFrameSize(write); err != nil {
		t.Fatal(err)
	}
	if _, err := MarshalFrame(write); err != nil {
		t.Fatal(err)
	}
}

func TestOversizedFrameRejected(t *testing.T) {
	f := MustNewFrame(7, sandboxID, &ownsandv1.FileReadEvent{Event: &ownsandv1.FileReadEvent_Chunk{
		Chunk: &ownsandv1.FileChunk{Data: make([]byte, MaxFrameSize)},
	}})
	if _, err := MarshalFrame(f); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("MarshalFrame: %v", err)
	}
	if _, err := AppendFrame(nil, f); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("AppendFrame: %v", err)
	}
	if _, err := UnmarshalFrame(make([]byte, MaxFrameSize+1)); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("UnmarshalFrame: %v", err)
	}
}

func TestEncodeMessagesSplitsAndDecoderReassembles(t *testing.T) {
	f := maxChunkFrame()
	messages, err := EncodeMessages(f)
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, m := range messages {
		if len(m) == 0 || len(m) > MaxDataChannelMessageSize {
			t.Fatalf("message size %d", len(m))
		}
		total += len(m)
	}
	size := proto.Size(f)
	if total != size+protowire.SizeVarint(uint64(size)) {
		t.Fatalf("total %d, frame %d", total, size)
	}
	d := NewDecoder()
	var got []*ownsandv1.DataFrame
	for i, m := range messages {
		frames, err := d.Push(m)
		if err != nil {
			t.Fatal(err)
		}
		if i < len(messages)-1 && len(frames) != 0 {
			t.Fatal("frame completed early")
		}
		got = append(got, frames...)
	}
	if len(got) != 1 || !proto.Equal(got[0], f) {
		t.Fatalf("reassembled %d frames", len(got))
	}
	if d.Buffered() != 0 {
		t.Fatalf("buffered %d", d.Buffered())
	}
}

func TestDecoderRejectsDeclaredOversizeBeforeBuffering(t *testing.T) {
	d := NewDecoder()
	header := protowire.AppendVarint(nil, MaxFrameSize+1)
	frames, err := d.Push(header)
	if !errors.Is(err, ErrFrameTooLarge) || len(frames) != 0 {
		t.Fatalf("err = %v", err)
	}
	if d.Buffered() != 0 {
		t.Fatalf("buffered %d after rejection", d.Buffered())
	}
	if _, err := d.Push([]byte{0}); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("decoder recovered after error: %v", err)
	}
	if !errors.Is(d.Err(), ErrFrameTooLarge) {
		t.Fatal("Err() lost the error")
	}
	// A declared length exactly at the limit is accepted and buffered.
	d = NewDecoder()
	if _, err := d.Push(protowire.AppendVarint(nil, MaxFrameSize)); err != nil {
		t.Fatal(err)
	}
}

func TestDecoderMalformedInput(t *testing.T) {
	d := NewDecoder()
	if _, err := d.Push(bytes.Repeat([]byte{0xff}, 11)); !errors.Is(err, ErrMalformedStream) {
		t.Fatalf("long varint: %v", err)
	}
	d = NewDecoder()
	// Length 2 followed by an invalid protobuf (field 0).
	if _, err := d.Push([]byte{2, 0x00, 0x00}); !errors.Is(err, ErrMalformedFrame) {
		t.Fatalf("bad body: %v", err)
	}
}

func TestDecoderZeroLengthFrame(t *testing.T) {
	d := NewDecoder()
	frames, err := d.Push([]byte{0})
	if err != nil || len(frames) != 1 {
		t.Fatalf("frames %d err %v", len(frames), err)
	}
	if err := ValidateFrame(frames[0]); !errors.Is(err, ErrMalformedFrame) {
		t.Fatalf("empty frame validated: %v", err)
	}
}

// TestCodecRandomSplits encodes many frames into one byte stream, cuts it at
// random points and checks that the decoder returns the same frames.
func TestCodecRandomSplits(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for round := 0; round < 200; round++ {
		var stream []byte
		var want []*ownsandv1.DataFrame
		n := 1 + rng.IntN(20)
		for i := 0; i < n; i++ {
			payloads := samplePayloads()
			f := frameFor(t, payloads[rng.IntN(len(payloads))])
			if rng.IntN(4) == 0 {
				f = MustNewFrame(uint64(i+1), sandboxID, &ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Stderr{
					Stderr: &ownsandv1.ExecOutput{Offset: int64(rng.Uint32()), Data: randomBytes(rng, rng.IntN(70_000))},
				}})
			}
			var err error
			stream, err = AppendFrame(stream, f)
			if err != nil {
				t.Fatal(err)
			}
			want = append(want, f)
		}
		d := NewDecoder()
		var got []*ownsandv1.DataFrame
		for len(stream) > 0 {
			cut := min(len(stream), 1+rng.IntN(3*MaxDataChannelMessageSize))
			frames, err := d.Push(stream[:cut])
			if err != nil {
				t.Fatalf("round %d: %v", round, err)
			}
			got = append(got, frames...)
			stream = stream[cut:]
		}
		if len(got) != len(want) {
			t.Fatalf("round %d: got %d frames, want %d", round, len(got), len(want))
		}
		for i := range want {
			if !proto.Equal(got[i], want[i]) {
				t.Fatalf("round %d frame %d differs", round, i)
			}
		}
	}
}

// TestDecoderRandomBytes feeds random byte streams to the decoder. It must
// never panic and never buffer more than one maximum frame.
func TestDecoderRandomBytes(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	for round := 0; round < 2000; round++ {
		d := NewDecoder()
		for i := 0; i < 8; i++ {
			_, err := d.Push(randomBytes(rng, rng.IntN(64)))
			if d.Buffered() > MaxFrameSize+maxVarintLen {
				t.Fatalf("buffered %d", d.Buffered())
			}
			if err != nil {
				break
			}
		}
	}
}

func FuzzDecoder(f *testing.F) {
	stream, _ := AppendFrame(nil, MustNewFrame(1, sandboxID, &ownsandv1.ExecEvent{}))
	f.Add(stream, 3)
	f.Add([]byte{0xff, 0xff, 0xff}, 1)
	f.Fuzz(func(t *testing.T, data []byte, cut int) {
		d := NewDecoder()
		if cut <= 0 {
			cut = 1
		}
		for len(data) > 0 {
			n := min(cut, len(data))
			if _, err := d.Push(data[:n]); err != nil {
				return
			}
			data = data[n:]
		}
	})
}

func randomBytes(rng *rand.Rand, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(rng.Uint32())
	}
	return b
}

func TestSplitMessages(t *testing.T) {
	if got := SplitMessages(nil, 4); len(got) != 0 {
		t.Fatalf("empty stream gave %d messages", len(got))
	}
	got := SplitMessages([]byte("abcdefghij"), 4)
	if len(got) != 3 || string(got[0]) != "abcd" || string(got[2]) != "ij" {
		t.Fatalf("got %q", got)
	}
	// Appending to one message must not overwrite the next.
	got[0] = append(got[0], 'Z')
	if string(got[1]) != "efgh" {
		t.Fatalf("messages share capacity: %q", got[1])
	}
}

func TestSendWindow(t *testing.T) {
	w, err := NewSendWindow(100)
	if err != nil {
		t.Fatal(err)
	}
	if !w.TryConsume(60) || w.TryConsume(41) || w.Available() != 40 {
		t.Fatalf("available %d", w.Available())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := w.Consume(ctx, 50); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Consume without credit: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- w.Consume(context.Background(), 50) }()
	time.Sleep(10 * time.Millisecond)
	if err := w.Add(10); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("consumer not woken by a window update")
	}
	if w.Available() != 0 {
		t.Fatalf("available %d after consume", w.Available())
	}
	for _, bad := range []int64{0, -1} {
		if err := w.Add(bad); !errors.Is(err, ErrInvalidWindowUpdate) {
			t.Fatalf("Add(%d): %v", bad, err)
		}
	}
	if err := w.Add(MaxWindowSize + 1); !errors.Is(err, ErrInvalidWindowUpdate) {
		t.Fatalf("overflow: %v", err)
	}
	if _, err := NewSendWindow(-1); err == nil {
		t.Fatal("negative initial window accepted")
	}
}

func TestReceiveWindow(t *testing.T) {
	if _, err := NewReceiveWindow(MinWindowSize - 1); err == nil {
		t.Fatal("window below one maximum frame accepted")
	}
	w, err := NewReceiveWindow(2 * MinWindowSize)
	if err != nil {
		t.Fatal(err)
	}
	if w.InitialWindow() != 2*MinWindowSize {
		t.Fatal("initial window")
	}
	if err := w.Receive(MinWindowSize); err != nil {
		t.Fatal(err)
	}
	if err := w.Receive(MinWindowSize); err != nil {
		t.Fatal(err)
	}
	if err := w.Receive(1); !errors.Is(err, ErrWindowExceeded) {
		t.Fatalf("over window: %v", err)
	}
	if w.Outstanding() != 0 || w.Buffered() != 2*MinWindowSize {
		t.Fatalf("outstanding %d buffered %d", w.Outstanding(), w.Buffered())
	}
	inc, err := w.Release(1000)
	if err != nil || inc != 1000 {
		t.Fatalf("release: %d %v", inc, err)
	}
	if w.Outstanding()+w.Buffered() != w.InitialWindow() {
		t.Fatal("invariant broken")
	}
	if _, err := w.Release(w.Buffered() + 1); err == nil {
		t.Fatal("over-release accepted")
	}
}

// TestFlowControlEndToEnd runs a sender and a slow receiver through a pair
// of windows and checks that the receiver never buffers more than its
// capacity.
func TestFlowControlEndToEnd(t *testing.T) {
	recv, _ := NewReceiveWindow(MinWindowSize)
	send, _ := NewSendWindow(recv.InitialWindow())
	frames := make(chan int64, 1000)
	updates := make(chan int64, 1000)
	ctx := context.Background()
	go func() {
		for inc := range updates {
			if err := send.Add(inc); err != nil {
				panic(err)
			}
		}
	}()
	const count = 200
	go func() {
		for i := 0; i < count; i++ {
			f := maxChunkFrame()
			if i%2 == 0 {
				f = MustNewFrame(7, sandboxID, &ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Stdout{Stdout: &ownsandv1.ExecOutput{Data: make([]byte, 1000)}}})
			}
			cost := FrameCost(f)
			if err := send.Consume(ctx, cost); err != nil {
				panic(err)
			}
			frames <- cost
		}
		close(frames)
	}()
	received := 0
	for cost := range frames {
		if err := recv.Receive(cost); err != nil {
			t.Fatal(err)
		}
		if recv.Buffered() > recv.InitialWindow() {
			t.Fatal("buffer exceeded")
		}
		inc, err := recv.Release(cost)
		if err != nil {
			t.Fatal(err)
		}
		updates <- inc
		received++
	}
	close(updates)
	if received != count {
		t.Fatalf("received %d", received)
	}
}

func TestCheckOperationID(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	idAt := func(at time.Time) string {
		id, err := uuidv7.FromTime(at, nil)
		if err != nil {
			t.Fatal(err)
		}
		return id.String()
	}
	cases := []struct {
		name   string
		id     string
		code   ownsandv1.ErrorCode
		reason AdmissionReason
	}{
		{"now", idAt(now), 0, 0},
		{"just under 15 minutes", idAt(now.Add(-OperationIDMaxAge + time.Millisecond)), 0, 0},
		{"exactly 15 minutes", idAt(now.Add(-OperationIDMaxAge)), ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND, AdmissionReasonExpired},
		{"an hour old", idAt(now.Add(-time.Hour)), ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND, AdmissionReasonExpired},
		{"exactly 5 minutes ahead", idAt(now.Add(OperationIDMaxFuture)), 0, 0},
		{"over 5 minutes ahead", idAt(now.Add(OperationIDMaxFuture + time.Millisecond)), ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION, AdmissionReasonFuture},
		{"not a UUID", "exec-1", ownsandv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, AdmissionReasonInvalid},
		{"UUID v4", "0192f4b6-7c3a-4def-8abc-0123456789ab", ownsandv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, AdmissionReasonInvalid},
	}
	for _, tc := range cases {
		err := CheckOperationID(tc.id, now)
		if tc.code == 0 {
			if err != nil {
				t.Errorf("%s: %v", tc.name, err)
			}
			continue
		}
		var ae *AdmissionError
		if !errors.As(err, &ae) || ae.Code != tc.code || ae.Reason != tc.reason {
			t.Errorf("%s: err = %v", tc.name, err)
			continue
		}
		if detail := ae.Detail(); detail.GetCode() != tc.code || detail.GetMessage() == "" {
			t.Errorf("%s: detail %v", tc.name, detail)
		}
	}
}
