// SPDX-License-Identifier: FSL-1.1-ALv2

// Package server builds the accounts-api HTTP handler.
package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// maxOptionsBodyBytes bounds the body read for a server-wide "OPTIONS *"
// request, matching net/http's built-in handler.
const maxOptionsBodyBytes = 4 << 10

// NewHandler returns the root HTTP handler with request logging applied.
//
// The handler answers a server-wide "OPTIONS *" request itself, inside the
// request logger. net/http answers that request before it consults the
// configured Handler unless http.Server.DisableGeneralOptionsHandler is set, so
// the production server must set it (see newServer in cmd/accounts-api);
// otherwise the request is neither routed here nor logged, a later 417 on the
// same connection is not logged by the request head observer, and the
// duration_ms of later http_request_rejected records on that connection is
// measured from the "OPTIONS *" request.
func NewHandler(logger *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealthz(logger))
	return logRequests(logger, withGeneralOptions(mux))
}

// withGeneralOptions answers "OPTIONS *" and passes every other request to
// next. It reproduces net/http's built-in handler: 200 with Content-Length 0,
// reading at most maxOptionsBodyBytes of request body.
func withGeneralOptions(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodOptions || r.RequestURI != "*" {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Length", "0")
		if r.ContentLength != 0 {
			// http.MaxBytesReader tells the server to close the connection
			// after an oversized body through an unexported method of the
			// ResponseWriter that net/http created. Wrappers such as
			// statusRecorder only embed the http.ResponseWriter interface and
			// do not promote that method, so the reader must be given the
			// unwrapped writer or the connection would be kept alive.
			body := http.MaxBytesReader(unwrapResponseWriter(w), r.Body, maxOptionsBodyBytes)
			_, _ = io.Copy(io.Discard, body)
		}
	})
}

// unwrapResponseWriter returns the innermost writer reachable through Unwrap
// methods, which is the one net/http created for the request.
func unwrapResponseWriter(w http.ResponseWriter) http.ResponseWriter {
	for {
		u, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return w
		}
		w = u.Unwrap()
	}
}

type healthResponse struct {
	Status string `json:"status"`
}

func handleHealthz(logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if err := json.NewEncoder(w).Encode(healthResponse{Status: "ok"}); err != nil {
			logger.WarnContext(r.Context(), "health_response_write_failed", slog.Any("error", err))
		}
	}
}

// statusRecorder captures the status code and body size for request logs.
// status is the first committed status, matching what net/http sends to the
// client; wroteHeader reports whether a header has been committed, either by
// the first WriteHeader call or by the first implicit Write. bytes counts the
// response body bytes that net/http accepted from the handler. It equals what
// the client receives for a response that completes. net/http buffers the
// response and discards the buffered part when the handler panics or the
// client disconnects, so the count can exceed what the client received.
// noBody marks a response that has no body on the wire (a HEAD response), so
// body bytes the handler writes are discarded by net/http and must not be
// counted. hijacked reports that the handler took over the connection:
// net/http then sends nothing and tracks no response, so the recorder cannot
// know what status, if any, the client received, and it stops recording
// statuses.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	bytes       int
	wroteHeader bool
	noBody      bool
	hijacked    bool
}

// maxCommitStatus is the highest status code net/http accepts in WriteHeader
// (see checkWriteHeaderCode in net/http/server.go).
const maxCommitStatus = 999

func (r *statusRecorder) WriteHeader(status int) {
	// net/http sends informational 1xx headers (except 101 Switching Protocols)
	// without committing the final status, so they must not be recorded. After
	// a hijack net/http ignores the call, so nothing is committed either.
	// net/http commits only codes from 100 to 999 and panics on any other code
	// before it writes a response, so such a code is never committed: leaving
	// it unrecorded lets the panic fallback in logRequests report 500.
	if !r.hijacked && !r.wroteHeader &&
		status >= http.StatusContinue && status <= maxCommitStatus &&
		(status >= http.StatusOK || status == http.StatusSwitchingProtocols) {
		r.status = status
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(body []byte) (int, error) {
	// An implicit WriteHeader(200) commits the default status, except after a
	// hijack, when net/http rejects the write with http.ErrHijacked.
	if !r.hijacked {
		r.wroteHeader = true
	}
	n, err := r.ResponseWriter.Write(body)
	if !r.noBody {
		r.bytes += n
	}
	return n, err
}

// commitStatus records status as the committed status unless a status is
// already committed, which keeps the first-committed-status rule.
func (r *statusRecorder) commitStatus(status int) {
	if !r.hijacked && !r.wroteHeader {
		r.status = status
		r.wroteHeader = true
	}
}

// FlushError sends buffered data to the client. net/http commits the response
// header on the first flush, with 200 when no status was written, so the
// recorder must observe it: http.ResponseController would otherwise reach the
// underlying writer through Unwrap and the log of a later handler panic would
// substitute 500 for a status the client already received. It delegates through
// a ResponseController so a wrapped inner writer still works, and records the
// status only when the inner writer supports flushing.
func (r *statusRecorder) FlushError() error {
	err := http.NewResponseController(r.ResponseWriter).Flush()
	if !errors.Is(err, http.ErrNotSupported) {
		r.commitStatus(http.StatusOK)
	}
	return err
}

// Flush implements http.Flusher for handlers that assert that interface
// instead of using http.ResponseController. It ignores the error, as
// http.Flusher has no way to return it.
func (r *statusRecorder) Flush() {
	_ = r.FlushError()
}

// Hijack takes over the connection. A hijack only transfers the connection: the
// handler may then send a 101 switch, a raw response with any other status, or
// nothing, and the recorder cannot observe which. A successful hijack therefore
// records the takeover separately from the committed status and commits no
// status itself. A failed hijack (an unsupported writer or HTTP/2) leaves the
// state unchanged.
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := http.NewResponseController(r.ResponseWriter).Hijack()
	if err == nil {
		r.hijacked = true
	}
	return conn, rw, err
}

// Unwrap lets http.ResponseController reach the underlying writer for the
// operations that statusRecorder does not implement itself.
func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

func logRequests(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		// Tell the connection observer that net/http dispatched a request, so it
		// can tell it from one that net/http rejects before the handler.
		if obs := observerFrom(r.Context()); obs != nil {
			obs.handlerStarted()
			defer obs.handlerFinished()
		}
		recorder := &statusRecorder{
			ResponseWriter: w,
			status:         http.StatusOK,
			// net/http reports a HEAD body write as successful but never sends
			// the bytes, so the log must not count them as transmitted.
			noBody: r.Method == http.MethodHead,
		}
		// The record is written from a defer so a panicking handler is logged
		// too. The panic is deliberately not recovered here: net/http keeps its
		// connection-level recovery (including http.ErrAbortHandler handling)
		// and the client sees the aborted connection.
		returned := false
		defer func() {
			status := recorder.status
			if !returned && !recorder.wroteHeader && !recorder.hijacked {
				// The handler did not return and nothing was committed, so
				// net/http will not send a response; record it as a failure.
				// A hijacked connection is excluded: net/http sends no response
				// for it, and the handler may have written its own, so a 500
				// would be a status the recorder cannot know.
				status = http.StatusInternalServerError
			}
			var attrs []slog.Attr
			if recorder.hijacked {
				attrs = append(attrs, slog.Bool("hijacked", true))
			}
			logRequest(r.Context(), logger, r.Method, r.URL.Path, status, time.Since(start), recorder.bytes, attrs...)
		}()
		next.ServeHTTP(recorder, r)
		returned = true
	})
}

// logRequest writes the http_request record. It is the only place that defines
// the record shape, shared by the request logger and by the observer that logs
// requests net/http rejects before the handler runs. extra attributes follow
// the fixed fields; the request logger uses them only for a hijacked
// connection, so the record of every other request keeps its shape.
func logRequest(ctx context.Context, logger *slog.Logger, method, path string, status int, duration time.Duration, bytes int, extra ...slog.Attr) {
	attrs := []slog.Attr{
		slog.String("method", method),
		slog.String("path", path),
		slog.Int("status", status),
		slog.Int64("duration_ms", duration.Milliseconds()),
		slog.Int("bytes", bytes),
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "http_request", append(attrs, extra...)...)
}
