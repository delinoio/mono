// SPDX-License-Identifier: FSL-1.1-ALv2

package dataplane

import "time"

// Revocation tombstones. The control plane may revoke a session that the
// agent has not registered yet: the offer is answered on its own goroutine,
// so a revocation of a canceled or timed-out OpenSession can reach Revoke
// before Offer inserts the session, or while the offer still waits for its
// sandbox-state check. Revoke records the session ID of such a revocation as
// a tombstone, and Offer refuses an offer whose session ID has one.
const (
	// tombstoneTTL is how long a tombstone lives. It equals the absolute
	// session lifetime of one hour, after which the control plane no longer
	// signals an offer with that session ID as live (the revocation itself
	// stays pending on the control plane only until that expiry).
	tombstoneTTL = time.Hour
	// maxTombstones bounds the tombstone set. Past it, the oldest tombstone
	// is dropped first.
	maxTombstones = 1024
)

type tombstone struct {
	id       string
	expireAt time.Time
	seq      uint64
}

// tombstones is the bounded set of session IDs that were revoked before the
// agent registered them. Its methods need the caller to hold DataPlane.mu.
// Every tombstone has the same lifetime, so the insertion order is also the
// expiry order and an expiry sweep or a size trim pops the head of the queue.
type tombstones struct {
	// live maps a session ID to the sequence number of its queue entry; a
	// queue entry whose sequence number differs was consumed or replaced.
	live  map[string]uint64
	queue []tombstone
	seq   uint64
}

// add records a tombstone for id. A repeated revocation keeps the first one.
func (t *tombstones) add(id string, now time.Time) {
	t.sweep(now)
	if _, ok := t.live[id]; ok {
		return
	}
	if t.live == nil {
		t.live = map[string]uint64{}
	}
	t.seq++
	t.live[id] = t.seq
	t.queue = append(t.queue, tombstone{id: id, expireAt: now.Add(tombstoneTTL), seq: t.seq})
	for len(t.live) > maxTombstones {
		t.popHead()
	}
}

// has reports whether id has a live tombstone.
func (t *tombstones) has(id string, now time.Time) bool {
	t.sweep(now)
	_, ok := t.live[id]
	return ok
}

// take reports whether id has a live tombstone and consumes it.
func (t *tombstones) take(id string, now time.Time) bool {
	if !t.has(id, now) {
		return false
	}
	delete(t.live, id)
	return true
}

// sweep drops the tombstones that expired and the queue entries of consumed
// ones at the head.
func (t *tombstones) sweep(now time.Time) {
	for len(t.queue) > 0 {
		h := t.queue[0]
		if t.live[h.id] == h.seq && now.Before(h.expireAt) {
			return
		}
		t.popHead()
	}
}

func (t *tombstones) popHead() {
	h := t.queue[0]
	t.queue[0] = tombstone{}
	t.queue = t.queue[1:]
	if t.live[h.id] == h.seq {
		delete(t.live, h.id)
	}
}

func (t *tombstones) len() int { return len(t.live) }
