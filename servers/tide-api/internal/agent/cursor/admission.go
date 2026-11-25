// SPDX-License-Identifier: FSL-1.1-ALv2

package cursor

import (
	"context"
	"sync"

	"github.com/delinoio/mono/servers/tide-api/internal/agent/vendorhttp"
)

type pendingAdmission struct {
	client  *vendorhttp.Client
	request PendingRequest
}

type demandKey struct {
	id     string
	marker int64
}

func (a pendingAdmission) key() demandKey {
	return demandKey{id: a.request.ID, marker: a.request.CreatedAtMs}
}

// admissionQueue bounds the work of one watcher and coalesces copies of
// each pending entry until admission finishes.
type admissionQueue struct {
	pending chan pendingAdmission
	mu      sync.Mutex
	active  map[demandKey]struct{}
}

func newAdmissionQueue(size int) *admissionQueue {
	return &admissionQueue{
		pending: make(chan pendingAdmission, size),
		active:  make(map[demandKey]struct{}),
	}
}

// offer returns false when the queue is full. A later list can offer the
// demand again if it is still pending at Cursor.
func (q *admissionQueue) offer(client *vendorhttp.Client, request PendingRequest) bool {
	a := pendingAdmission{client: client, request: request}
	key := a.key()
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, exists := q.active[key]; exists {
		return true
	}
	select {
	case q.pending <- a:
		q.active[key] = struct{}{}
		return true
	default:
		return false
	}
}

func (q *admissionQueue) run(ctx context.Context, admit func(pendingAdmission)) {
	for {
		select {
		case <-ctx.Done():
			return
		case a := <-q.pending:
			if ctx.Err() != nil {
				return
			}
			admit(a)
			q.mu.Lock()
			delete(q.active, a.key())
			q.mu.Unlock()
		}
	}
}
