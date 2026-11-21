// SPDX-License-Identifier: FSL-1.1-ALv2

package relay

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/delinoio/mono/go/ownsand/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

var testLimits = Limits{
	StreamBuffer:       datapath.MinWindowSize,
	MaxStreamsPerConn:  4,
	MaxStreamsPerGrant: 4,
	Keepalive:          30 * time.Millisecond,
	IdleTimeout:        100 * time.Millisecond,
}

// runConn starts c.Run and returns a channel with its result.
func runConn(c *Conn, recv func() (*ownsandv1.DataFrame, error), send func(*ownsandv1.DataFrame) error) <-chan error {
	done := make(chan error, 1)
	go func() { done <- c.Run(recv, send) }()
	return done
}

// blockedSend returns a send that blocks until the returned release function
// runs, like a stream.Send stalled on flow control.
func blockedSend(t *testing.T) (send func(*ownsandv1.DataFrame) error, entered <-chan struct{}) {
	t.Helper()
	release := make(chan struct{})
	enteredCh := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { close(release) })
	return func(*ownsandv1.DataFrame) error {
		once.Do(func() { close(enteredCh) })
		<-release
		return nil
	}, enteredCh
}

// blockingRecv returns a recv that blocks until the connection closes.
func blockingRecv(c *Conn) func() (*ownsandv1.DataFrame, error) {
	return func() (*ownsandv1.DataFrame, error) {
		<-c.Done()
		return nil, c.Err()
	}
}

func waitRun(t *testing.T, done <-chan error, within time.Duration) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(within):
		t.Fatal("Run did not return in time")
		return nil
	}
}

func TestRunClosesIdleConnectionWhileSendBlocks(t *testing.T) {
	c := NewConn(context.Background(), "device", 1, testLimits)
	send, entered := blockedSend(t)
	done := runConn(c, blockingRecv(c), send)
	<-entered
	s, err := c.Open(context.Background(), "grant", "sandbox")
	if err != nil {
		t.Fatal(err)
	}
	if err := waitRun(t, done, time.Second); !errors.Is(err, ErrIdle) {
		t.Fatalf("Run = %v, want ErrIdle", err)
	}
	if _, err := s.Next(context.Background(), nil); !errors.Is(err, ErrIdle) {
		t.Fatalf("stream error = %v, want ErrIdle", err)
	}
	if c.StreamCount() != 0 {
		t.Fatalf("open streams = %d, want 0", c.StreamCount())
	}
}

func TestRunClosesSilentConnectionWithHealthySend(t *testing.T) {
	c := NewConn(context.Background(), "device", 1, testLimits)
	done := runConn(c, blockingRecv(c), func(*ownsandv1.DataFrame) error { return nil })
	if err := waitRun(t, done, time.Second); !errors.Is(err, ErrIdle) {
		t.Fatalf("Run = %v, want ErrIdle", err)
	}
}

func TestRunKeepsConnectionThatReceivesFrames(t *testing.T) {
	c := NewConn(context.Background(), "device", 1, testLimits)
	var mu sync.Mutex
	keepalives := 0
	send := func(f *ownsandv1.DataFrame) error {
		if f.GetRelayKeepalive() != nil {
			mu.Lock()
			keepalives++
			mu.Unlock()
		}
		return nil
	}
	recv := func() (*ownsandv1.DataFrame, error) {
		select {
		case <-c.Done():
			return nil, c.Err()
		case <-time.After(testLimits.Keepalive):
			return datapath.MustNewFrame(0, "", &ownsandv1.RelayKeepalive{}), nil
		}
	}
	done := runConn(c, recv, send)
	select {
	case err := <-done:
		t.Fatalf("Run returned early: %v", err)
	case <-time.After(time.Second):
	}
	c.Close(ErrConnClosed)
	waitRun(t, done, time.Second)
	mu.Lock()
	defer mu.Unlock()
	if keepalives == 0 {
		t.Fatal("no keepalive was sent without outbound frames")
	}
}

func TestRunReturnsOnCloseWhileSendBlocks(t *testing.T) {
	for name, cause := range map[string]error{
		"replacement":    ErrReplaced,
		"channel closed": errors.New("the agent channel closed"),
		"shutdown":       context.Canceled,
	} {
		t.Run(name, func(t *testing.T) {
			// A long idle timeout keeps the idle check out of the way.
			limits := testLimits
			limits.IdleTimeout = time.Minute
			c := NewConn(context.Background(), "device", 1, limits)
			send, entered := blockedSend(t)
			done := runConn(c, blockingRecv(c), send)
			<-entered
			c.Close(cause)
			if err := waitRun(t, done, time.Second); !errors.Is(err, cause) {
				t.Fatalf("Run = %v, want %v", err, cause)
			}
		})
	}
}

func TestRunDoesNotSendAfterClose(t *testing.T) {
	c := NewConn(context.Background(), "device", 1, testLimits)
	var mu sync.Mutex
	closed := false
	late := 0
	send := func(*ownsandv1.DataFrame) error {
		mu.Lock()
		defer mu.Unlock()
		if closed {
			late++
		}
		return nil
	}
	done := runConn(c, blockingRecv(c), send)
	waitRun(t, done, time.Second)
	mu.Lock()
	closed = true
	mu.Unlock()
	time.Sleep(3 * testLimits.Keepalive)
	mu.Lock()
	defer mu.Unlock()
	if late != 0 {
		t.Fatalf("%d sends started after Run returned", late)
	}
}

func acceptedFrame(s *Stream) *ownsandv1.DataFrame {
	return datapath.MustNewFrame(s.ID, s.SandboxID, &ownsandv1.RequestAccepted{})
}

func openStream(t *testing.T, c *Conn) *Stream {
	t.Helper()
	s, err := c.Open(context.Background(), "grant", "sandbox")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestStreamQueueBoundsFramesWithoutCredit(t *testing.T) {
	limits := testLimits
	limits.MaxQueuedControlFrames = 3
	c := NewConn(context.Background(), "device", 1, limits)
	s := openStream(t, c)
	for i := 0; i < limits.MaxQueuedControlFrames; i++ {
		if err := c.dispatch(acceptedFrame(s)); err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
	}
	if err := c.dispatch(acceptedFrame(s)); !errors.Is(err, ErrControlFlood) {
		t.Fatalf("frame beyond the limit = %v, want ErrControlFlood", err)
	}
	// Dequeuing a frame frees a slot.
	if _, err := s.Next(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if err := c.dispatch(acceptedFrame(s)); err != nil {
		t.Fatalf("frame after a dequeue: %v", err)
	}
	// The count belongs to the stream: another stream has its own budget.
	other := openStream(t, c)
	if err := c.dispatch(acceptedFrame(other)); err != nil {
		t.Fatalf("frame on another stream: %v", err)
	}
}

func TestStreamQueueBoundsBytesWithoutCredit(t *testing.T) {
	limits := testLimits
	limits.MaxQueuedControlFrames = 100
	limits.MaxQueuedControlBytes = 1 << 10
	c := NewConn(context.Background(), "device", 1, limits)
	s := openStream(t, c)
	big := datapath.MustNewFrame(s.ID, s.SandboxID, &ownsandv1.ErrorDetail{Message: string(make([]byte, 2<<10))})
	if err := c.dispatch(big); !errors.Is(err, ErrControlFlood) {
		t.Fatalf("frame beyond the byte limit = %v, want ErrControlFlood", err)
	}
	small := datapath.MustNewFrame(s.ID, s.SandboxID, &ownsandv1.ErrorDetail{Message: string(make([]byte, 600))})
	if err := c.dispatch(small); err != nil {
		t.Fatalf("first frame within the limit: %v", err)
	}
	if err := c.dispatch(small); !errors.Is(err, ErrControlFlood) {
		t.Fatalf("second frame beyond the byte limit = %v, want ErrControlFlood", err)
	}
	if _, err := s.Next(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if err := c.dispatch(small); err != nil {
		t.Fatalf("frame after a dequeue: %v", err)
	}
}

func TestCompliantExchangeStaysWithinDefaultLimits(t *testing.T) {
	c := NewConn(context.Background(), "device", 1, DefaultLimits())
	s := openStream(t, c)
	// Nobody dequeues: a slow handler holds the whole exchange in the queue.
	if err := c.dispatch(acceptedFrame(s)); err != nil {
		t.Fatal(err)
	}
	// Credit-bearing frames are bounded by the receive window, not by the
	// control limits.
	for i := 0; i < 1000; i++ {
		f := datapath.MustNewFrame(s.ID, s.SandboxID, &ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Stdout{Stdout: &ownsandv1.ExecOutput{Data: []byte("x")}}})
		if err := c.dispatch(f); err != nil {
			t.Fatalf("credit frame %d: %v", i, err)
		}
	}
	end := datapath.MustNewFrame(s.ID, s.SandboxID, &ownsandv1.SessionEnd{})
	if err := c.dispatch(end); err != nil {
		t.Fatalf("terminal frame: %v", err)
	}
}

func TestDefaultLimitsApplyToZeroValues(t *testing.T) {
	c := NewConn(context.Background(), "device", 1, Limits{StreamBuffer: datapath.MinWindowSize, MaxStreamsPerConn: 1, MaxStreamsPerGrant: 1})
	if c.limits.MaxQueuedControlFrames != DefaultLimits().MaxQueuedControlFrames || c.limits.MaxQueuedControlBytes != DefaultLimits().MaxQueuedControlBytes {
		t.Fatalf("limits = %+v, want the defaults", c.limits)
	}
}

func TestRunClosesConnectionOnControlFlood(t *testing.T) {
	limits := testLimits
	limits.IdleTimeout = time.Minute
	limits.MaxQueuedControlFrames = 3
	c := NewConn(context.Background(), "device", 1, limits)
	s := openStream(t, c)
	frames := make(chan *ownsandv1.DataFrame, limits.MaxQueuedControlFrames+1)
	for i := 0; i <= limits.MaxQueuedControlFrames; i++ {
		frames <- acceptedFrame(s)
	}
	recv := func() (*ownsandv1.DataFrame, error) {
		select {
		case f := <-frames:
			return f, nil
		case <-c.Done():
			return nil, c.Err()
		}
	}
	done := runConn(c, recv, func(*ownsandv1.DataFrame) error { return nil })
	if err := waitRun(t, done, time.Second); !errors.Is(err, ErrControlFlood) {
		t.Fatalf("Run = %v, want ErrControlFlood", err)
	}
	if _, err := s.Next(context.Background(), nil); err != nil && !errors.Is(err, ErrControlFlood) {
		// Queued frames drain first; the stream error follows.
		t.Fatalf("stream error = %v", err)
	}
}
