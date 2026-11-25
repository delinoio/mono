// SPDX-License-Identifier: FSL-1.1-ALv2

package dataplane

import (
	"container/list"
	"time"
)

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
}

// tombstones is the bounded set of session IDs that were revoked before the
// agent registered them. Its methods need the caller to hold DataPlane.mu.
// Every tombstone has the same lifetime, so the insertion order is also the
// expiry order and an expiry sweep or a size trim pops the head of the queue.
type tombstones struct {
	// Map each session ID to its queue entry so consuming a tombstone
	// removes it from both collections immediately.
	live  map[string]*list.Element
	queue list.List
}

// add records a tombstone for id. A repeated revocation keeps the first one.
func (t *tombstones) add(id string, now time.Time) {
	t.sweep(now)
	if _, ok := t.live[id]; ok {
		return
	}
	if t.live == nil {
		t.live = make(map[string]*list.Element)
	}
	if t.queue.Len() == maxTombstones {
		t.remove(t.queue.Front())
	}
	t.live[id] = t.queue.PushBack(tombstone{id: id, expireAt: now.Add(tombstoneTTL)})
}

// has reports whether id has a live tombstone.
func (t *tombstones) has(id string, now time.Time) bool {
	t.sweep(now)
	_, ok := t.live[id]
	return ok
}

// take reports whether id has a live tombstone and consumes it.
func (t *tombstones) take(id string, now time.Time) bool {
	t.sweep(now)
	entry, ok := t.live[id]
	if !ok {
		return false
	}
	t.remove(entry)
	return true
}

// sweep drops expired tombstones from the head.
func (t *tombstones) sweep(now time.Time) {
	for entry := t.queue.Front(); entry != nil; entry = t.queue.Front() {
		if now.Before(entry.Value.(tombstone).expireAt) {
			return
		}
		t.remove(entry)
	}
}

func (t *tombstones) remove(entry *list.Element) {
	record := t.queue.Remove(entry).(tombstone)
	delete(t.live, record.id)
}

func (t *tombstones) len() int { return len(t.live) }
