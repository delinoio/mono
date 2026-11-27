// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/delinoio/mono/go/ownsand/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/grants"
	"github.com/delinoio/mono/servers/ownsand-api/internal/relay"
)

// rotationHarness runs serveStream against a real relay connection whose
// agent side the test drives frame by frame, so the request context can end at
// an exact point of the handler: while the accepted event is being sent, with
// the next agent frame already queued or not. Nothing here depends on timing.
type rotationHarness struct {
	t      *testing.T
	plane  *DataPlane
	call   *relayCall
	logs   *bytes.Buffer
	in     chan *ownsandv1.DataFrame
	idle   chan struct{}
	done   chan struct{}
	runEnd chan error
	// synced is set when the connection waits in recv for its next frame.
	synced bool

	mu     sync.Mutex
	events []*ownsandv1.ExecEvent
	sent   chan *ownsandv1.DataFrame
}

func newRotationHarness(t *testing.T, timing RelayTiming, beforeSend func(*ownsandv1.DataFrame)) *rotationHarness {
	t.Helper()
	limits := relay.Limits{StreamBuffer: datapath.MinWindowSize, MaxStreamsPerConn: 4, MaxStreamsPerGrant: 4, Keepalive: time.Minute, IdleTimeout: time.Hour}
	h := &rotationHarness{
		t: t, logs: &bytes.Buffer{},
		in: make(chan *ownsandv1.DataFrame), idle: make(chan struct{}), done: make(chan struct{}),
		runEnd: make(chan error, 1), sent: make(chan *ownsandv1.DataFrame, 64),
	}
	conn := relay.NewConn(context.Background(), "device", 1, limits)
	// recv signals on idle each time it asks for the next frame. The
	// connection dispatches a frame before it calls recv again, so a receive
	// from idle after a feed proves that the frame reached its stream queue.
	recv := func() (*ownsandv1.DataFrame, error) {
		select {
		case h.idle <- struct{}{}:
		case <-h.done:
			return nil, errors.New("test finished")
		}
		select {
		case f := <-h.in:
			return f, nil
		case <-h.done:
			return nil, errors.New("test finished")
		}
	}
	send := func(f *ownsandv1.DataFrame) error {
		if beforeSend != nil {
			beforeSend(f)
		}
		if f.GetRelayKeepalive() != nil {
			return nil
		}
		select {
		case h.sent <- f:
		default:
		}
		return nil
	}
	go func() { h.runEnd <- conn.Run(recv, send) }()
	t.Cleanup(func() {
		close(h.done)
		conn.Close(errors.New("test finished"))
		<-h.runEnd
	})
	stream, err := conn.Open(context.Background(), "grant-1", "sandbox")
	if err != nil {
		t.Fatal(err)
	}
	h.plane = &DataPlane{
		S:      &Service{Logger: slog.New(slog.NewJSONHandler(h.logs, nil)), Now: time.Now},
		Timing: timing,
	}
	// The stream was allocated an hour ago. With the one-minute Rotation of
	// pastRotation, the rotation time passed 59 minutes ago, so the rotation
	// branch is reached at the first frame on every platform. The tests must
	// not rely on a short rotation interval that elapses between two
	// time.Now calls: on Windows the Go clock only advances every 0.5 to
	// 15.6 ms, so the allocation time and the first frame can read the same
	// instant, the rotation check is false and the handler forwards the frame.
	stream.Started = time.Now().Add(-time.Hour)
	h.call = &relayCall{procedure: "/delino.ownsand.v1.RelayService/Exec", grant: &grants.Grant{ID: "grant-1"}, stream: stream, start: time.Now()}
	h.feed(&ownsandv1.RelayWindowUpdate{IncrementBytes: datapath.MinWindowSize})
	return h
}

// feed delivers one agent frame and returns after the connection queued it on
// its stream.
func (h *rotationHarness) feed(payload any) {
	h.t.Helper()
	var f *ownsandv1.DataFrame
	switch p := payload.(type) {
	case *ownsandv1.RequestAccepted:
		f = datapath.MustNewFrame(h.call.stream.ID, "sandbox", p)
	case *ownsandv1.ExecEvent:
		f = datapath.MustNewFrame(h.call.stream.ID, "sandbox", p)
	case *ownsandv1.RelayWindowUpdate:
		f = datapath.MustNewFrame(h.call.stream.ID, "sandbox", p)
	default:
		h.t.Fatalf("unsupported payload %T", payload)
	}
	// The connection calls recv, which signals on idle, once per frame, after
	// it dispatched the previous one. The signal that follows the delivery
	// below proves that f reached its stream queue; it stays consumed until
	// the next feed, which then needs no new signal.
	waitIdle := func() {
		select {
		case <-h.idle:
		case <-time.After(10 * time.Second):
			h.t.Fatal("the relay connection did not ask for a frame")
		}
	}
	if !h.synced {
		waitIdle()
	}
	h.in <- f
	waitIdle()
	h.synced = true
}

// serve runs serveStream with ctx. onAccepted runs inside the callback that
// sends the accepted event to the caller, which is the point at which the
// handler has committed to the accepted state and not yet read the next frame.
func (h *rotationHarness) serve(ctx context.Context, onAccepted func()) error {
	h.t.Helper()
	send := func(e *ownsandv1.ExecEvent) error {
		h.mu.Lock()
		h.events = append(h.events, e)
		h.mu.Unlock()
		if e.GetAccepted() != nil && onAccepted != nil {
			onAccepted()
		}
		return nil
	}
	request := datapath.MustNewFrame(h.call.stream.ID, "sandbox", &ownsandv1.ExecRequest{})
	return h.plane.serveStream(ctx, h.call, time.Now().Add(time.Minute), request, execEvents(send))
}

func (h *rotationHarness) eventKinds() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var kinds []string
	for _, e := range h.events {
		switch {
		case e.GetAccepted() != nil:
			kinds = append(kinds, "accepted")
		case e.GetStdout() != nil:
			kinds = append(kinds, "stdout")
		case e.GetExited() != nil:
			kinds = append(kinds, "exited")
		case e.GetKeepalive() != nil:
			kinds = append(kinds, "keepalive")
		default:
			kinds = append(kinds, "other")
		}
	}
	return kinds
}

// waitClose waits for the relay stream close frame of the call on the agent
// connection.
func (h *rotationHarness) waitClose() {
	h.t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case f := <-h.sent:
			if f.GetStreamId() == h.call.stream.ID && f.GetRelayStreamClose() != nil {
				return
			}
		case <-timeout:
			h.t.Fatal("the agent did not receive a relay stream close frame")
		}
	}
}

// logCode returns the code of the relay_rpc log record.
func (h *rotationHarness) logCode() string {
	h.t.Helper()
	for _, line := range strings.Split(h.logs.String(), "\n") {
		var rec struct {
			Msg  string `json:"msg"`
			Code string `json:"code"`
		}
		if json.Unmarshal([]byte(line), &rec) == nil && rec.Msg == "relay_rpc" {
			return rec.Code
		}
	}
	h.t.Fatalf("no relay_rpc log record in %q", h.logs.String())
	return ""
}

// pastRotation is a timing whose rotation time has already passed when the
// first frame after the accepted event is received: newRotationHarness
// allocates the stream an hour in the past, which is far beyond this one-minute
// Rotation and beyond the resolution of any platform clock.
var pastRotation = RelayTiming{Rotation: time.Minute, Keepalive: time.Minute, Acceptance: time.Minute, RequestDeadline: time.Minute}

func stdout(data string) *ownsandv1.ExecEvent {
	return &ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Stdout{Stdout: &ownsandv1.ExecOutput{Data: []byte(data)}}}
}

// TestRotationAfterRequestCancelReturnsContextError checks that a rotation
// that finds the request context ended does not return OK. The caller
// cancels during the accepted event while another agent frame is already
// queued. The call is logged as canceled, the queued frame stays unsent, and
// the agent receives a stream close frame.
func TestRotationAfterRequestCancelReturnsContextError(t *testing.T) {
	h := newRotationHarness(t, pastRotation, nil)
	h.feed(&ownsandv1.RequestAccepted{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := h.serve(ctx, func() {
		h.feed(stdout("after"))
		cancel()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("serveStream = %v, want context.Canceled", err)
	}
	if got := h.logCode(); got != "canceled" {
		t.Errorf("logged code = %q, want canceled", got)
	}
	if got := h.eventKinds(); len(got) != 1 || got[0] != "accepted" {
		t.Errorf("events = %v, want only the accepted event", got)
	}
	if h.call.frames != 0 || h.call.bytes != 0 {
		t.Errorf("forwarded %d frames and %d bytes, want none", h.call.frames, h.call.bytes)
	}
	h.waitClose()
}

// expiringContext reports context.DeadlineExceeded from Err once expire ran.
// A real deadline would also end the request upload and the first Next before
// the handler sends the accepted event. Done stays open here to simulate a
// deadline first observed through Err while the handler is sending.
type expiringContext struct {
	context.Context
	expired atomic.Bool
}

func (c *expiringContext) expire() { c.expired.Store(true) }

func (c *expiringContext) Err() error {
	if c.expired.Load() {
		return context.DeadlineExceeded
	}
	return c.Context.Err()
}

// TestRotationAfterRequestDeadlineReturnsDeadlineExceeded is the same check
// for a request context that ended with its deadline.
func TestRotationAfterRequestDeadlineReturnsDeadlineExceeded(t *testing.T) {
	h := newRotationHarness(t, pastRotation, nil)
	h.feed(&ownsandv1.RequestAccepted{})
	ctx := &expiringContext{Context: context.Background()}
	err := h.serve(ctx, func() {
		h.feed(stdout("after"))
		ctx.expire()
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("serveStream = %v, want context.DeadlineExceeded", err)
	}
	if got := h.logCode(); got != "deadline_exceeded" {
		t.Errorf("logged code = %q, want deadline_exceeded", got)
	}
	if got := h.eventKinds(); len(got) != 1 || got[0] != "accepted" {
		t.Errorf("events = %v, want only the accepted event", got)
	}
	h.waitClose()
}

// TestRotationWithEmptyQueueAfterRequestCancel checks cancellation at
// rotation with no queued frame after the accepted event.
func TestRotationWithEmptyQueueAfterRequestCancel(t *testing.T) {
	timing := pastRotation
	timing.Keepalive = 4 * time.Millisecond // the handler wakes every millisecond
	h := newRotationHarness(t, timing, nil)
	h.feed(&ownsandv1.RequestAccepted{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := h.serve(ctx, cancel)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("serveStream = %v, want context.Canceled", err)
	}
	if got := h.logCode(); got != "canceled" {
		t.Errorf("logged code = %q, want canceled", got)
	}
	h.waitClose()
}

// TestRotationWithLiveContextReturnsOK is the control: the same queued frame
// with a live context still rotates with OK status, the frame at or after the
// deadline is not forwarded, no extra event is sent and the agent still gets
// its stream close frame.
func TestRotationWithLiveContextReturnsOK(t *testing.T) {
	h := newRotationHarness(t, pastRotation, nil)
	h.feed(&ownsandv1.RequestAccepted{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := h.serve(ctx, func() { h.feed(stdout("after")) })
	if err != nil {
		t.Fatalf("serveStream = %v, want nil (rotation)", err)
	}
	if got := h.logCode(); got != "ok" {
		t.Errorf("logged code = %q, want ok", got)
	}
	if got := h.eventKinds(); len(got) != 1 || got[0] != "accepted" {
		t.Errorf("events = %v, want only the accepted event", got)
	}
	if h.call.frames != 0 || h.call.bytes != 0 {
		t.Errorf("forwarded %d frames and %d bytes, want none", h.call.frames, h.call.bytes)
	}
	h.waitClose()
}

// TestRotationWhileReturningRelayCredit holds the connection writer after
// request acceptance, so returning credit for delivered output must wait
// for room. Rotation still returns OK and reserves a close frame.
func TestRotationWhileReturningRelayCredit(t *testing.T) {
	timing := pastRotation
	timing.Rotation = time.Second
	writerBlocked := make(chan struct{})
	releaseWriter := make(chan struct{})
	var block sync.Once
	h := newRotationHarness(t, timing, func(f *ownsandv1.DataFrame) {
		if f.GetStreamId() != 0 && f.GetRelayKeepalive() != nil {
			block.Do(func() {
				close(writerBlocked)
				<-releaseWriter
			})
		}
	})
	release := sync.OnceFunc(func() { close(releaseWriter) })
	t.Cleanup(release)
	h.call.stream.Started = time.Now()
	rotateAt := h.call.stream.Started.Add(timing.Rotation)
	ctx, cancel := context.WithDeadline(context.Background(), rotateAt.Add(time.Second))
	defer cancel()
	h.feed(&ownsandv1.RequestAccepted{})
	err := h.serve(ctx, func() {
		fillCtx, stop := context.WithTimeout(ctx, 100*time.Millisecond)
		defer stop()
		f := datapath.MustNewFrame(0, "", &ownsandv1.RelayKeepalive{})
		if err := h.call.stream.Send(fillCtx, f); err != nil {
			t.Fatalf("send the frame that gates the writer: %v", err)
		}
		select {
		case <-writerBlocked:
		case <-fillCtx.Done():
			t.Fatal("the writer did not reach the gate")
		}
		// Use a fresh frame for each send because Send sets its identifiers.
		filled := 0
		for {
			f := datapath.MustNewFrame(0, "", &ownsandv1.RelayKeepalive{})
			err := h.call.stream.Send(fillCtx, f)
			if errors.Is(err, context.DeadlineExceeded) {
				break
			}
			if err != nil {
				t.Fatalf("fill the agent connection's queue: %v", err)
			}
			filled++
		}
		if filled != 1024 {
			t.Fatalf("queued %d frames, want all 1024 ordinary slots filled", filled)
		}
		h.feed(stdout("before rotation"))
		if !time.Now().Before(rotateAt) {
			t.Fatal("the setup exceeded the rotation deadline")
		}
	})
	if err != nil {
		t.Fatalf("serveStream = %v, want clean rotation with credit pending", err)
	}
	if time.Now().Before(rotateAt) {
		t.Fatal("the stream ended before its rotation deadline")
	}
	if got := h.eventKinds(); len(got) != 2 || got[0] != "accepted" || got[1] != "stdout" {
		t.Fatalf("events = %v, want accepted and stdout without a final event", got)
	}
	if h.call.frames != 1 || h.call.bytes <= 0 {
		t.Fatalf("forwarded %d frames and %d bytes, want one payload frame", h.call.frames, h.call.bytes)
	}
	if got := h.logCode(); got != "ok" {
		t.Errorf("logged code = %q, want ok", got)
	}
	release()
	h.waitClose()
}

// queuedReplyContext exposes the error after the response check for the
// accepted event. This lets the test reach Next with the terminal reply queued.
type queuedReplyContext struct {
	context.Context
	err    error
	queued atomic.Bool
	checks atomic.Uint32
}

func (c *queuedReplyContext) Err() error {
	if c.queued.Load() && c.checks.Add(1) > 1 {
		return c.err
	}
	return c.Context.Err()
}

func TestRotationInFutureWithQueuedTerminalReply(t *testing.T) {
	timing := pastRotation
	timing.Rotation = 2 * time.Hour // the harness sets Started one hour in the past
	for _, tc := range []struct {
		name       string
		wantErr    error
		wantCode   string
		wantEvents []string
	}{
		{
			name:       "canceled",
			wantErr:    context.Canceled,
			wantCode:   "canceled",
			wantEvents: []string{"accepted"},
		},
		{
			name:       "deadline exceeded",
			wantErr:    context.DeadlineExceeded,
			wantCode:   "deadline_exceeded",
			wantEvents: []string{"accepted"},
		},
		{
			name:       "live",
			wantCode:   "ok",
			wantEvents: []string{"accepted", "exited"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newRotationHarness(t, timing, nil)
			h.feed(&ownsandv1.RequestAccepted{})
			ctx := &queuedReplyContext{Context: context.Background(), err: tc.wantErr}
			exited := &ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Exited{Exited: &ownsandv1.ExecExited{
				Status: &ownsandv1.ExecExited_ExitCode{ExitCode: 0},
			}}}
			err := h.serve(ctx, func() {
				h.feed(exited)
				ctx.queued.Store(tc.wantErr != nil)
			})
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("serveStream = %v, want %v", err, tc.wantErr)
			}
			if got := h.logCode(); got != tc.wantCode {
				t.Errorf("logged code = %q, want %q", got, tc.wantCode)
			}
			if got := h.eventKinds(); !slices.Equal(got, tc.wantEvents) {
				t.Errorf("events = %v, want %v", got, tc.wantEvents)
			}
			wantFrames, wantBytes := 0, int64(0)
			if tc.wantErr == nil {
				wantFrames = 1
				wantBytes = datapath.FrameCost(datapath.MustNewFrame(h.call.stream.ID, "sandbox", exited))
			}
			if h.call.frames != wantFrames || h.call.bytes != wantBytes {
				t.Errorf("forwarded %d frames and %d bytes, want %d frames and %d bytes", h.call.frames, h.call.bytes, wantFrames, wantBytes)
			}
			h.waitClose()
		})
	}
}
