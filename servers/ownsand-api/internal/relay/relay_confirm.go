// SPDX-License-Identifier: FSL-1.1-ALv2

package relay

import (
	"context"
	"time"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// SendConfirmed is SendMetered that returns only after the connection writer
// finished the send of f: the frame is on the wire (or in the transport's
// send path), not only in the outbound queue. A caller uses it when a later
// deadline must start at the moment the peer can first see the frame, for
// example the acceptance timeout of a relay request, because a frame that
// waits in the queue behind a blocked writer is not yet visible to the agent.
//
// If acceptance is positive, the writer also starts the stream's acceptance
// window when the send succeeds.
//
// The wait for credit, for a place in the queue and for the writer all end
// when ctx ends (it returns the context error) or the connection closes (it
// returns ErrConnClosed, also when the writer failed). A frame that is queued
// when ctx ends may still be written later; the caller then closes the stream.
func (s *Stream) SendConfirmed(ctx context.Context, f *ownsandv1.DataFrame, onWritten func(n int64), acceptance time.Duration) error {
	w := &confirmedWrite{stream: s, acceptance: acceptance, done: make(chan error, 1)}
	// Register before the frame can reach the writer. The pointer identifies
	// the frame, and a caller never queues the same frame twice.
	s.conn.writeWaiters.Store(f, w)
	defer s.conn.writeWaiters.Delete(f)
	if err := s.SendMetered(ctx, f, onWritten); err != nil {
		return err
	}
	select {
	case err := <-w.done:
		return s.confirmResult(err)
	case <-ctx.Done():
	case <-s.conn.Done():
	}
	// A result that arrived together with the end of ctx or of the
	// connection wins, so a completed send is never reported as a failure.
	select {
	case err := <-w.done:
		return s.confirmResult(err)
	default:
	}
	if s.conn.ctx.Err() != nil {
		return ErrConnClosed
	}
	return ctx.Err()
}

// confirmResult maps the result of the writer's send to the error of
// SendConfirmed. A failed send closes the connection, so it reports the same
// error as every other send on a closed connection.
func (s *Stream) confirmResult(err error) error {
	if err != nil {
		return ErrConnClosed
	}
	return nil
}

type confirmedWrite struct {
	stream     *Stream
	acceptance time.Duration
	done       chan error
}

// sendMeasured sends the frame and reports metering and confirmed sends. For
// a confirmed send, it starts the acceptance window as soon as the transport
// send returns, before reporting the result to the waiting goroutine.
func (c *Conn) sendMeasured(of outFrame, send func(*ownsandv1.DataFrame) error) error {
	v, ok := c.writeWaiters.Load(of.frame)
	if !ok {
		return c.sendCounted(of, send)
	}
	w := v.(*confirmedWrite)
	err := c.sendCounted(of, func(f *ownsandv1.DataFrame) error {
		err := send(f)
		if err == nil && w.acceptance > 0 {
			c.mu.Lock()
			completedAt := c.clk.Elapsed()
			w.stream.acceptance.window = w.acceptance
			w.stream.acceptance.deadline = completedAt + w.acceptance
			c.mu.Unlock()
		}
		return err
	})
	w.done <- err
	return err
}
