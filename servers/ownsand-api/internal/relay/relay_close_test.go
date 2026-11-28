// SPDX-License-Identifier: FSL-1.1-ALv2

package relay

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/delinoio/mono/go/ownsand/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// closeTestLimits has long timers, so that no keepalive or idle close
// interferes, and room for two streams on a connection and a grant each.
var closeTestLimits = Limits{
	StreamBuffer:       datapath.MinWindowSize,
	MaxStreamsPerConn:  2,
	MaxStreamsPerGrant: 1,
	Keepalive:          time.Hour,
	IdleTimeout:        time.Hour,
}

func pendingFinals(c *Conn) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pendingFinals
}

const (
	closeAssertionTimeout     = time.Second
	relayWorkerCleanupTimeout = 2 * time.Second
)

func waitForRelayTestWorker(done <-chan struct{}, timeout time.Duration) bool {
	select {
	case <-done:
		return true
	case <-time.After(timeout):
	}
	// The timer and the completion signal can both be ready.
	select {
	case <-done:
		return true
	default:
		return false
	}
}

// closeReturnsPromptly runs s.Close and fails when it blocks.
func closeReturnsPromptly(t *testing.T, s *Stream, sendClose bool) {
	t.Helper()
	if !runCloseTestWorker(func() { s.Close(sendClose) }, t.Cleanup, func() {
		t.Error("Stream.Close worker was still running at the cleanup deadline")
	}) {
		t.Fatal("Stream.Close blocked while the outbound queue was full")
	}
}

func runCloseTestWorker(closeStream func(), registerCleanup func(func()), reportStuck func()) bool {
	deadline := time.Now().Add(closeAssertionTimeout)
	done := make(chan struct{})
	registerCleanup(func() {
		if !waitForRelayTestWorker(done, relayWorkerCleanupTimeout) {
			reportStuck()
		}
	})

	var returnedPromptly bool
	go func() {
		defer close(done)
		// Check the deadline here too in case this worker starts after
		// the assertion times out.
		if time.Now().Before(deadline) {
			closeStream()
			returnedPromptly = time.Now().Before(deadline)
		}
	}()
	return waitForRelayTestWorker(done, time.Until(deadline)) && returnedPromptly
}

func TestCloseWorkerSkipsExpiredInvocation(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		called := make(chan struct{})
		var cleanup func()
		if runCloseTestWorker(func() { close(called) }, func(fn func()) {
			cleanup = fn
			time.Sleep(closeAssertionTimeout)
		}, func() {
			t.Error("cleanup timed out waiting for the pending close worker")
		}) {
			t.Error("close reported success after its deadline")
		}
		cleanup()
		select {
		case <-called:
			t.Error("close callback started after its deadline")
		default:
		}
	})
}

func TestCloseWorkerCleanupAfterTimeout(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		wantStuck bool
	}{
		{name: "released during cleanup"},
		{name: "blocked past cleanup deadline", wantStuck: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				entered := make(chan struct{})
				release := make(chan struct{})
				finished := make(chan struct{})
				releaseClose := sync.OnceFunc(func() { close(release) })
				defer releaseClose()

				var cleanup func()
				stuck := false
				if runCloseTestWorker(func() {
					close(entered)
					<-release
					close(finished)
				}, func(fn func()) { cleanup = fn }, func() { stuck = true }) {
					t.Error("close reported success while its callback was blocked")
				}
				select {
				case <-entered:
				default:
					t.Fatal("close callback did not start")
				}
				if !tc.wantStuck {
					go func() {
						time.Sleep(relayWorkerCleanupTimeout / 2)
						releaseClose()
					}()
				}

				cleanup()
				if stuck != tc.wantStuck {
					t.Errorf("cleanup reported a stuck worker = %t, want %t", stuck, tc.wantStuck)
				}
				if !tc.wantStuck {
					select {
					case <-finished:
					default:
						t.Error("cleanup returned before the close callback finished")
					}
				}
				releaseClose()
				<-finished
			})
		})
	}
}

// sentFrames is a send function that records every frame, optionally blocking
// on gate first.
type sentFrames struct {
	mu     sync.Mutex
	frames []*ownsandv1.DataFrame
	notify chan struct{}
}

func newSentFrames() *sentFrames { return &sentFrames{notify: make(chan struct{}, 1024)} }

func (r *sentFrames) send(f *ownsandv1.DataFrame) error {
	r.mu.Lock()
	r.frames = append(r.frames, f)
	r.mu.Unlock()
	r.notify <- struct{}{}
	return nil
}

func (r *sentFrames) snapshot() []*ownsandv1.DataFrame {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*ownsandv1.DataFrame(nil), r.frames...)
}

// TestCloseWithFullQueueIsDeliveredAfterTheQueuedFrames fills the outbound
// queue while the writer does not run, closes a stream, and then starts the
// writer: Close returns at once, and the writer sends the queued frames and
// then exactly one stream close frame of that stream.
func TestCloseWithFullQueueIsDeliveredAfterTheQueuedFrames(t *testing.T) {
	c := NewConn(context.Background(), "device", 1, closeTestLimits)
	s, err := c.Open(context.Background(), "grant-a", "sandbox")
	if err != nil {
		t.Fatal(err)
	}
	filler := fillOutbound(c)
	if filler != outboundQueue-1 {
		t.Fatalf("filler = %d, want %d (one place holds the stream open frame)", filler, outboundQueue-1)
	}

	closeReturnsPromptly(t, s, true)
	// Ordinary frames still wait for room; only the final owns a reserved place.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := c.enqueue(ctx, datapath.MustNewFrame(0, "", &ownsandv1.RelayKeepalive{})); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ordinary enqueue with a full queue = %v, want deadline exceeded", err)
	}
	if got := len(c.out); got != outboundQueue+1 {
		t.Fatalf("queued frames = %d, want %d (the ordinary queue plus the close frame)", got, outboundQueue+1)
	}

	rec := newSentFrames()
	done := runConn(c, blockingRecv(c), rec.send)
	defer func() {
		c.Close(ErrConnClosed)
		waitRun(t, done, 2*time.Second)
	}()
	// The queued frames, the close frame and the supported kinds frame that Run
	// enqueues once there is room.
	want := 1 + filler + 1 + 1
	for i := 0; i < want; i++ {
		select {
		case <-rec.notify:
		case <-time.After(5 * time.Second):
			t.Fatalf("writer sent %d of %d frames", i, want)
		}
	}
	frames := rec.snapshot()
	closeAt := -1
	for i, f := range frames {
		if f.GetRelayStreamClose() == nil {
			continue
		}
		if closeAt >= 0 {
			t.Fatalf("second stream close frame at %d (first at %d)", i, closeAt)
		}
		if f.GetStreamId() != s.ID {
			t.Fatalf("stream close frame for stream %d, want %d", f.GetStreamId(), s.ID)
		}
		closeAt = i
	}
	if closeAt != 1+filler {
		t.Fatalf("stream close frame at %d, want %d (behind the stream open frame and the %d queued frames)", closeAt, 1+filler, filler)
	}
	if frames[0].GetRelayStreamOpen() == nil {
		t.Fatalf("first frame = %v, want the stream open frame", datapath.PayloadKind(frames[0]))
	}
	if got := pendingFinals(c); got != 0 {
		t.Fatalf("pending finals after the writer sent the close = %d, want 0", got)
	}
}

// TestPendingClosesCountAgainstAdmission checks that a stream whose close
// frame waits for the writer keeps its connection and grant slots, that the
// pending closes stay bounded, and that the slots return when the writer
// sends the close frames.
func TestPendingClosesCountAgainstAdmission(t *testing.T) {
	c := NewConn(context.Background(), "device", 1, closeTestLimits)
	a, err := c.Open(context.Background(), "grant-a", "sandbox")
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.Open(context.Background(), "grant-b", "sandbox")
	if err != nil {
		t.Fatal(err)
	}
	filler := fillOutbound(c)

	closeReturnsPromptly(t, a, true)
	closeReturnsPromptly(t, b, true)
	closeReturnsPromptly(t, a, true) // closing twice sends and reserves nothing more
	if got := c.StreamCount(); got != 0 {
		t.Fatalf("open streams = %d, want 0", got)
	}
	if got := pendingFinals(c); got != 2 {
		t.Fatalf("pending finals = %d, want 2", got)
	}
	if got, limit := len(c.out), outboundQueue+closeTestLimits.MaxStreamsPerConn; got > limit {
		t.Fatalf("queued frames = %d, above the bound %d", got, limit)
	}
	// Both connection slots are held by pending closes, so no stream is admitted
	// (also not for a grant without streams).
	if _, err := c.Open(context.Background(), "grant-c", "sandbox"); !errors.Is(err, ErrTooManyStreams) {
		t.Fatalf("Open while two closes are pending = %v, want ErrTooManyStreams", err)
	}
	if got := perGrantCount(c, "grant-a"); got != 1 {
		t.Fatalf("grant-a slots with a pending close = %d, want 1", got)
	}

	// Drain the queue like the writer: the open frames, the fillers, then the
	// two close frames; each close releases its slots.
	for i := 0; i < 2+filler; i++ {
		f := takeOut(c)
		if f.GetRelayStreamClose() != nil {
			t.Fatalf("stream close frame at %d, before the queued frames", i)
		}
	}
	if got := pendingFinals(c); got != 2 {
		t.Fatalf("pending finals before the close frames left = %d, want 2", got)
	}
	closed := map[uint64]bool{}
	for i := 0; i < 2; i++ {
		f := takeOut(c)
		if f.GetRelayStreamClose() == nil {
			t.Fatalf("frame %d after the queued frames = %v, want a stream close", i, datapath.PayloadKind(f))
		}
		closed[f.GetStreamId()] = true
	}
	if !closed[a.ID] || !closed[b.ID] || len(closed) != 2 {
		t.Fatalf("closed streams = %v, want %d and %d once each", closed, a.ID, b.ID)
	}
	if got := pendingFinals(c); got != 0 {
		t.Fatalf("pending finals after the writer sent the closes = %d, want 0", got)
	}
	if got := perGrantCount(c, "grant-a"); got != 0 {
		t.Fatalf("grant-a slots after the close was sent = %d, want 0", got)
	}
	if _, err := c.Open(context.Background(), "grant-a", "sandbox"); err != nil {
		t.Fatalf("Open after the close frames were sent = %v", err)
	}
}

// TestPendingCloseCountsAgainstGrantLimit checks the per-grant admission.
func TestPendingCloseCountsAgainstGrantLimit(t *testing.T) {
	c := NewConn(context.Background(), "device", 1, closeTestLimits)
	s, err := c.Open(context.Background(), "grant-a", "sandbox")
	if err != nil {
		t.Fatal(err)
	}
	fillOutbound(c)
	closeReturnsPromptly(t, s, true)
	if _, err := c.Open(context.Background(), "grant-a", "sandbox"); !errors.Is(err, ErrTooManyStreams) {
		t.Fatalf("Open on a grant with a pending close = %v, want ErrTooManyStreams", err)
	}
}

func TestFinalFrameHoldsAdmissionSlotsUntilSendCompletes(t *testing.T) {
	t.Parallel()
	const (
		closingGrant = "grant-a"
		wait         = 5 * time.Second
	)
	for _, tc := range []struct {
		name              string
		maxStreamsPerConn int
		retryGrant        string
	}{
		{name: "connection limit", maxStreamsPerConn: 1, retryGrant: "grant-b"},
		{name: "grant limit", maxStreamsPerConn: 2, retryGrant: closingGrant},
	} {
		t.Run(tc.name, func(t *testing.T) {
			limits := closeTestLimits
			limits.MaxStreamsPerConn = tc.maxStreamsPerConn
			c := NewConn(context.Background(), "device", 1, limits)
			open := func(grant string) (*Stream, error) {
				ctx, cancel := context.WithTimeout(t.Context(), wait)
				defer cancel()
				return c.Open(ctx, grant, "sandbox")
			}
			s, err := open(closingGrant)
			if err != nil {
				t.Fatalf("Open stream to close: %v", err)
			}
			closeReturnsPromptly(t, s, true)

			closeEntered := make(chan struct{})
			closeReleased := make(chan struct{})
			kindsEntered := make(chan struct{})
			releaseClose := sync.OnceFunc(func() { close(closeReleased) })
			var done <-chan error
			t.Cleanup(func() {
				releaseClose()
				c.Close(ErrConnClosed)
				waitRun(t, done, wait)
			})
			done = runConn(c, blockingRecv(c), func(f *ownsandv1.DataFrame) error {
				switch {
				case f.GetStreamId() == s.ID && f.GetRelayStreamClose() != nil:
					close(closeEntered)
					<-closeReleased
				case f.GetSupportedFrameKinds() != nil:
					close(kindsEntered)
				}
				return nil
			})

			select {
			case <-closeEntered:
			case <-time.After(wait):
				t.Fatal("writer did not enter the close send")
			}
			select {
			case <-kindsEntered:
				t.Fatal("writer sent supported frame kinds before the close frame")
			default:
			}
			if _, err := open(tc.retryGrant); !errors.Is(err, ErrTooManyStreams) {
				t.Fatalf("Open during the close send = %v, want ErrTooManyStreams", err)
			}

			releaseClose()
			// Run queues supported frame kinds behind the close. The writer
			// accounts for the close before it starts the next send.
			select {
			case <-kindsEntered:
			case <-time.After(wait):
				t.Fatal("writer did not enter the supported frame kinds send")
			}
			if _, err := open(tc.retryGrant); err != nil {
				t.Fatalf("Open after the close send completed: %v", err)
			}
		})
	}
}

// TestCloseWithoutFrameReleasesAtOnce covers the agent terminal frame: the
// RPC closes the stream without a close frame, so nothing is queued and the
// slots return immediately, even with a full queue.
func TestCloseWithoutFrameReleasesAtOnce(t *testing.T) {
	c := NewConn(context.Background(), "device", 1, closeTestLimits)
	s, err := c.Open(context.Background(), "grant-a", "sandbox")
	if err != nil {
		t.Fatal(err)
	}
	fillOutbound(c)
	before := len(c.out)
	closeReturnsPromptly(t, s, false)
	if got := len(c.out); got != before {
		t.Fatalf("queued frames = %d, want %d: Close(false) queues nothing", got, before)
	}
	if got := pendingFinals(c); got != 0 {
		t.Fatalf("pending finals = %d, want 0", got)
	}
	if got := perGrantCount(c, "grant-a"); got != 0 {
		t.Fatalf("grant-a slots = %d, want 0", got)
	}
	// The slot is free again: the queue is full, so Open is admitted and then
	// waits for room (it would fail with ErrTooManyStreams otherwise).
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := c.Open(ctx, "grant-a", "sandbox"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Open after Close(false) = %v, want to wait for room", err)
	}
}

// TestCloseWithWritableQueueSendsOneClose is the normal cancellation path.
func TestCloseWithWritableQueueSendsOneClose(t *testing.T) {
	c := NewConn(context.Background(), "device", 1, closeTestLimits)
	s, err := c.Open(context.Background(), "grant-a", "sandbox")
	if err != nil {
		t.Fatal(err)
	}
	s.Close(true)
	s.Close(true)
	frames := drainOut(c)
	if len(frames) != 2 || frames[0].GetRelayStreamOpen() == nil || frames[1].GetRelayStreamClose() == nil || frames[1].GetStreamId() != s.ID {
		t.Fatalf("queued frames = %d, want the stream open frame and one stream close frame", len(frames))
	}
	if got := pendingFinals(c); got != 0 {
		t.Fatalf("pending finals = %d, want 0", got)
	}
}

// TestCloseAfterConnectionClosedQueuesNothing checks that closing a stream of
// a closed connection leaks no reservation.
func TestCloseAfterConnectionClosedQueuesNothing(t *testing.T) {
	c := NewConn(context.Background(), "device", 1, closeTestLimits)
	s, err := c.Open(context.Background(), "grant-a", "sandbox")
	if err != nil {
		t.Fatal(err)
	}
	c.Close(ErrConnClosed)
	s.Close(true)
	if got := pendingFinals(c); got != 0 {
		t.Fatalf("pending finals = %d, want 0", got)
	}
}
