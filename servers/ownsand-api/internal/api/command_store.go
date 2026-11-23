// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"sync"
	"time"

	"github.com/delinoio/mono/servers/ownsand-api/internal/hub"
)

// DefaultCommandGrace is how long the server keeps the record of a command
// whose channel closed before it answered (2 minutes). A late result can
// arrive on a replacement channel of the same device, so the window covers
// the heartbeat timeout of the agent (45 seconds) plus the maximum agent
// reconnect backoff (30 seconds) with margin.
const DefaultCommandGrace = 2 * time.Minute

// pendingCommand is the record of a sent command that has no result yet.
type pendingCommand struct {
	info commandInfo
	// closedAt is when the channel that carried the command closed, or the
	// zero time while that channel is open. The record expires
	// DefaultCommandGrace (Timing.CommandGrace) after closedAt. Every time
	// that commandStore receives is a Service.commandRetentionNow reading
	// (elapsed time on a fixed origin), never a wall-clock time.
	closedAt time.Time
}

// commandStore holds the pending commands by command ID. Its mutex
// serializes add with markClosed, so a command sent on a channel that is
// closing is either seen as closed by add or marked by markClosed. The zero
// value is ready to use.
type commandStore struct {
	mu sync.Mutex
	m  map[string]pendingCommand
}

// add remembers the command sent on conn. When conn is already closed (its
// close precedes markClosed), the record starts expiring at once.
func (c *commandStore) add(id string, info commandInfo, conn *hub.Conn, now time.Time) {
	p := pendingCommand{info: info}
	c.mu.Lock()
	defer c.mu.Unlock()
	if conn.Context().Err() != nil {
		p.closedAt = now
	}
	if c.m == nil {
		c.m = map[string]pendingCommand{}
	}
	c.m[id] = p
}

// remove forgets a command at once (its enqueue failed).
func (c *commandStore) remove(id string) {
	c.mu.Lock()
	delete(c.m, id)
	c.mu.Unlock()
}

// takeOutcome is the result of commandStore.take.
type takeOutcome int

const (
	// takeUnknown means the command has no live record: it never existed,
	// was already consumed, or its grace period has passed.
	takeUnknown takeOutcome = iota
	// takeFound means the record belongs to the reporting device; take
	// consumed it.
	takeFound
	// takeWrongDevice means a live record exists for another device; take
	// left it in place.
	takeWrongDevice
)

// take consumes the record of a command for the device that reports its
// result. A record whose grace period has passed counts as absent, also
// before the sweep removed it, and is removed. A live record of another
// device stays in the store, so a result from the wrong device cannot
// discard the command that its expected device still has to complete.
func (c *commandStore) take(id, deviceID string, now time.Time, grace time.Duration) (commandInfo, takeOutcome) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.m[id]
	if !ok {
		return commandInfo{}, takeUnknown
	}
	if p.expired(now, grace) {
		delete(c.m, id)
		return commandInfo{}, takeUnknown
	}
	if p.info.deviceID != deviceID {
		return commandInfo{}, takeWrongDevice
	}
	delete(c.m, id)
	return p.info, takeFound
}

// markClosed starts the expiry of the commands that were sent on the
// channel of the device with the generation, and returns their number. The
// channel must already be closed. Records of other channels of the device
// stay untouched.
func (c *commandStore) markClosed(deviceID string, generation uint64, now time.Time) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for id, p := range c.m {
		if p.info.deviceID == deviceID && p.info.generation == generation && p.closedAt.IsZero() {
			p.closedAt = now
			c.m[id] = p
			n++
		}
	}
	return n
}

// expire removes the records whose grace period has passed and returns their
// number.
func (c *commandStore) expire(now time.Time, grace time.Duration) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for id, p := range c.m {
		if p.expired(now, grace) {
			delete(c.m, id)
			n++
		}
	}
	return n
}

// len returns the number of retained records.
func (c *commandStore) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m)
}

func (p pendingCommand) expired(now time.Time, grace time.Duration) bool {
	return !p.closedAt.IsZero() && now.Sub(p.closedAt) >= grace
}
