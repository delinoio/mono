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
	// senderFrameLimit makes the sender finish after that many frames. Zero
	// leaves the usual frame count in place.
	senderFrameLimit int
	// senderErr is the error the sender returns after sending its frames.
	senderErr error
}

// flowFixtureResult reports one fixture run.
type flowFixtureResult struct {
	received int
	// running counts the workers that have not exited yet. The fixture joins
	// both workers before it returns, so the count is zero by then.
	running int32
}

type flowFixtureWorkers struct {
	errs    chan error
	running atomic.Int32
	wait    sync.WaitGroup
}

func (w *flowFixtureWorkers) start(ctx context.Context, name string, run func() error, complete func()) {
	w.running.Add(1)
	w.wait.Add(1)
	go func() {
		defer w.wait.Done()
		defer w.running.Add(-1)
		if complete != nil {
			// Publish the worker error before signaling completion.
			defer complete()
		}
		// The fixture reports its own outcome when it cancels a worker.
		if err := run(); err != nil && ctx.Err() == nil {
			w.errs <- fmt.Errorf("%s: %w", name, err)
		}
	}()
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
	workers := flowFixtureWorkers{errs: workerErrs}
	// Cancel first and join afterwards, so that a worker blocked on credit or
	// on a channel send always wakes up. The results are named so that this
	// deferred function can set running after the return statement has copied
	// the other fields: with an unnamed result the caller would never see it.
	defer func() {
		cancel()
		workers.wait.Wait()
		result.running = workers.running.Load()
	}()

	workers.start(ctx, "credit worker", func() error {
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
	}, nil)
	workers.start(ctx, "sender", func() error {
		frameCount := flowFixtureFrames
		if opts.senderFrameLimit > 0 {
			frameCount = opts.senderFrameLimit
		}
		for i := 0; i < frameCount; i++ {
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
		return opts.senderErr
	}, func() {
		close(frames)
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

func TestFlowControlFixtureSenderErrorBeforeClose(t *testing.T) {
	frames := make(chan int64)
	workerErrs := make(chan error, 2)
	workers := flowFixtureWorkers{errs: workerErrs}
	cause := errors.New("injected sender failure")
	var queuedErr error
	workers.start(t.Context(), "sender", func() error {
		return cause
	}, func() {
		select {
		case queuedErr = <-workerErrs:
		default:
		}
		close(frames)
	})
	workers.wait.Wait()

	select {
	case <-frames:
	default:
		t.Fatal("sender did not close frames")
	}
	if !errors.Is(queuedErr, cause) || queuedErr.Error() != "sender: "+cause.Error() {
		t.Fatalf("error queued before closing frames = %v, want wrapped sender error", queuedErr)
	}
	if running := workers.running.Load(); running != 0 {
		t.Fatalf("%d workers still running after the join", running)
	}
}

// TestFlowControlFixtureFaults checks the diagnostic and worker cleanup for
// each failed transfer.
func TestFlowControlFixtureFaults(t *testing.T) {
	t.Run("sender failure retains its cause", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), flowFixtureDeadline)
		defer cancel()
		cause := errors.New("injected sender failure")
		result, err := runFlowFixture(ctx, flowFixtureOptions{senderFrameLimit: 1, senderErr: cause})
		if !errors.Is(err, cause) || err.Error() != "sender: "+cause.Error() {
			t.Fatalf("sender failure: want wrapped sender error, got %v", err)
		}
		if result.running != 0 {
			t.Fatalf("%d workers still running after the fixture returned", result.running)
		}
	})
	t.Run("short transfer without a sender error reports the count", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), flowFixtureDeadline)
		defer cancel()
		result, err := runFlowFixture(ctx, flowFixtureOptions{senderFrameLimit: 1})
		if want := fmt.Sprintf("received 1 of %d frames", flowFixtureFrames); err == nil || err.Error() != want {
			t.Fatalf("short transfer: want %q, got %v", want, err)
		}
		if result.received != 1 || result.running != 0 {
			t.Fatalf("result = %+v, want one received frame and zero running workers", result)
		}
	})
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
