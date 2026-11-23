// SPDX-License-Identifier: Apache-2.0

package datapath

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// flowFixtureFrames is the number of frames that the flow-control fixture
// sends. Frames alternate between a small stdout frame and a maximum-size
// file chunk, so the credit accounting sees both extremes.
const flowFixtureFrames = 200

// flowFixtureDeadline bounds a run that must succeed. It only guards against
// a hang under heavy CI load: a healthy run takes milliseconds.
const flowFixtureDeadline = 30 * time.Second

// flowFixtureStallDeadline bounds a run that must stall. The run ends at this
// deadline by design; the tests assert the diagnostic, never how long it took.
const flowFixtureStallDeadline = 200 * time.Millisecond

var (
	// errFlowFixtureStalled marks a run that hit its deadline before the
	// receiver got every frame.
	errFlowFixtureStalled = errors.New("flow-control fixture stalled")
	// errFlowFixtureReceiverStopped marks a run in which the receiver
	// deliberately left before the sender finished.
	errFlowFixtureReceiverStopped = errors.New("flow-control fixture receiver stopped early")
)

// flowFixtureOptions selects a fault for the fixture. The zero value is a
// healthy run.
type flowFixtureOptions struct {
	// withholdCredit makes the credit worker drop every window update, so
	// the sender runs out of credit.
	withholdCredit bool
	// stopAfter makes the receiver return after that many frames; zero
	// means it receives all of them.
	stopAfter int
}

// flowFixtureResult reports one fixture run.
type flowFixtureResult struct {
	received int
	// running counts the workers that have not exited yet. The fixture joins
	// both workers before it returns, so the count is zero by then.
	running int32
}

// runFlowFixture runs a sender, a credit worker and a slow receiver through a
// pair of windows. The receiver runs on the calling goroutine. Both workers
// always exit and are joined before it returns, whichever way the run ends.
// Worker failures are returned as errors, never raised as panics on the
// worker goroutines. A frame or credit stall ends at the deadline of ctx with
// an error that wraps errFlowFixtureStalled and names the window state.
func runFlowFixture(ctx context.Context, opts flowFixtureOptions) (result flowFixtureResult, _ error) {
	recv, err := NewReceiveWindow(MinWindowSize)
	if err != nil {
		return result, err
	}
	send, err := NewSendWindow(recv.InitialWindow())
	if err != nil {
		return result, err
	}

	ctx, cancel := context.WithCancel(ctx)
	frames := make(chan int64, flowFixtureFrames)
	updates := make(chan int64, flowFixtureFrames)
	workerErrs := make(chan error, 2)
	var running atomic.Int32
	var workers sync.WaitGroup
	start := func(name string, run func() error) {
		running.Add(1)
		workers.Add(1)
		go func() {
			defer workers.Done()
			defer running.Add(-1)
			// A worker that ends because the fixture cancelled it is not a
			// failure; the run reports its own outcome.
			if err := run(); err != nil && ctx.Err() == nil {
				workerErrs <- fmt.Errorf("%s: %w", name, err)
			}
		}()
	}
	// Cancel first and join afterwards, so that a worker blocked on credit or
	// on a channel send always wakes up. The results are named so that this
	// deferred function can set running after the return statement has copied
	// the other fields: with an unnamed result the caller would never see it.
	defer func() {
		cancel()
		workers.Wait()
		result.running = running.Load()
	}()

	start("credit worker", func() error {
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case inc, ok := <-updates:
				if !ok {
					return nil
				}
				if opts.withholdCredit {
					continue
				}
				if err := send.Add(inc); err != nil {
					return err
				}
			}
		}
	})
	start("sender", func() error {
		defer close(frames)
		for i := 0; i < flowFixtureFrames; i++ {
			f := maxChunkFrame()
			if i%2 == 0 {
				f = MustNewFrame(7, sandboxID, &ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Stdout{Stdout: &ownsandv1.ExecOutput{Data: make([]byte, 1000)}}})
			}
			cost := FrameCost(f)
			if err := send.Consume(ctx, cost); err != nil {
				return err
			}
			select {
			case frames <- cost:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	})

	stalled := func() error {
		return fmt.Errorf("%w: received %d of %d frames; sender credit available %d, receiver outstanding %d, buffered %d: %w",
			errFlowFixtureStalled, result.received, flowFixtureFrames, send.Available(), recv.Outstanding(), recv.Buffered(), ctx.Err())
	}
	for {
		select {
		case err := <-workerErrs:
			return result, err
		case <-ctx.Done():
			return result, stalled()
		case cost, ok := <-frames:
			if !ok {
				// The sender finished. Report its failure, if any, before
				// the frame count.
				select {
				case err := <-workerErrs:
					return result, err
				default:
				}
				// A sender that ended because the run ended did not fail.
				if ctx.Err() != nil {
					return result, stalled()
				}
				if result.received != flowFixtureFrames {
					return result, fmt.Errorf("received %d of %d frames", result.received, flowFixtureFrames)
				}
				return result, nil
			}
			if err := recv.Receive(cost); err != nil {
				return result, err
			}
			if recv.Buffered() > recv.InitialWindow() {
				return result, fmt.Errorf("buffer exceeded: buffered %d, capacity %d", recv.Buffered(), recv.InitialWindow())
			}
			inc, err := recv.Release(cost)
			if err != nil {
				return result, err
			}
			result.received++
			if opts.stopAfter > 0 && result.received >= opts.stopAfter {
				return result, errFlowFixtureReceiverStopped
			}
			select {
			case updates <- inc:
			case <-ctx.Done():
				return result, stalled()
			}
		}
	}
}

// TestFlowControlEndToEnd runs a sender and a slow receiver through a pair
// of windows and checks that the receiver never buffers more than its
// capacity.
func TestFlowControlEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), flowFixtureDeadline)
	defer cancel()
	result, err := runFlowFixture(ctx, flowFixtureOptions{})
	if err != nil {
		t.Fatalf("flow-control fixture: %v", err)
	}
	if result.received != flowFixtureFrames {
		t.Fatalf("received %d frames, want %d", result.received, flowFixtureFrames)
	}
	if result.running != 0 {
		t.Fatalf("%d workers still running after the fixture returned", result.running)
	}
}

// TestFlowControlFixtureFaults checks that the fixture reports a stall or an
// early receiver exit instead of hanging, and that it joins its workers.
func TestFlowControlFixtureFaults(t *testing.T) {
	t.Run("withheld credit stalls the sender", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), flowFixtureStallDeadline)
		defer cancel()
		result, err := runFlowFixture(ctx, flowFixtureOptions{withholdCredit: true})
		if !errors.Is(err, errFlowFixtureStalled) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("withheld credit: want a stall at the deadline, got %v", err)
		}
		if result.received >= flowFixtureFrames {
			t.Fatalf("received %d frames, want fewer than %d", result.received, flowFixtureFrames)
		}
		if result.running != 0 {
			t.Fatalf("%d workers still running after the fixture returned", result.running)
		}
	})
	t.Run("early receiver exit joins the workers", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), flowFixtureDeadline)
		defer cancel()
		result, err := runFlowFixture(ctx, flowFixtureOptions{stopAfter: 3})
		if !errors.Is(err, errFlowFixtureReceiverStopped) {
			t.Fatalf("early exit: want the receiver-stopped error, got %v", err)
		}
		if result.received != 3 {
			t.Fatalf("received %d frames, want 3", result.received)
		}
		if result.running != 0 {
			t.Fatalf("%d workers still running after the fixture returned", result.running)
		}
	})
	t.Run("an ended context reports a stall", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		result, err := runFlowFixture(ctx, flowFixtureOptions{})
		if !errors.Is(err, errFlowFixtureStalled) || !errors.Is(err, context.Canceled) {
			t.Fatalf("ended context: want a stall, got %v", err)
		}
		if result.running != 0 {
			t.Fatalf("%d workers still running after the fixture returned", result.running)
		}
	})
}
