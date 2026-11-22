// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// DefaultUnaryBodyReadDeadline bounds how long a unary request may take to
// deliver its body (Timing.UnaryBodyReadDeadline).
const DefaultUnaryBodyReadDeadline = 30 * time.Second

// UnaryBodyDeadline returns the body read deadline of unary requests.
func (t Timing) UnaryBodyDeadline() time.Duration {
	if t.UnaryBodyReadDeadline > 0 {
		return t.UnaryBodyReadDeadline
	}
	return DefaultUnaryBodyReadDeadline
}

// UnaryReadDeadline bounds the upload of the request body of the unary
// procedures that next serves. The http.Server has no ReadTimeout, because
// the agent channel and the relay connection are long-lived streams, and
// Connect reads a unary request body completely before any interceptor runs,
// so without this handler a client that sends its headers and then stalls, or
// sends one byte per second, holds a goroutine and a connection slot for as
// long as it likes.
//
// A POST request whose body is not empty gets a read deadline of timeout,
// counted from the moment the handler starts, through the response
// controller; a body that is not complete by then fails with
// deadline_exceeded. The deadline is lifted when the body has been read to its
// end, so that a complete upload leaves no armed deadline behind for the rest
// of the request, for example a unary handler that takes longer than timeout
// to answer. A failed body read keeps the deadline armed, so that it still
// bounds any later read of an upload that stalled, such as the one that
// net/http makes to discard the rest of the body when the request ends.
//
// streaming lists the URL paths of the procedures that next serves and that
// are not unary (for example a bidirectional stream); they pass through
// untouched. Do not wrap a handler that arms its own body deadline (the
// RelayService request gate does) or a handler that only serves streams.
func UnaryReadDeadline(next http.Handler, timeout time.Duration, logger *slog.Logger, streaming ...string) http.Handler {
	exempt := make(map[string]bool, len(streaming))
	for _, p := range streaming {
		exempt[p] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Body == nil || r.Body == http.NoBody || exempt[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}
		rc := http.NewResponseController(w)
		if err := rc.SetReadDeadline(time.Now().Add(timeout)); err != nil {
			// Without the controller the body read is unbounded, as before
			// this handler existed.
			if logger != nil {
				logger.WarnContext(r.Context(), "unary_body_deadline_unsupported", slog.String("path", r.URL.Path), slog.String("error", err.Error()))
			}
			next.ServeHTTP(w, r)
			return
		}
		r.Body = &deadlineBody{ReadCloser: r.Body, lift: func() { _ = rc.SetReadDeadline(time.Time{}) }}
		next.ServeHTTP(w, r)
	})
}

// deadlineBody lifts the read deadline of its request when the body ends.
type deadlineBody struct {
	io.ReadCloser
	lift func()
	once sync.Once
}

func (b *deadlineBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err == io.EOF {
		b.once.Do(b.lift)
	}
	return n, err
}
