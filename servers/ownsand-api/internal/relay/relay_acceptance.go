// SPDX-License-Identifier: FSL-1.1-ALv2

package relay

import "time"

// acceptanceWait is protected by the connection mutex. A window starts when
// the writer confirms a request and can extend only while it is active.
type acceptanceWait struct {
	timeout  time.Duration
	deadline time.Duration
	accepted bool
}

// startAcceptance runs on the connection writer after a successful send.
func (s *Stream) startAcceptance(timeout time.Duration) {
	c := s.conn
	c.mu.Lock()
	defer c.mu.Unlock()
	if !s.acceptance.accepted {
		s.acceptance = acceptanceWait{timeout: timeout, deadline: c.clk.Elapsed() + timeout}
	}
}

// recordLargeReceive runs with the connection mutex held. The receive loop
// processes receipts in order, so each qualifying frame can extend an
// active window even when the RPC handler has not checked it yet.
func (c *Conn) recordLargeReceive() {
	now := c.clk.Elapsed()
	c.lastLarge.Store(c.clk.Now().UnixNano())
	for _, s := range c.streams {
		wait := &s.acceptance
		if wait.timeout > 0 && now < wait.deadline {
			wait.deadline = now + wait.timeout
		}
	}
}

// AcceptanceExpired reports whether the active acceptance window reached
// its cutoff on the connection's monotonic clock.
func (s *Stream) AcceptanceExpired() bool {
	c := s.conn
	c.mu.Lock()
	defer c.mu.Unlock()
	wait := s.acceptance
	return wait.timeout > 0 && c.clk.Elapsed() >= wait.deadline
}

// MarkAccepted stops the acceptance window.
func (s *Stream) MarkAccepted() {
	c := s.conn
	c.mu.Lock()
	defer c.mu.Unlock()
	s.acceptance = acceptanceWait{accepted: true}
}
