// SPDX-License-Identifier: FSL-1.1-ALv2

package work

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"
)

type watchDeadlineRecorder struct {
	*httptest.ResponseRecorder
	deadlines []time.Time
}

func (r *watchDeadlineRecorder) SetWriteDeadline(at time.Time) error {
	r.deadlines = append(r.deadlines, at)
	return nil
}

func TestWatchQueueWriteDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := &Handler{watch: WatchQueueTiming{Lifetime: watchLifetime, WriteGrace: watchWriteGrace}}
		start := time.Now()
		writer := &watchDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
		check := func() {
			t.Helper()
			if len(writer.deadlines) != 1 || !writer.deadlines[0].Equal(start.Add(watchLifetime+watchWriteGrace)) {
				t.Fatalf("write deadlines = %v, want one absolute deadline", writer.deadlines)
			}
			if !writer.deadlines[0].Before(start.Add(14 * time.Minute)) {
				t.Fatalf("write deadline %s exceeds the documented limit", writer.deadlines[0].Sub(start))
			}
		}
		handler := h.withWatchQueueDeadline(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			check()
			rotateAt, ok := req.Context().Value(watchQueueRotationKey{}).(time.Time)
			if !ok || !rotateAt.Equal(start.Add(watchLifetime)) {
				t.Fatalf("rotation deadline = %v, want %v", rotateAt, start.Add(watchLifetime))
			}
			w.WriteHeader(http.StatusOK)
		}), nil)
		handler.ServeHTTP(writer, httptest.NewRequest(http.MethodPost, "/", nil))
		check()
	})
}
