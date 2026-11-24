// SPDX-License-Identifier: FSL-1.1-ALv2

package relay

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/delinoio/mono/go/ownsand/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// stepClock is a Clock that a test moves by hand: the wall reading and the
// monotonic reading change only when the test changes them, independently of
// each other, so a test can step the wall clock without monotonic time
// passing and the other way round.
type stepClock struct {
	wallNanos atomic.Int64
	monoNanos atomic.Int64
}

func newStepClock() *stepClock {
	k := &stepClock{}
	k.wallNanos.Store(time.Date(2026, time.November, 23, 12, 0, 0, 0, time.UTC).UnixNano())
	return k
}

func (k *stepClock) Now() time.Time         { return time.Unix(0, k.wallNanos.Load()) }
func (k *stepClock) Elapsed() time.Duration { return time.Duration(k.monoNanos.Load()) }

// stepWall moves the wall clock by d (negative d moves it back); monotonic
// time does not move.
func (k *stepClock) stepWall(d time.Duration) { k.wallNanos.Add(int64(d)) }

// advance moves monotonic time by d and the wall clock with it, like time
// that passes normally.
func (k *stepClock) advance(d time.Duration) {
	k.monoNanos.Add(int64(d))
	k.wallNanos.Add(int64(d))
}

// stepLimits are limits with real tickers that fire often (Keepalive/3) and
// thresholds that only the stepClock reaches.
var stepLimits = Limits{
	StreamBuffer:       datapath.MinWindowSize,
	MaxStreamsPerConn:  4,
	MaxStreamsPerGrant: 4,
	Keepalive:          30 * time.Millisecond,
	IdleTimeout:        500 * time.Millisecond,
}

// stepWallDeltas are a forward and a backward wall-clock step, each far
// larger than the thresholds.
var stepWallDeltas = map[string]time.Duration{
	"forward":  time.Hour,
	"backward": -time.Hour,
}

// stepSettle is how long a test waits to show that a timer did not fire. The
// tickers fire every 10 ms, so a timer that the clock reading allowed would
// have fired many times by then.
const stepSettle = 150 * time.Millisecond

// stepConn is a running connection on a stepClock. recvCh feeds its recv and
// sent receives the frames that its send wrote.
type stepConn struct {
	c   *Conn
	clk *stepClock
	// done receives the result of Run once; finished closes when Run returned.
	done     <-chan error
	finished <-chan struct{}
	recvCh   chan *ownsandv1.DataFrame
	sent     chan *ownsandv1.DataFrame
}

func runStepConn(t *testing.T, limits Limits) *stepConn {
	t.Helper()
	clk := newStepClock()
	c := NewConnWithClock(context.Background(), "device", 1, limits, clk)
	sc := &stepConn{c: c, clk: clk, recvCh: make(chan *ownsandv1.DataFrame), sent: make(chan *ownsandv1.DataFrame, 64)}
	recv := func() (*ownsandv1.DataFrame, error) {
		select {
		case f := <-sc.recvCh:
			return f, nil
		case <-c.Done():
			return nil, c.Err()
		}
	}
	send := func(f *ownsandv1.DataFrame) error {
		sc.sent <- f
		return nil
	}
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		done <- c.Run(recv, send)
		close(finished)
	}()
	sc.done, sc.finished = done, finished
	t.Cleanup(func() {
		c.Close(errors.New("test done"))
		<-finished
	})
	return sc
}

// awaitFrame returns the next frame that the connection sent, or fails.
func (sc *stepConn) awaitFrame(t *testing.T) *ownsandv1.DataFrame {
	t.Helper()
	select {
	case f := <-sc.sent:
		return f
	case err := <-sc.done:
		t.Fatalf("Run returned %v before it sent a frame", err)
	case <-time.After(2 * time.Second):
		t.Fatal("no frame sent within 2s")
	}
	return nil
}

func isStepKeepalive(f *ownsandv1.DataFrame) bool {
	_, ok := f.GetPayload().(*ownsandv1.DataFrame_RelayKeepalive)
	return ok
}

// expectNoFrame fails when the connection sends a frame within stepSettle.
func (sc *stepConn) expectNoFrame(t *testing.T, why string) {
	t.Helper()
	select {
	case f := <-sc.sent:
		t.Fatalf("%s: connection sent %T", why, f.GetPayload())
	case <-time.After(stepSettle):
	}
}

// expectRunning fails when Run returns within stepSettle.
func (sc *stepConn) expectRunning(t *testing.T, why string) {
	t.Helper()
	select {
	case err := <-sc.done:
		t.Fatalf("%s: Run returned %v", why, err)
	case <-time.After(stepSettle):
	}
}

// awaitStoredSend sends f through the writer and waits until the writer has
// recorded the send time for the current monotonic reading, so the test then
// knows which reading the keepalive timer counts from.
func (sc *stepConn) awaitStoredSend(t *testing.T, f *ownsandv1.DataFrame) {
	t.Helper()
	if err := sc.c.enqueue(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	sc.awaitFrame(t)
	awaitStepValue(t, "lastSend", &sc.c.lastSend, int64(sc.clk.Elapsed()))
}

// deliver hands a frame to the connection and waits until the receive time
// for the current monotonic reading is recorded.
func (sc *stepConn) deliver(t *testing.T, f *ownsandv1.DataFrame) {
	t.Helper()
	select {
	case sc.recvCh <- f:
	case <-time.After(2 * time.Second):
		t.Fatal("the connection did not take a received frame within 2s")
	}
	awaitStepValue(t, "lastRecv", &sc.c.lastRecv, int64(sc.clk.Elapsed()))
}

func awaitStepValue(t *testing.T, name string, v *atomic.Int64, want int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for v.Load() != want {
		if time.Now().After(deadline) {
			t.Fatalf("%s = %d, want %d within 2s", name, v.Load(), want)
		}
		time.Sleep(time.Millisecond)
	}
}

func awaitStepIdle(t *testing.T, sc *stepConn) {
	t.Helper()
	select {
	case err := <-sc.done:
		if !errors.Is(err, ErrIdle) {
			t.Fatalf("Run = %v, want ErrIdle", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the connection did not close as idle within 2s of the monotonic idle timeout")
	}
}

func stepKeepaliveFrame() *ownsandv1.DataFrame {
	return datapath.MustNewFrame(0, "", &ownsandv1.RelayKeepalive{})
}

// TestIdleCloseIgnoresWallClockSteps checks that a wall-clock step moves the
// idle close neither earlier nor later: only monotonic time counts.
func TestIdleCloseIgnoresWallClockSteps(t *testing.T) {
	for name, delta := range stepWallDeltas {
		t.Run(name, func(t *testing.T) {
			sc := runStepConn(t, stepLimits)
			sc.awaitFrame(t) // supported frame kinds
			sc.clk.advance(stepLimits.IdleTimeout / 2)
			sc.clk.stepWall(delta)
			sc.expectRunning(t, "monotonic time is below the idle timeout")
			sc.clk.advance(stepLimits.IdleTimeout)
			awaitStepIdle(t, sc)
		})
	}
}

// TestIdleCloseWithoutMonotonicProgressIgnoresWallClockSteps checks that a
// wall step alone, however large, never closes the connection.
func TestIdleCloseWithoutMonotonicProgressIgnoresWallClockSteps(t *testing.T) {
	for name, delta := range stepWallDeltas {
		t.Run(name, func(t *testing.T) {
			sc := runStepConn(t, stepLimits)
			sc.awaitFrame(t)
			sc.clk.stepWall(delta)
			sc.expectRunning(t, "only the wall clock moved")
		})
	}
}

// TestReceiveResetsIdleTimerOnMonotonicTime checks that a received frame
// restarts the idle count, also across a wall-clock step.
func TestReceiveResetsIdleTimerOnMonotonicTime(t *testing.T) {
	for name, delta := range stepWallDeltas {
		t.Run(name, func(t *testing.T) {
			sc := runStepConn(t, stepLimits)
			sc.awaitFrame(t)
			sc.clk.advance(stepLimits.IdleTimeout * 3 / 5)
			sc.deliver(t, stepKeepaliveFrame())
			sc.clk.stepWall(delta)
			sc.clk.advance(stepLimits.IdleTimeout * 3 / 5)
			// 120% of the idle timeout since the connection opened, 60% since
			// the last received frame.
			sc.expectRunning(t, "a frame arrived 60% of the idle timeout ago")
			sc.clk.advance(stepLimits.IdleTimeout * 3 / 5)
			awaitStepIdle(t, sc)
		})
	}
}

// TestKeepaliveIgnoresWallClockSteps checks that a wall-clock step neither
// sends a keepalive early nor delays it.
func TestKeepaliveIgnoresWallClockSteps(t *testing.T) {
	limits := stepLimits
	limits.IdleTimeout = time.Hour
	for name, delta := range stepWallDeltas {
		t.Run(name, func(t *testing.T) {
			sc := runStepConn(t, limits)
			sc.awaitFrame(t)
			sc.clk.advance(time.Second)
			sc.awaitStoredSend(t, stepKeepaliveFrame())
			sc.clk.stepWall(delta)
			sc.expectNoFrame(t, "no monotonic time passed since the last send")
			sc.clk.advance(limits.Keepalive)
			if f := sc.awaitFrame(t); !isStepKeepalive(f) {
				t.Fatalf("frame = %T, want a keepalive", f.GetPayload())
			}
		})
	}
}

// TestSendResetsKeepaliveTimerOnMonotonicTime checks that a send restarts the
// keepalive count.
func TestSendResetsKeepaliveTimerOnMonotonicTime(t *testing.T) {
	limits := stepLimits
	limits.IdleTimeout = time.Hour
	sc := runStepConn(t, limits)
	sc.awaitFrame(t)
	sc.clk.advance(time.Second)
	sc.awaitStoredSend(t, stepKeepaliveFrame())
	sc.clk.advance(limits.Keepalive * 4 / 5)
	sc.expectNoFrame(t, "80% of the keepalive interval since the last send")
	sc.awaitStoredSend(t, stepKeepaliveFrame())
	sc.clk.advance(limits.Keepalive * 4 / 5)
	// 160% of the keepalive interval since the first send, 80% since the
	// second.
	sc.expectNoFrame(t, "the second send restarted the keepalive count")
	sc.clk.advance(limits.Keepalive * 4 / 5)
	if f := sc.awaitFrame(t); !isStepKeepalive(f) {
		t.Fatalf("frame = %T, want a keepalive", f.GetPayload())
	}
}

// TestObserversReturnWallTimestamps checks that LastReceive and
// LastLargeReceive keep reporting wall time, including a wall-clock step.
func TestObserversReturnWallTimestamps(t *testing.T) {
	limits := stepLimits
	limits.IdleTimeout = time.Hour
	sc := runStepConn(t, limits)
	sc.awaitFrame(t)
	opened := sc.clk.Now()
	if got := sc.c.LastReceive(); !got.Equal(opened) {
		t.Fatalf("LastReceive of a new connection = %v, want its wall start %v", got, opened)
	}
	if !sc.c.LastLargeReceive().IsZero() {
		t.Fatal("a new connection has a large receive time")
	}
	s := openStream(t, sc.c)

	sc.clk.advance(time.Second)
	sc.clk.stepWall(-time.Hour)
	want := sc.clk.Now()
	sc.deliver(t, stepKeepaliveFrame())
	if got := sc.c.LastReceive(); !got.Equal(want) {
		t.Fatalf("LastReceive = %v, want the wall time %v of the received frame", got, want)
	}

	large := datapath.MustNewFrame(s.ID, "sandbox", &ownsandv1.ExecEvent{
		Event: &ownsandv1.ExecEvent_Stdout{Stdout: &ownsandv1.ExecOutput{Data: make([]byte, 2*LargeFrameBytes)}}})
	sc.clk.stepWall(2 * time.Hour)
	wantLarge := sc.clk.Now()
	if err := sc.c.dispatch(large); err != nil {
		t.Fatal(err)
	}
	if got := sc.c.LastLargeReceive(); !got.Equal(wantLarge) {
		t.Fatalf("LastLargeReceive = %v, want the wall time %v of the large frame", got, wantLarge)
	}
}

// TestSystemClockMeasuresElapsedMonotonicTime checks the production clock.
func TestSystemClockMeasuresElapsedMonotonicTime(t *testing.T) {
	clk := newSystemClock()
	first := clk.Elapsed()
	time.Sleep(5 * time.Millisecond)
	second := clk.Elapsed()
	if first < 0 || second < first+5*time.Millisecond {
		t.Fatalf("mono = %v then %v, want elapsed time since creation to grow by at least 5ms", first, second)
	}
	if d := time.Since(clk.Now()); d < 0 || d > time.Second {
		t.Fatalf("wall is %v behind the current time", d)
	}
}

// TestAcceptanceExtensionsAreMonotonic checks that every in-window receipt
// extends the deadline, even when the handler has not checked it yet.
func TestAcceptanceExtensionsAreMonotonic(t *testing.T) {
	clock := newStepClock()
	c := NewConnWithClock(t.Context(), "device", 1, testLimits, clock)
	t.Cleanup(func() { c.Close(errors.New("test finished")) })
	waiting := openStream(t, c)
	other, err := c.Open(t.Context(), "other-grant", "sandbox")
	if err != nil {
		t.Fatal(err)
	}
	large := datapath.MustNewFrame(other.ID, other.SandboxID, &ownsandv1.ExecEvent{
		Event: &ownsandv1.ExecEvent_Stdout{Stdout: &ownsandv1.ExecOutput{Data: make([]byte, LargeFrameBytes)}},
	})
	receiveLarge := func() {
		t.Helper()
		if err := c.dispatch(large); err != nil {
			t.Fatal(err)
		}
	}

	waiting.startAcceptance(30 * time.Second)
	clock.advance(10 * time.Second)
	clock.stepWall(time.Hour)
	receiveLarge() // Deadline moves from elapsed second 30 to 40.
	clock.advance(25 * time.Second)
	clock.stepWall(-2 * time.Hour)
	receiveLarge() // Deadline moves from elapsed second 40 to 65.
	clock.advance(30*time.Second - time.Nanosecond)
	if waiting.AcceptanceExpired() {
		t.Fatal("acceptance expired before the extended deadline")
	}
	clock.advance(time.Nanosecond)
	receiveLarge() // A receipt at the deadline cannot restart the wait.
	if !waiting.AcceptanceExpired() {
		t.Fatal("a receipt at the deadline revived the acceptance wait")
	}
}
