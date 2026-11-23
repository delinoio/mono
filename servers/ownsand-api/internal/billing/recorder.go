// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// counterKey identifies the counter of one organization, meter and window.
type counterKey struct {
	org    string
	meter  ownsandv1.BillingMeter
	window int64 // window start in Unix seconds
}

func (k counterKey) windowStart() time.Time { return time.Unix(k.window, 0).UTC() }

// Recorder adds usage to in-memory counters of this instance. The metering
// worker moves them to the shared counter rows in Postgres (see
// Metering.Flush) in batches that carry a receipt ID. It is safe for concurrent use.
type Recorder struct {
	logger *slog.Logger

	mu sync.Mutex
	// counts are the live counters that no batch holds yet.
	counts map[counterKey]int64
	// pending are the sealed batches that no flush has applied yet, oldest
	// first. A failed flush leaves its batch here for the retry.
	pending []*flushBatch
	// closed is set by the final sealBatch: the final snapshot of the process
	// was taken, so a later Add can no longer reach Postgres.
	closed bool
}

// NewRecorder returns an empty recorder.
func NewRecorder(logger *slog.Logger) *Recorder {
	return &Recorder{logger: logger, counts: map[counterKey]int64{}}
}

// Add adds quantity units of meter to the organization's counter of the
// window that contains at. A quantity of 0 or less, an unknown meter and an
// organization ID that is not a UUID v7 add nothing; the last two are bugs
// of the caller and are logged.
func (r *Recorder) Add(orgID string, meter ownsandv1.BillingMeter, at time.Time, quantity int64) {
	if quantity <= 0 {
		return
	}
	if _, ok := priceOf(meter); !ok {
		r.logger.Error("billing_usage_dropped", slog.String("organization_id", orgID), slog.Int("meter", int(meter)), slog.String("cause", "unknown meter"))
		return
	}
	id, err := uuidv7.Parse(orgID)
	if err != nil {
		r.logger.Error("billing_usage_dropped", slog.String("meter", MeterID(meter)), slog.String("cause", "invalid organization ID"))
		return
	}
	k := counterKey{org: id.String(), meter: meter, window: WindowStart(at).Unix()}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		// The final flush already took its snapshot, so this count cannot be
		// written. Report it instead of keeping it in memory unseen.
		r.logger.Error("billing_usage_dropped_after_final_flush", slog.String("organization_id", k.org), slog.String("meter", MeterID(meter)),
			slog.Time("window_start", k.windowStart()), slog.Int64("quantity", quantity))
		return
	}
	r.counts[k] = saturatingAdd(r.counts[k], quantity)
	r.mu.Unlock()
}

// flushBatch is a set of counters that one flush attempt writes to Postgres
// under one receipt ID. A batch keeps its ID and its counters until a flush
// of it succeeded: the retry of a batch whose commit outcome is unknown
// carries the same ID, so Postgres can tell that the batch was applied.
type flushBatch struct {
	id      string
	counts  map[counterKey]int64
	created time.Time
}

// sealBatch moves every live counter into a new pending batch (nothing when
// there is none) and, when closeRecorder is set, closes the recorder in the
// same step, so every Add either lands in a batch or is rejected and logged
// by Add. A counter that Add records later goes to the live counters and
// never into a batch that a flush may already have sent. The batch ID is a
// UUID v7; an error of the ID source leaves the live counters in place.
func (r *Recorder) sealBatch(closeRecorder bool, now time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if closeRecorder {
		r.closed = true
	}
	if len(r.counts) == 0 {
		return nil
	}
	id, err := uuidv7.NewString()
	if err != nil {
		return err
	}
	r.pending = append(r.pending, &flushBatch{id: id, counts: r.counts, created: now})
	r.counts = map[counterKey]int64{}
	return nil
}

// firstPending returns the oldest batch that no flush has applied yet, or
// nil.
func (r *Recorder) firstPending() *flushBatch {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.pending) == 0 {
		return nil
	}
	return r.pending[0]
}

// ack drops a batch whose flush succeeded (or whose receipt shows that an
// earlier attempt applied it).
func (r *Recorder) ack(b *flushBatch) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pending = slices.DeleteFunc(r.pending, func(p *flushBatch) bool { return p == b })
}

// Pending returns the number of counters that wait for a flush, in the live
// counters and in pending batches (tests and metrics).
func (r *Recorder) Pending() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := len(r.counts)
	for _, b := range r.pending {
		n += len(b.counts)
	}
	return n
}
