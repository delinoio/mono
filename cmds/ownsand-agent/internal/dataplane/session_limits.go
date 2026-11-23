// SPDX-License-Identifier: FSL-1.1-ALv2

package dataplane

import (
	"log/slog"
	"sync"
	"time"
)

// SessionLimits bound what the peer-to-peer path of one agent can hold, so a
// caller (or a compromised control plane) cannot exhaust device memory and
// goroutines with many sessions or many open streams in one session. The
// relay path has the equivalent RelayLimits. A zero field selects its default.
type SessionLimits struct {
	// MaxStreamsPerSession caps the open request streams of one session. The
	// relay allows 8 streams per grant and 64 per connection; a session
	// serves one caller, so it gets the connection value.
	MaxStreamsPerSession int
	// MaxSessions caps the sessions of the data plane, answered or in setup.
	// It equals the relay's per-device grant limit.
	MaxSessions int
}

// DefaultSessionLimits are the recorded limits. They are conservative on the
// permissive side: a caller that runs the documented relay maximum of 64
// concurrent requests, and a device that serves the 256 grants the relay
// admits, stay below them.
func DefaultSessionLimits() SessionLimits {
	return SessionLimits{MaxStreamsPerSession: 64, MaxSessions: 256}
}

func (l SessionLimits) withDefaults() SessionLimits {
	d := DefaultSessionLimits()
	if l.MaxStreamsPerSession <= 0 {
		l.MaxStreamsPerSession = d.MaxStreamsPerSession
	}
	if l.MaxSessions <= 0 {
		l.MaxSessions = d.MaxSessions
	}
	return l
}

// Messages of the capacity rejections. They are part of the contract.
const (
	sessionCapacityMessage = "agent peer-to-peer session capacity reached"
	streamCapacityMessage  = "agent peer-to-peer stream capacity reached"
)

// limitRejectLogInterval is the minimum time between two rejection log
// records of one kind.
const limitRejectLogInterval = 5 * time.Second

// limitRejectLog logs the rejections of one kind at most once per interval,
// with the number of rejections since the previous record. It never logs
// frame content.
type limitRejectLog struct {
	mu       sync.Mutex
	last     time.Time
	rejected int
}

// note counts one rejection and logs when the interval has passed. It
// reports whether it logged.
func (r *limitRejectLog) note(logger *slog.Logger, now time.Time, kind, sessionID, sandboxID string, limit int) bool {
	r.mu.Lock()
	r.rejected++
	if !r.last.IsZero() && now.Sub(r.last) < limitRejectLogInterval {
		r.mu.Unlock()
		return false
	}
	count := r.rejected
	r.rejected = 0
	r.last = now
	r.mu.Unlock()
	logger.Warn("peer-to-peer path at capacity; rejected work",
		slog.String("kind", kind),
		slog.String("session_id", sessionID),
		slog.String("sandbox_id", sandboxID),
		slog.Int("limit", limit),
		slog.Int("rejected", count))
	return true
}
