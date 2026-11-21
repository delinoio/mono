// SPDX-License-Identifier: FSL-1.1-ALv2

package relay

import (
	"context"
	"errors"
	"strconv"
	"strings"
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

// fillOutbound fills the outbound queue so that an enqueue waits for room. It
// returns the number of filler frames.
func fillOutbound(c *Conn) int {
	n := 0
	for {
		select {
		case c.slots <- struct{}{}:
			c.out <- outFrame{frame: datapath.MustNewFrame(0, "", &ownsandv1.RelayKeepalive{})}
			n++
		default:
			return n
		}
	}
}

// takeOut dequeues the next frame like the writer does (without a send).
func takeOut(c *Conn) *ownsandv1.DataFrame {
	of := <-c.out
	c.dequeued(of)
	c.sent(of)
	return of.frame
}

// tryTakeOut is takeOut without waiting; ok is false when the queue is empty.
func tryTakeOut(c *Conn) (f *ownsandv1.DataFrame, ok bool) {
	select {
	case of := <-c.out:
		c.dequeued(of)
		c.sent(of)
		return of.frame, true
	default:
		return nil, false
	}
}

// openResult is the outcome of an Open that runs in its own goroutine.
type openResult struct {
	s   *Stream
	err error
}

func openAsync(ctx context.Context, c *Conn, grant string) <-chan openResult {
	ch := make(chan openResult, 1)
	go func() {
		s, err := c.Open(ctx, grant, "sandbox")
		ch <- openResult{s, err}
	}()
	return ch
}

func waitOpen(t *testing.T, ch <-chan openResult, within time.Duration) openResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(within):
		t.Fatal("Open did not return in time")
		return openResult{}
	}
}

func waitStreams(t *testing.T, c *Conn, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for c.StreamCount() != want {
		if time.Now().After(deadline) {
			t.Fatalf("StreamCount = %d, want %d", c.StreamCount(), want)
		}
		time.Sleep(time.Millisecond)
	}
}

func perGrantCount(c *Conn, grant string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.perGrant[grant]
}

// TestOpenEnqueuesInStreamIDOrderWhileQueueIsFull holds the first Open in the
// outbound queue wait, starts a second Open, and then drains the queue: the
// stream open frames must leave in stream_id order. Without the open gate the
// second Open allocates stream_id 2 at once and both race for the first free
// slot.
func TestOpenEnqueuesInStreamIDOrderWhileQueueIsFull(t *testing.T) {
	c := NewConn(context.Background(), "device", 1, testLimits)
	filler := fillOutbound(c)
	first := openAsync(context.Background(), c, "grant-a")
	waitStreams(t, c, 1)
	second := openAsync(context.Background(), c, "grant-b")
	// The second Open waits for the gate and holds no stream slot or ID.
	time.Sleep(50 * time.Millisecond)
	if got := c.StreamCount(); got != 1 {
		t.Fatalf("StreamCount while the first Open waits = %d, want 1", got)
	}
	for i := 0; i < filler; i++ {
		takeOut(c)
	}
	a, b := waitOpen(t, first, time.Second), waitOpen(t, second, time.Second)
	if a.err != nil || b.err != nil {
		t.Fatalf("Open errors: %v, %v", a.err, b.err)
	}
	if a.s.ID != 1 || b.s.ID != 2 {
		t.Fatalf("stream IDs = %d, %d, want 1, 2", a.s.ID, b.s.ID)
	}
	for _, want := range []uint64{1, 2} {
		f, ok := tryTakeOut(c)
		if !ok {
			t.Fatalf("stream open %d is not queued", want)
		}
		if f.GetStreamId() != want || f.GetRelayStreamOpen() == nil {
			t.Fatalf("queued frame = stream %d kind %v, want stream open %d", f.GetStreamId(), datapath.PayloadKind(f), want)
		}
	}
}

// TestConcurrentOpensReachTheAgentInIDOrder runs many concurrent opens
// through the writer and applies the agent's rule: it drops a stream open
// frame whose stream_id is not above the highest one it has seen. Every open
// must be accepted (the agent grants initial credit for it).
func TestConcurrentOpensReachTheAgentInIDOrder(t *testing.T) {
	const opens = 64
	limits := testLimits
	limits.MaxStreamsPerConn = opens
	limits.IdleTimeout = time.Minute
	limits.Keepalive = time.Minute
	c := NewConn(context.Background(), "device", 1, limits)
	var mu sync.Mutex
	var highest uint64
	var accepted, dropped int
	send := func(f *ownsandv1.DataFrame) error {
		if f.GetRelayStreamOpen() == nil {
			return nil
		}
		mu.Lock()
		defer mu.Unlock()
		if f.GetStreamId() <= highest {
			dropped++
			return nil
		}
		highest = f.GetStreamId()
		accepted++
		return nil
	}
	done := runConn(c, blockingRecv(c), send)
	var wg sync.WaitGroup
	errs := make(chan error, opens)
	for i := 0; i < opens; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := c.Open(context.Background(), "grant-"+string(rune('a'+i%26))+string(rune('a'+i/26)), "sandbox")
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := accepted + dropped
		mu.Unlock()
		if n == opens {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("writer sent %d of %d stream open frames", n, opens)
		}
		time.Sleep(time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if dropped != 0 || accepted != opens {
		t.Fatalf("agent accepted %d and dropped %d stream open frames, want %d and 0", accepted, dropped, opens)
	}
	c.Close(ErrConnClosed)
	waitRun(t, done, time.Second)
}

// TestOpenCancelWhileQueueIsFull cancels an Open that waits for room in a full
// outbound queue and an Open that waits for the gate: both return at once and
// release their grant and connection slots, and a later Open gets a fresh,
// larger stream_id.
func TestOpenCancelWhileQueueIsFull(t *testing.T) {
	c := NewConn(context.Background(), "device", 1, testLimits)
	filler := fillOutbound(c)
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	defer cancelFirst()
	first := openAsync(firstCtx, c, "grant-a")
	waitStreams(t, c, 1)
	secondCtx, cancelSecond := context.WithCancel(context.Background())
	defer cancelSecond()
	second := openAsync(secondCtx, c, "grant-b")
	time.Sleep(20 * time.Millisecond)

	cancelSecond()
	if r := waitOpen(t, second, time.Second); !errors.Is(r.err, context.Canceled) {
		t.Fatalf("canceled gate waiter = %v, want context.Canceled", r.err)
	}
	if got := c.StreamCount(); got != 1 {
		t.Fatalf("StreamCount after the gate waiter canceled = %d, want 1", got)
	}
	if got := perGrantCount(c, "grant-b"); got != 0 {
		t.Fatalf("grant-b streams = %d, want 0", got)
	}

	cancelFirst()
	if r := waitOpen(t, first, time.Second); !errors.Is(r.err, context.Canceled) {
		t.Fatalf("canceled enqueue waiter = %v, want context.Canceled", r.err)
	}
	if got := c.StreamCount(); got != 0 {
		t.Fatalf("StreamCount after both canceled = %d, want 0", got)
	}
	if got := perGrantCount(c, "grant-a"); got != 0 {
		t.Fatalf("grant-a streams = %d, want 0", got)
	}

	for i := 0; i < filler; i++ {
		takeOut(c)
	}
	s, err := c.Open(context.Background(), "grant-a", "sandbox")
	if err != nil {
		t.Fatal(err)
	}
	if s.ID <= 1 {
		t.Fatalf("stream_id after a canceled Open = %d, want above the canceled stream_id 1", s.ID)
	}
	// The canceled stream's open frame never reached the queue.
	f := takeOut(c)
	if f.GetStreamId() != s.ID {
		t.Fatalf("first queued frame has stream_id %d, want %d", f.GetStreamId(), s.ID)
	}
}

func TestOpenReturnsWhenConnectionClosesWhileWaitingForGate(t *testing.T) {
	c := NewConn(context.Background(), "device", 1, testLimits)
	fillOutbound(c)
	first := openAsync(context.Background(), c, "grant-a")
	waitStreams(t, c, 1)
	second := openAsync(context.Background(), c, "grant-b")
	time.Sleep(20 * time.Millisecond)
	c.Close(ErrConnClosed)
	for _, ch := range []<-chan openResult{first, second} {
		if r := waitOpen(t, ch, time.Second); !errors.Is(r.err, ErrConnClosed) {
			t.Fatalf("Open after Close = %v, want ErrConnClosed", r.err)
		}
	}
}

// TestCreditWaitDoesNotDelayAnotherStream shows that a sender that waits for
// the agent's credit does not hold the open gate or the queue.
func TestCreditWaitDoesNotDelayAnotherStream(t *testing.T) {
	c := NewConn(context.Background(), "device", 1, testLimits)
	waiting := openStream(t, c)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sent := make(chan error, 1)
	go func() {
		sent <- waiting.Send(ctx, datapath.MustNewFrame(0, "", &ownsandv1.ExecRequest{}))
	}()
	time.Sleep(20 * time.Millisecond) // the agent granted no credit
	other, err := c.Open(context.Background(), "grant-b", "sandbox")
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Send(context.Background(), datapath.MustNewFrame(0, "", &ownsandv1.RelayKeepalive{})); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-sent:
		t.Fatalf("Send returned %v before the agent granted credit", err)
	default:
	}
	cancel()
	if err := <-sent; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled credit wait = %v, want context.Canceled", err)
	}
}

// TestLastLargeReceiveCountsOnlyLargeStreamFrames checks which frames extend
// the wait for a request accepted notice: only a large frame of an open
// stream does.
func TestLastLargeReceiveCountsOnlyLargeStreamFrames(t *testing.T) {
	c := NewConn(context.Background(), "device", 1, testLimits)
	s := openStream(t, c)
	if !c.LastLargeReceive().IsZero() {
		t.Fatal("a new connection has a large receive time")
	}
	stdout := func(streamID uint64, size int) *ownsandv1.DataFrame {
		return datapath.MustNewFrame(streamID, "sandbox", &ownsandv1.ExecEvent{
			Event: &ownsandv1.ExecEvent_Stdout{Stdout: &ownsandv1.ExecOutput{Data: make([]byte, size)}}})
	}
	for name, f := range map[string]*ownsandv1.DataFrame{
		"keepalive":          datapath.MustNewFrame(0, "", &ownsandv1.RelayKeepalive{}),
		"small stream frame": stdout(s.ID, 1024),
		"closed stream":      stdout(s.ID+100, 2*LargeFrameBytes),
	} {
		if err := c.dispatch(f); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !c.LastLargeReceive().IsZero() {
			t.Fatalf("%s counted as a large frame", name)
		}
	}
	if err := c.dispatch(stdout(s.ID, 2*LargeFrameBytes)); err != nil {
		t.Fatal(err)
	}
	if c.LastLargeReceive().IsZero() {
		t.Fatal("a large stream frame was not counted")
	}
}

// drainOut returns the frames waiting in the outbound queue.
func drainOut(c *Conn) []*ownsandv1.DataFrame {
	var frames []*ownsandv1.DataFrame
	for {
		f, ok := tryTakeOut(c)
		if !ok {
			return frames
		}
		frames = append(frames, f)
	}
}

func TestUnknownKindOnAStreamSendsOneErrorFrameAndClosesOnlyThatStream(t *testing.T) {
	c := NewConn(context.Background(), "device", 1, testLimits)
	a, b := openStream(t, c), openStream(t, c)
	if got := len(drainOut(c)); got != 2 {
		t.Fatalf("queued stream open frames = %d, want 2", got)
	}

	if err := c.dispatch(&ownsandv1.DataFrame{StreamId: a.ID, SandboxId: "sandbox", Kind: ownsandv1.DataFrameKind(99)}); err != nil {
		t.Fatalf("dispatch of an unknown kind closed the connection: %v", err)
	}
	if _, err := a.Next(context.Background(), nil); !errors.Is(err, ErrUnknownKind) {
		t.Fatalf("stream error = %v, want ErrUnknownKind", err)
	}
	// The RPC ends and closes the stream; that adds no stream close frame
	// behind the error frame.
	a.Close(true)
	frames := drainOut(c)
	if len(frames) != 1 {
		t.Fatalf("frames after the unknown kind = %d, want one error frame", len(frames))
	}
	e := frames[0]
	if e.GetStreamId() != a.ID || e.GetSandboxId() != "sandbox" || e.GetKind() != ownsandv1.DataFrameKind_DATA_FRAME_KIND_ERROR ||
		e.GetError().GetCode() != ownsandv1.ErrorCode_ERROR_CODE_UNIMPLEMENTED {
		t.Fatalf("frame = %v, want an UNIMPLEMENTED error frame on stream %d", e, a.ID)
	}
	if msg := e.GetError().GetMessage(); !strings.Contains(msg, "kind 99") || !strings.Contains(msg, "stream "+strconv.FormatUint(a.ID, 10)) {
		t.Fatalf("message %q does not name the kind and the stream", msg)
	}
	if got := c.StreamCount(); got != 1 {
		t.Fatalf("StreamCount = %d, want 1", got)
	}

	// The other stream keeps working.
	out := datapath.MustNewFrame(b.ID, "sandbox", &ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Stdout{Stdout: &ownsandv1.ExecOutput{Data: []byte("x")}}})
	if err := c.dispatch(out); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if f, err := b.Next(ctx, nil); err != nil || f.GetExecEvent().GetStdout() == nil {
		t.Fatalf("other stream: %v, %v", f, err)
	}
	// A late frame of the closed stream is dropped without a reply.
	if err := c.dispatch(&ownsandv1.DataFrame{StreamId: a.ID, Kind: ownsandv1.DataFrameKind(99)}); err != nil {
		t.Fatal(err)
	}
	if frames := drainOut(c); len(frames) != 0 {
		t.Fatalf("a frame of a closed stream got %d replies", len(frames))
	}
}

func TestUnknownKindOnStreamZeroIsIgnored(t *testing.T) {
	c := NewConn(context.Background(), "device", 1, testLimits)
	s := openStream(t, c)
	drainOut(c)
	if err := c.dispatch(&ownsandv1.DataFrame{Kind: ownsandv1.DataFrameKind(99)}); err != nil {
		t.Fatalf("unknown kind on stream 0: %v", err)
	}
	if frames := drainOut(c); len(frames) != 0 {
		t.Fatalf("unknown kind on stream 0 queued %d frames", len(frames))
	}
	if err := c.dispatch(datapath.MustNewFrame(0, "", &ownsandv1.RelayKeepalive{EchoRequested: true})); err != nil {
		t.Fatal(err)
	}
	if frames := drainOut(c); len(frames) != 1 || frames[0].GetRelayKeepalive() == nil {
		t.Fatalf("keepalive echo after the unknown kind = %v", frames)
	}
	if c.StreamCount() != 1 {
		t.Fatalf("StreamCount = %d, want 1", c.StreamCount())
	}
	// Ordinary stream close is unchanged: it sends a stream close frame.
	s.Close(true)
	if frames := drainOut(c); len(frames) != 1 || frames[0].GetRelayStreamClose() == nil || frames[0].GetStreamId() != s.ID {
		t.Fatalf("ordinary close frames = %v, want one stream close frame", frames)
	}
}

func TestRegistryRejectsStaleChannelWithoutClosingCurrent(t *testing.T) {
	r := NewRegistry()
	current := NewConn(context.Background(), "device", 2, testLimits)
	if err := r.Register(current, func() bool { return true }); err != nil {
		t.Fatal(err)
	}
	stale := NewConn(context.Background(), "device", 1, testLimits)
	if err := r.Register(stale, func() bool { return false }); !errors.Is(err, ErrStaleChannel) {
		t.Fatalf("stale Register = %v, want ErrStaleChannel", err)
	}
	select {
	case <-current.Done():
		t.Fatal("a stale registration closed the current connection")
	default:
	}
	if got := r.Get("device", 2); got != current {
		t.Fatal("the current connection is no longer registered")
	}
	if got := r.Get("device", 1); got != nil {
		t.Fatal("the stale connection was registered")
	}
}

func TestRegistryReplacesSameGenerationConnection(t *testing.T) {
	r := NewRegistry()
	first := NewConn(context.Background(), "device", 3, testLimits)
	second := NewConn(context.Background(), "device", 3, testLimits)
	ok := func() bool { return true }
	if err := r.Register(first, ok); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(second, ok); err != nil {
		t.Fatal(err)
	}
	select {
	case <-first.Done():
	case <-time.After(time.Second):
		t.Fatal("the replaced connection stayed open")
	}
	if got := r.Get("device", 3); got != second {
		t.Fatal("the newer connection is not current")
	}
}
