// SPDX-License-Identifier: FSL-1.1-ALv2

package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func newTestHandler(t *testing.T) (http.Handler, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	return NewHandler(logger), &logs
}

// decodeRecords parses every JSON log line written to logs.
func decodeRecords(t *testing.T, logs *bytes.Buffer) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(logs.Bytes()), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatalf("decode log record %q: %v", line, err)
		}
		records = append(records, record)
	}
	return records
}

func TestHealthz(t *testing.T) {
	handler, logs := newTestHandler(t)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	var body healthResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Status != "ok" {
		t.Errorf("status field = %q, want ok", body.Status)
	}

	records := decodeRecords(t, logs)
	if len(records) != 1 {
		t.Fatalf("got %d log records, want exactly 1: %v", len(records), records)
	}
	record := records[0]
	if record["msg"] != "http_request" || record["path"] != "/healthz" || record["status"] != float64(http.StatusOK) {
		t.Errorf("unexpected request log: %v", record)
	}
}

func TestUnknownRouteAndMethod(t *testing.T) {
	handler, _ := newTestHandler(t)
	for _, tc := range []struct {
		method string
		path   string
		want   int
	}{
		{http.MethodGet, "/missing", http.StatusNotFound},
		{http.MethodPost, "/healthz", http.StatusMethodNotAllowed},
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(tc.method, tc.path, nil))
		if recorder.Code != tc.want {
			t.Errorf("%s %s status = %d, want %d", tc.method, tc.path, recorder.Code, tc.want)
		}
	}
}

// serveWithHandler runs handler behind the request logger on a real HTTP
// server and returns the client-observed status and the logged status.
func serveWithHandler(t *testing.T, handler http.HandlerFunc) (clientStatus int, loggedStatus float64, record map[string]any) {
	t.Helper()
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	ts := httptest.NewServer(logRequests(logger, handler))
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/probe")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp.Body.Close()
	// Close waits for in-flight handlers, so the log record is complete.
	ts.Close()

	if err := json.Unmarshal(logs.Bytes(), &record); err != nil {
		t.Fatalf("decode log record: %v (logs %q)", err, logs.String())
	}
	status, _ := record["status"].(float64)
	return resp.StatusCode, status, record
}

func TestRequestLogRecordsFirstCommittedStatus(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		want    int
	}{
		{"double WriteHeader", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			w.WriteHeader(http.StatusOK)
		}, http.StatusInternalServerError},
		{"body only", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("hello"))
		}, http.StatusOK},
		{"body then error header", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("hello"))
			w.WriteHeader(http.StatusInternalServerError)
		}, http.StatusOK},
		{"informational then final", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusEarlyHints)
			w.WriteHeader(http.StatusOK)
		}, http.StatusOK},
		{"no writes", func(http.ResponseWriter, *http.Request) {}, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, logged, record := serveWithHandler(t, tc.handler)
			if client != tc.want {
				t.Errorf("client status = %d, want %d", client, tc.want)
			}
			if logged != float64(tc.want) {
				t.Errorf("logged status = %v, want %d (record %v)", logged, tc.want, record)
			}
		})
	}
}

func TestStatusRecorderWroteHeader(t *testing.T) {
	for _, tc := range []struct {
		name string
		do   func(*statusRecorder)
		want bool
	}{
		{"untouched", func(*statusRecorder) {}, false},
		{"informational", func(r *statusRecorder) { r.WriteHeader(http.StatusEarlyHints) }, false},
		{"switching protocols", func(r *statusRecorder) { r.WriteHeader(http.StatusSwitchingProtocols) }, true},
		{"final", func(r *statusRecorder) { r.WriteHeader(http.StatusNoContent) }, true},
		{"implicit write", func(r *statusRecorder) { _, _ = r.Write([]byte("x")) }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &statusRecorder{ResponseWriter: httptest.NewRecorder(), status: http.StatusOK}
			tc.do(r)
			if r.wroteHeader != tc.want {
				t.Errorf("wroteHeader = %v, want %v", r.wroteHeader, tc.want)
			}
		})
	}
}

// invalidStatusHandler returns a handler that writes a status code that
// net/http rejects. net/http commits only 100..999 and panics on any other
// code before it writes a response, so the client never receives the code.
func invalidStatusHandler(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(code)
	}
}

// TestStatusRecorderInvalidStatusCodes checks that a status code that net/http
// rejects with a panic is never recorded as committed, while valid codes keep
// their recorded status. The inner writer is plainWriter because
// httptest.ResponseRecorder also panics on an invalid code, which would hide
// the recorder state under test.
func TestStatusRecorderInvalidStatusCodes(t *testing.T) {
	for _, tc := range []struct {
		code       int
		wantWrote  bool
		wantStatus int
	}{
		{0, false, http.StatusOK},
		{99, false, http.StatusOK},
		{-1, false, http.StatusOK},
		{1000, false, http.StatusOK},
		{99999, false, http.StatusOK},
		{http.StatusContinue, false, http.StatusOK},
		{199, false, http.StatusOK},
		{http.StatusSwitchingProtocols, true, http.StatusSwitchingProtocols},
		{http.StatusOK, true, http.StatusOK},
		{600, true, 600},
		{999, true, 999},
	} {
		t.Run(fmt.Sprint(tc.code), func(t *testing.T) {
			r := &statusRecorder{ResponseWriter: plainWriter{header: http.Header{}}, status: http.StatusOK}
			r.WriteHeader(tc.code)
			if r.wroteHeader != tc.wantWrote || r.status != tc.wantStatus {
				t.Errorf("status = %d, wroteHeader = %v, want %d, %v", r.status, r.wroteHeader, tc.wantStatus, tc.wantWrote)
			}
		})
	}
}

// startLoggedServer starts a real HTTP server that serves handler behind the
// request logger and writes the logs to the returned buffer. The returned
// channel is closed once the logger has finished its request, which is after
// its deferred record is written even when the handler panics (the outer defer
// runs after the logger's own, while the panic unwinds). net/http's own panic
// trace is discarded to keep test output quiet.
//
// Tests must wait on the channel before they read the buffer. httptest.Server's
// Close waits only for connections that are still tracked, and a hijacked
// connection leaves that set at once, so Close does not order the deferred log
// write before the read of the shared buffer for a hijacking handler.
func startLoggedServer(handler http.HandlerFunc) (ts *httptest.Server, logs *bytes.Buffer, logged <-chan struct{}) {
	logs = &bytes.Buffer{}
	done := make(chan struct{})
	logger := slog.New(slog.NewJSONHandler(logs, nil))
	logging := logRequests(logger, handler)
	ts = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		logging.ServeHTTP(w, r)
	}))
	ts.Config.ErrorLog = log.New(io.Discard, "", 0)
	ts.Start()
	return ts, logs, done
}

// waitLogged blocks until the request logger has written its record, then
// closes the server, which waits for the remaining non-hijacked connections.
func waitLogged(t *testing.T, ts *httptest.Server, logged <-chan struct{}) {
	t.Helper()
	select {
	case <-logged:
	case <-time.After(10 * time.Second):
		t.Fatal("the request logger did not finish the request")
	}
	ts.Close()
}

// serveRequest sends one GET to handler behind the request logger on a real
// HTTP server and returns the client error (nil when a response arrived), the
// client-observed status (0 on error) and the decoded log records. net/http's
// own panic trace is discarded to keep test output quiet.
func serveRequest(t *testing.T, handler http.HandlerFunc) (clientErr error, clientStatus int, records []map[string]any) {
	t.Helper()
	ts, logs, logged := startLoggedServer(handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/probe")
	if err == nil {
		clientStatus = resp.StatusCode
		resp.Body.Close()
	}
	waitLogged(t, ts, logged)
	return err, clientStatus, decodeRecords(t, logs)
}

// TestRequestLogOnPanic checks the status and bytes of the record of a handler
// that panics, together with what the client received. bytes counts the body
// bytes net/http accepted from the handler, not what reached the client:
// net/http buffers the response (a 2048-byte buffer in front of the chunked
// writer) and discards the buffered part when the handler panics, so the
// logged count exceeds wantClientBody for bytes that were still buffered. The
// client body is read with serveCommittedRequest, so a flushed part that
// reached the client before the abort is counted.
func TestRequestLogOnPanic(t *testing.T) {
	for _, tc := range []struct {
		name           string
		handler        http.HandlerFunc
		wantStatus     int
		wantBytes      int
		wantClientBody int
	}{
		{"before any write", func(http.ResponseWriter, *http.Request) {
			panic("boom")
		}, http.StatusInternalServerError, 0, 0},
		{"after committed status", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusAccepted)
			panic("boom")
		}, http.StatusAccepted, 0, 0},
		{"after body write", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("partial"))
			panic("boom")
		}, http.StatusOK, len("partial"), 0},
		{"after body write and flush", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("partial"))
			flushOrPanic(w)
			panic("boom")
		}, http.StatusOK, len("partial"), len("partial")},
		{"flush then more body", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("partial"))
			flushOrPanic(w)
			_, _ = w.Write([]byte("abc"))
			panic("boom")
		}, http.StatusOK, len("partial") + len("abc"), len("partial")},
		{"full buffer plus one byte", func(w http.ResponseWriter, _ *http.Request) {
			// The first write fills the 2048-byte net/http buffer exactly. The
			// second write forces net/http to send that buffer and keep the one
			// new byte buffered, which the panic then discards.
			_, _ = w.Write(bytes.Repeat([]byte("x"), 2048))
			_, _ = w.Write([]byte("y"))
			panic("boom")
		}, http.StatusOK, 2049, 2048},
		{"invalid status 0", invalidStatusHandler(0), http.StatusInternalServerError, 0, 0},
		{"invalid status 99", invalidStatusHandler(99), http.StatusInternalServerError, 0, 0},
		{"invalid status -1", invalidStatusHandler(-1), http.StatusInternalServerError, 0, 0},
		{"invalid status 1000", invalidStatusHandler(1000), http.StatusInternalServerError, 0, 0},
		{"invalid status 99999", invalidStatusHandler(99999), http.StatusInternalServerError, 0, 0},
		{"abort handler", func(http.ResponseWriter, *http.Request) {
			panic(http.ErrAbortHandler)
		}, http.StatusInternalServerError, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clientErr, clientStatus, bodyErr, clientBody, records := serveCommittedRequest(t, tc.handler)
			// net/http recovers the re-raised panic by closing the connection.
			// Without a flushed header the client sees a transport error rather
			// than a 500 status; with one it sees the status followed by a body
			// read error.
			if clientErr == nil && bodyErr == nil {
				t.Errorf("client status = %d with a complete body, want an error from the aborted connection", clientStatus)
			}
			if clientBody != int64(tc.wantClientBody) {
				t.Errorf("client received %d body bytes, want %d", clientBody, tc.wantClientBody)
			}
			if len(records) != 1 {
				t.Fatalf("got %d log records, want exactly 1: %v", len(records), records)
			}
			record := records[0]
			if record["msg"] != "http_request" || record["method"] != http.MethodGet || record["path"] != "/probe" {
				t.Errorf("unexpected request log: %v", record)
			}
			if record["status"] != float64(tc.wantStatus) {
				t.Errorf("logged status = %v, want %d", record["status"], tc.wantStatus)
			}
			if record["bytes"] != float64(tc.wantBytes) {
				t.Errorf("logged bytes = %v, want %d (client received %d)", record["bytes"], tc.wantBytes, clientBody)
			}
			if duration, ok := record["duration_ms"].(float64); !ok || duration < 0 {
				t.Errorf("duration_ms = %v, want a non-negative number", record["duration_ms"])
			}
		})
	}
}

// flushOrPanic flushes the response and panics when the flush fails, so a
// handler that cannot flush fails the test instead of passing silently.
func flushOrPanic(w http.ResponseWriter) {
	if err := http.NewResponseController(w).Flush(); err != nil {
		panic(err)
	}
}

// serveCommittedRequest is like serveRequest, but it also reads the response
// body, so a handler that commits a response and then panics is observed as the
// client sees it: a response with a status followed by a body read error.
// clientBody is the number of body bytes the client read before the read ended.
func serveCommittedRequest(t *testing.T, handler http.HandlerFunc) (clientErr error, clientStatus int, bodyErr error, clientBody int64, records []map[string]any) {
	t.Helper()
	ts, logs, logged := startLoggedServer(handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/probe")
	if err == nil {
		clientStatus = resp.StatusCode
		clientBody, bodyErr = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	waitLogged(t, ts, logged)
	return err, clientStatus, bodyErr, clientBody, decodeRecords(t, logs)
}

// TestRequestLogRecordsFlushedAndHijackedStatus checks that a response
// committed through http.ResponseController is logged with the status the
// client received, also when the handler then panics.
func TestRequestLogRecordsFlushedAndHijackedStatus(t *testing.T) {
	for _, tc := range []struct {
		name       string
		handler    http.HandlerFunc
		panics     bool
		wantStatus int
	}{
		{"flush then panic", func(w http.ResponseWriter, _ *http.Request) {
			if err := http.NewResponseController(w).Flush(); err != nil {
				panic(err)
			}
			panic("boom")
		}, true, http.StatusOK},
		{"flush then return", func(w http.ResponseWriter, _ *http.Request) {
			if err := http.NewResponseController(w).Flush(); err != nil {
				panic(err)
			}
		}, false, http.StatusOK},
		{"explicit status, flush, then panic", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
			if err := http.NewResponseController(w).Flush(); err != nil {
				panic(err)
			}
			panic("boom")
		}, true, http.StatusNoContent},
		{"informational, flush, final status, then panic", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusEarlyHints)
			if err := http.NewResponseController(w).Flush(); err != nil {
				panic(err)
			}
			w.WriteHeader(http.StatusOK)
			panic("boom")
		}, true, http.StatusOK},
		{"flusher assertion then panic", func(w http.ResponseWriter, _ *http.Request) {
			flusher, ok := w.(http.Flusher)
			if !ok {
				panic("the response writer is not an http.Flusher")
			}
			flusher.Flush()
			panic("boom")
		}, true, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clientErr, clientStatus, bodyErr, _, records := serveCommittedRequest(t, tc.handler)
			if clientErr != nil {
				t.Fatalf("client error = %v, want a response", clientErr)
			}
			if clientStatus != tc.wantStatus {
				t.Errorf("client status = %d, want %d", clientStatus, tc.wantStatus)
			}
			// net/http aborts the connection after a panic. The client already
			// has the header, so the failure shows up as a body read error.
			if tc.panics && bodyErr == nil && tc.wantStatus != http.StatusNoContent {
				t.Errorf("body read error = nil, want an error from the aborted connection")
			}
			if len(records) != 1 {
				t.Fatalf("got %d log records, want exactly 1: %v", len(records), records)
			}
			if records[0]["status"] != float64(tc.wantStatus) {
				t.Errorf("logged status = %v, want %d (client received %d)", records[0]["status"], tc.wantStatus, clientStatus)
			}
			if _, ok := records[0]["hijacked"]; ok {
				t.Errorf("record has a hijacked field %v, want none for a request that did not hijack", records[0]["hijacked"])
			}
		})
	}

	// A hijack only transfers the connection. The handler decides what, if
	// anything, the client receives, and the recorder cannot observe it, so the
	// log marks the takeover with hijacked, commits no status of its own (in
	// particular not 101) and never substitutes 500 for a later panic. The
	// logged status is the recorder's default of 200 here, which is not a
	// client-observed status for these connections.
	const rawNotFound = "HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"
	const rawOK = "HTTP/1.1 200 OK\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"
	for _, tc := range []struct {
		name         string
		response     string
		panics       bool
		wantClient   int // 0 means a transport error is expected
		wantLogged   int
		wantHijacked bool
	}{
		{"hijack, raw 200 response, then return", rawOK, false, http.StatusOK, http.StatusOK, true},
		{"hijack, raw 404 response, then return", rawNotFound, false, http.StatusNotFound, http.StatusOK, true},
		{"hijack, raw 404 response, then panic", rawNotFound, true, http.StatusNotFound, http.StatusOK, true},
		{"hijack, no response, then panic", "", true, 0, http.StatusOK, true},
		{"hijack, no response, then return", "", false, 0, http.StatusOK, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clientErr, clientStatus, records := serveRequest(t, func(w http.ResponseWriter, _ *http.Request) {
				conn, _, err := http.NewResponseController(w).Hijack()
				if err != nil {
					panic(err)
				}
				if tc.response != "" {
					if _, err := io.WriteString(conn, tc.response); err != nil {
						panic(err)
					}
				}
				_ = conn.Close()
				if tc.panics {
					panic("boom")
				}
			})
			if tc.wantClient == 0 && clientErr == nil {
				t.Errorf("client status = %d, want a transport error from the closed connection", clientStatus)
			}
			if tc.wantClient != 0 && (clientErr != nil || clientStatus != tc.wantClient) {
				t.Errorf("client status = %d, error = %v, want status %d", clientStatus, clientErr, tc.wantClient)
			}
			if len(records) != 1 {
				t.Fatalf("got %d log records, want exactly 1: %v", len(records), records)
			}
			if records[0]["status"] != float64(tc.wantLogged) {
				t.Errorf("logged status = %v, want %d", records[0]["status"], tc.wantLogged)
			}
			if got, _ := records[0]["hijacked"].(bool); got != tc.wantHijacked {
				t.Errorf("logged hijacked = %v, want %v", records[0]["hijacked"], tc.wantHijacked)
			}
		})
	}
}

// hijackWriter is a response writer whose Hijack succeeds without a real
// connection, so the recorder state after a takeover can be checked directly.
type hijackWriter struct{ http.ResponseWriter }

func (hijackWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) { return nil, nil, nil }

func TestStatusRecorderHijackState(t *testing.T) {
	noop := func(*statusRecorder) {}
	for _, tc := range []struct {
		name       string
		before     func(*statusRecorder) // runs before the hijack
		after      func(*statusRecorder) // runs after the hijack
		wantStatus int
		wantWrote  bool
	}{
		{"hijack commits no status", noop, noop, http.StatusOK, false},
		{"status committed before the hijack is kept", func(r *statusRecorder) {
			r.WriteHeader(http.StatusNoContent)
		}, noop, http.StatusNoContent, true},
		{"WriteHeader after the hijack is ignored", noop, func(r *statusRecorder) {
			r.WriteHeader(http.StatusInternalServerError)
		}, http.StatusOK, false},
		{"Write after the hijack commits nothing", noop, func(r *statusRecorder) {
			_, _ = r.Write([]byte("raw"))
		}, http.StatusOK, false},
		{"flush after the hijack commits nothing", noop, func(r *statusRecorder) {
			_ = r.FlushError()
		}, http.StatusOK, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &statusRecorder{ResponseWriter: hijackWriter{httptest.NewRecorder()}, status: http.StatusOK}
			tc.before(r)
			if _, _, err := r.Hijack(); err != nil {
				t.Fatalf("Hijack error = %v", err)
			}
			if !r.hijacked {
				t.Errorf("hijacked = false, want true")
			}
			tc.after(r)
			if r.status != tc.wantStatus || r.wroteHeader != tc.wantWrote {
				t.Errorf("status = %d, wroteHeader = %v, want %d, %v", r.status, r.wroteHeader, tc.wantStatus, tc.wantWrote)
			}
		})
	}
}

// plainWriter implements only http.ResponseWriter, so neither flushing nor
// hijacking is supported through it.
type plainWriter struct{ header http.Header }

func (w plainWriter) Header() http.Header          { return w.header }
func (plainWriter) Write(body []byte) (int, error) { return len(body), nil }
func (plainWriter) WriteHeader(int)                {}

func TestStatusRecorderFlushAndHijackState(t *testing.T) {
	for _, tc := range []struct {
		name       string
		inner      http.ResponseWriter
		do         func(*statusRecorder) error
		wantErr    bool
		wantStatus int
		wantWrote  bool
	}{
		{"flush", httptest.NewRecorder(), func(r *statusRecorder) error { return r.FlushError() }, false, http.StatusOK, true},
		{"flush after explicit status", httptest.NewRecorder(), func(r *statusRecorder) error {
			r.WriteHeader(http.StatusNoContent)
			return r.FlushError()
		}, false, http.StatusNoContent, true},
		{"flush after informational", httptest.NewRecorder(), func(r *statusRecorder) error {
			r.WriteHeader(http.StatusEarlyHints)
			return r.FlushError()
		}, false, http.StatusOK, true},
		{"legacy flush", httptest.NewRecorder(), func(r *statusRecorder) error { r.Flush(); return nil }, false, http.StatusOK, true},
		{"flush on a writer that cannot flush", plainWriter{header: http.Header{}}, func(r *statusRecorder) error { return r.FlushError() }, true, http.StatusOK, false},
		{"hijack on a writer that cannot hijack", httptest.NewRecorder(), func(r *statusRecorder) error {
			_, _, err := r.Hijack()
			return err
		}, true, http.StatusOK, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &statusRecorder{ResponseWriter: tc.inner, status: http.StatusOK}
			err := tc.do(r)
			if tc.wantErr != (err != nil) {
				t.Errorf("error = %v, want error %v", err, tc.wantErr)
			}
			if tc.wantErr && !errors.Is(err, http.ErrNotSupported) {
				t.Errorf("error = %v, want http.ErrNotSupported", err)
			}
			if r.status != tc.wantStatus || r.wroteHeader != tc.wantWrote {
				t.Errorf("status = %d, wroteHeader = %v, want %d, %v", r.status, r.wroteHeader, tc.wantStatus, tc.wantWrote)
			}
		})
	}
}

func TestRequestLogRepanicsSamePanicValue(t *testing.T) {
	sentinel := errors.New("sentinel")
	for _, value := range []any{sentinel, http.ErrAbortHandler} {
		var logs bytes.Buffer
		logger := slog.New(slog.NewJSONHandler(&logs, nil))
		handler := logRequests(logger, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			panic(value)
		}))

		var recovered any
		func() {
			defer func() { recovered = recover() }()
			handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/probe", nil))
		}()
		if recovered != value {
			t.Errorf("recovered %v, want the original panic value %v", recovered, value)
		}
		if got := len(decodeRecords(t, &logs)); got != 1 {
			t.Errorf("panic %v: got %d log records, want exactly 1", value, got)
		}
	}
}

// TestRequestLogBytesForHead checks that the logged bytes are the body bytes
// transmitted to the client: a HEAD response carries no body, so it logs 0 with
// the same status as the equivalent GET, while GET logs the transmitted length.
func TestRequestLogBytesForHead(t *testing.T) {
	for _, tc := range []struct {
		method         string
		path           string
		wantStatus     int
		wantBytes      float64
		wantBodyLength int
	}{
		{http.MethodGet, "/healthz", http.StatusOK, 16, 16},
		{http.MethodHead, "/healthz", http.StatusOK, 0, 0},
		{http.MethodGet, "/unknown", http.StatusNotFound, 19, 19},
		{http.MethodHead, "/unknown", http.StatusNotFound, 0, 0},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			handler, logs := newTestHandler(t)
			ts := httptest.NewServer(handler)
			defer ts.Close()

			req, err := http.NewRequest(tc.method, ts.URL+tc.path, nil)
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("do request: %v", err)
			}
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			if resp.StatusCode != tc.wantStatus {
				t.Errorf("client status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			if len(body) != tc.wantBodyLength {
				t.Errorf("client body length = %d, want %d", len(body), tc.wantBodyLength)
			}
			// The response headers must be unchanged for HEAD: the same
			// Content-Length as the GET, without a body.
			if tc.method == http.MethodHead && resp.ContentLength <= 0 {
				t.Errorf("HEAD Content-Length = %d, want the GET body length", resp.ContentLength)
			}
			// Close waits for in-flight handlers, so the record is complete.
			ts.Close()

			records := decodeRecords(t, logs)
			if len(records) != 1 {
				t.Fatalf("got %d log records, want exactly 1: %v", len(records), records)
			}
			record := records[0]
			if record["method"] != tc.method || record["path"] != tc.path {
				t.Errorf("unexpected request log: %v", record)
			}
			if record["status"] != float64(tc.wantStatus) {
				t.Errorf("logged status = %v, want %d", record["status"], tc.wantStatus)
			}
			if record["bytes"] != tc.wantBytes {
				t.Errorf("logged bytes = %v, want %v", record["bytes"], tc.wantBytes)
			}
		})
	}
}
