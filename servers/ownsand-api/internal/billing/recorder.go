// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"log/slog"
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
// Metering.Flush). It is safe for concurrent use.
type Recorder struct {
	logger *slog.Logger

	mu     sync.Mutex
	counts map[counterKey]int64
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
	r.counts[k] = saturatingAdd(r.counts[k], quantity)
	r.mu.Unlock()
}

// take removes and returns every counter.
func (r *Recorder) take() map[counterKey]int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.counts) == 0 {
		return nil
	}
	out := r.counts
	r.counts = map[counterKey]int64{}
	return out
}

// restore adds counters that take returned back, after a failed flush.
func (r *Recorder) restore(counts map[counterKey]int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, v := range counts {
		r.counts[k] = saturatingAdd(r.counts[k], v)
	}
}

// Pending returns the number of counters that wait for a flush (tests and
// metrics).
func (r *Recorder) Pending() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.counts)
}
