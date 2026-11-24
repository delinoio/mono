// SPDX-License-Identifier: FSL-1.1-ALv2

package relay

import (
	"time"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// streamAcceptance tracks admission on the connection's monotonic clock.
// The writer starts the window, and the reader updates it in receive order
// so a later large frame cannot change the decision for an earlier notice.
type streamAcceptance struct {
	window   time.Duration
	deadline time.Duration
	resolved bool
}

func (a streamAcceptance) expired(now time.Duration) bool {
	return a.window > 0 && !a.resolved && now >= a.deadline
}

func (a *streamAcceptance) extend(receivedAt time.Duration) {
	if a.window > 0 && !a.resolved && receivedAt < a.deadline {
		a.deadline = receivedAt + a.window
	}
}

func (c *Conn) recordStreamReceipt(f *ownsandv1.DataFrame, size int) *Stream {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.streams[f.GetStreamId()]
	if s == nil || s.isClosed() {
		return nil
	}
	receivedAt := c.clk.Elapsed()
	if size >= LargeFrameBytes {
		c.lastLarge.Store(c.clk.Now().UnixNano())
		for _, stream := range c.streams {
			stream.acceptance.extend(receivedAt)
		}
	}
	if !s.acceptance.expired(receivedAt) {
		switch f.GetKind() {
		case ownsandv1.DataFrameKind_DATA_FRAME_KIND_REQUEST_ACCEPTED,
			ownsandv1.DataFrameKind_DATA_FRAME_KIND_SESSION_END,
			ownsandv1.DataFrameKind_DATA_FRAME_KIND_ERROR:
			// An agent can end the stream as soon as it sees the open
			// frame, or reply before the writer returns from the send.
			s.acceptance.resolved = true
		}
	}
	return s
}

// AcceptanceExpired reports whether a forwarded request ran out of time
// before the connection received an acceptance or terminal frame. Once a
// timely reply arrives, caller response backpressure cannot change the result.
func (s *Stream) AcceptanceExpired() bool {
	c := s.conn
	c.mu.Lock()
	defer c.mu.Unlock()
	return s.acceptance.expired(c.clk.Elapsed())
}
