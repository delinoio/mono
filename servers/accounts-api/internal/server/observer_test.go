// SPDX-License-Identifier: FSL-1.1-ALv2

package server

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

type pendingSummary struct {
	method       string
	path         string
	expectFailed bool
}

func newTestObserver() (*connObserver, *bytes.Buffer) {
	var logs bytes.Buffer
	return newConnObserver(slog.New(slog.NewJSONHandler(&logs, nil))), &logs
}

func summarize(o *connObserver) []pendingSummary {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []pendingSummary
	for _, p := range o.pending {
		out = append(out, pendingSummary{p.method, p.path, p.expectFailed})
	}
	return out
}

func TestObserverParsesRequestSequences(t *testing.T) {
	get := func(path string) string { return "GET " + path + " HTTP/1.1\r\nHost: t\r\n\r\n" }
	for _, tc := range []struct {
		name   string
		stream string
		want   []pendingSummary
	}{
		{"two gets", get("/a") + get("/b"), []pendingSummary{{"GET", "/a", false}, {"GET", "/b", false}}},
		{"bare LF line ends", "GET /a HTTP/1.1\nHost: t\n\n" + get("/b"), []pendingSummary{{"GET", "/a", false}, {"GET", "/b", false}}},
		{"options star", "OPTIONS * HTTP/1.1\r\nHost: t\r\n\r\n", []pendingSummary{{"OPTIONS", "*", false}}},
		{"absolute form", "GET http://t/abs?q=1 HTTP/1.1\r\nHost: t\r\n\r\n", []pendingSummary{{"GET", "/abs", false}}},
		{
			"content-length body is skipped",
			"POST /a HTTP/1.1\r\nHost: t\r\nContent-Length: 11\r\n\r\nGET /x HTTP" + get("/b"),
			[]pendingSummary{{"POST", "/a", false}, {"GET", "/b", false}},
		},
		{
			"chunked body with extension and trailer is skipped",
			"POST /a HTTP/1.1\r\nHost: t\r\nTransfer-Encoding: chunked\r\n\r\n3;ext=1\r\nabc\r\n0A\r\n0123456789\r\n0\r\nX-Trailer: v\r\n\r\n" + get("/b"),
			[]pendingSummary{{"POST", "/a", false}, {"GET", "/b", false}},
		},
		{
			"chunked body without trailer is skipped",
			"POST /a HTTP/1.1\r\nHost: t\r\nTransfer-Encoding: chunked\r\n\r\n1\r\nz\r\n0\r\n\r\n" + get("/b"),
			[]pendingSummary{{"POST", "/a", false}, {"GET", "/b", false}},
		},
		{
			"unsupported expectation stops parsing",
			get("/a") + "GET /b HTTP/1.1\r\nHost: t\r\nExpect: a-pony\r\n\r\n" + get("/c"),
			[]pendingSummary{{"GET", "/a", false}, {"GET", "/b", true}},
		},
		{
			"100-continue is supported",
			"POST /a HTTP/1.1\r\nHost: t\r\nExpect: 100-continue\r\nContent-Length: 2\r\n\r\nhi" + get("/b"),
			[]pendingSummary{{"POST", "/a", false}, {"GET", "/b", false}},
		},
		{
			"connection close ends the stream",
			"GET /a HTTP/1.1\r\nHost: t\r\nConnection: close\r\n\r\n" + get("/b"),
			[]pendingSummary{{"GET", "/a", false}},
		},
		{"malformed request line stops parsing", get("/a") + "NOT A REQUEST\r\n\r\n" + get("/b"), []pendingSummary{{"GET", "/a", false}}},
		{"leading empty line stops parsing", "\r\n" + get("/a"), nil},
		{
			"invalid chunk size stops parsing",
			"POST /a HTTP/1.1\r\nHost: t\r\nTransfer-Encoding: chunked\r\n\r\nzz\r\nabc\r\n0\r\n\r\n" + get("/b"),
			[]pendingSummary{{"POST", "/a", false}},
		},
		{
			"missing CRLF after chunk data stops parsing",
			"POST /a HTTP/1.1\r\nHost: t\r\nTransfer-Encoding: chunked\r\n\r\n1\r\nzXX0\r\n\r\n" + get("/b"),
			[]pendingSummary{{"POST", "/a", false}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			whole, _ := newTestObserver()
			whole.feed([]byte(tc.stream))
			if got := summarize(whole); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("whole stream: pending = %v, want %v", got, tc.want)
			}

			// The result must not depend on how the stream is split into reads.
			bytewise, _ := newTestObserver()
			for i := 0; i < len(tc.stream); i++ {
				bytewise.feed([]byte{tc.stream[i]})
			}
			if got := summarize(bytewise); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("byte by byte: pending = %v, want %v", got, tc.want)
			}

			rng := rand.New(rand.NewSource(1))
			for range 50 {
				split, _ := newTestObserver()
				rest := []byte(tc.stream)
				for len(rest) > 0 {
					n := 1 + rng.Intn(len(rest))
					split.feed(rest[:n])
					rest = rest[n:]
				}
				if got := summarize(split); !reflect.DeepEqual(got, tc.want) {
					t.Fatalf("random split: pending = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestObserverBoundsHeadSize(t *testing.T) {
	o, _ := newTestObserver()
	o.feed([]byte("GET /a HTTP/1.1\r\nHost: t\r\n"))
	// A header section larger than net/http accepts, fed in small reads.
	chunk := []byte("X-Fill: " + strings.Repeat("a", 1000) + "\r\n")
	for i := 0; i < (maxObservedHeadBytes/len(chunk))+5; i++ {
		o.feed(chunk)
		o.mu.Lock()
		size := cap(o.head)
		o.mu.Unlock()
		// append may over-allocate, but never past twice the limit.
		if size > 2*maxObservedHeadBytes {
			t.Fatalf("head buffer capacity %d exceeds twice the limit %d", size, maxObservedHeadBytes)
		}
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.phase != phaseStopped || o.head != nil {
		t.Errorf("phase = %v, head retained = %v, want a stopped observer holding no head", o.phase, o.head != nil)
	}
}

func TestObserverAcceptsHeadAtTheNetHTTPLimit(t *testing.T) {
	o, _ := newTestObserver()
	prefix := "GET /big HTTP/1.1\r\nHost: t\r\nX-Fill: "
	suffix := "\r\n\r\n"
	fill := strings.Repeat("a", maxObservedHeadBytes-len(prefix)-len(suffix))
	o.feed([]byte(prefix + fill + suffix))
	if got := summarize(o); len(got) != 1 || got[0].path != "/big" {
		t.Errorf("pending = %v, want the head of exactly the limit to be observed", got)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if cap(o.head) > retainedHeadCap {
		t.Errorf("head capacity %d retained after the request, want at most %d", cap(o.head), retainedHeadCap)
	}
}

func TestObserverBoundsPendingRequests(t *testing.T) {
	o, _ := newTestObserver()
	request := []byte("GET / HTTP/1.1\r\nHost: t\r\n\r\n")
	for range maxPendingRequests + 10 {
		o.feed(request)
	}
	if got := len(summarize(o)); got != maxPendingRequests {
		t.Errorf("pending = %d, want the cap %d", got, maxPendingRequests)
	}
}

func TestObserverBoundsChunkedScratch(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"chunk size line", strings.Repeat("1", maxChunkLineBytes+10)},
		{"trailer", "0\r\n" + strings.Repeat("X: y\r\n", maxTrailerBytes)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, _ := newTestObserver()
			o.feed([]byte("POST /a HTTP/1.1\r\nHost: t\r\nTransfer-Encoding: chunked\r\n\r\n"))
			for i := 0; i < len(tc.body); i += 100 {
				o.feed([]byte(tc.body[i:min(i+100, len(tc.body))]))
			}
			o.mu.Lock()
			defer o.mu.Unlock()
			if o.phase != phaseStopped || o.line != nil {
				t.Errorf("phase = %v, scratch retained = %v, want a stopped observer", o.phase, o.line != nil)
			}
		})
	}
}

func TestHandlerDequeuesPendingRequests(t *testing.T) {
	o, _ := newTestObserver()
	o.feed([]byte("GET /a HTTP/1.1\r\nHost: t\r\n\r\nGET /b HTTP/1.1\r\nHost: t\r\n\r\n"))
	o.handlerStarted()
	if got := summarize(o); len(got) != 1 || got[0].path != "/b" {
		t.Errorf("pending after one handler = %v, want only /b", got)
	}
	o.handlerFinished()
	o.handlerStarted()
	o.handlerFinished()
	// An extra call, such as after the observer gave up, must not break.
	o.handlerStarted()
	o.handlerFinished()
	if got := summarize(o); len(got) != 0 {
		t.Errorf("pending = %v, want none", got)
	}
}

const expectationFailedResponse = "HTTP/1.1 417 Expectation Failed\r\nConnection: close\r\nContent-Length: 0\r\nDate: Thu, 05 Nov 2026 00:00:00 GMT\r\n\r\n"

func TestClaimRejection(t *testing.T) {
	const badRequest = "GET /bad HTTP/1.1\r\nHost: t\r\nExpect: a-pony\r\n\r\n"
	for _, tc := range []struct {
		name   string
		setup  func(o *connObserver)
		write  string
		wantOK bool
	}{
		{"417 for the oldest pending rejected request", func(*connObserver) {}, expectationFailedResponse, true},
		{"417 answering an HTTP/1.0 request", func(*connObserver) {}, strings.Replace(expectationFailedResponse, "HTTP/1.1", "HTTP/1.0", 1), true},
		{"417 of another protocol version", func(*connObserver) {}, strings.Replace(expectationFailedResponse, "HTTP/1.1", "HTTP/2.0", 1), false},
		{"short write", func(*connObserver) {}, "HTTP/1.", false},
		{"handler still running", func(o *connObserver) { o.inFlight = 1 }, expectationFailedResponse, false},
		{"other status", func(*connObserver) {}, "HTTP/1.1 200 OK\r\nConnection: close\r\n\r\n", false},
		{"body continuation", func(*connObserver) {}, "xxHTTP/1.1 417 Expectation Failed\r\nConnection: close\r\n\r\n", false},
		{"417 without Connection close", func(*connObserver) {}, "HTTP/1.1 417 Expectation Failed\r\nContent-Length: 0\r\n\r\n", false},
		{"Connection close only in the body", func(*connObserver) {}, "HTTP/1.1 417 Expectation Failed\r\nContent-Length: 20\r\n\r\n\r\nConnection: close\r\n", false},
		{"Connection close is the first header", func(*connObserver) {}, "HTTP/1.1 417 Expectation Failed\r\nConnection: close\r\n\r\n", true},
		{"already claimed", func(o *connObserver) { o.rejected = true }, expectationFailedResponse, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, _ := newTestObserver()
			o.feed([]byte(badRequest))
			tc.setup(o)
			_, _, ok := o.claimRejection([]byte(tc.write))
			if ok != tc.wantOK {
				t.Errorf("claimRejection = %v, want %v", ok, tc.wantOK)
			}
		})
	}

	t.Run("a request without a rejectable Expect is never claimed", func(t *testing.T) {
		o, _ := newTestObserver()
		o.feed([]byte("GET /a HTTP/1.1\r\nHost: t\r\n\r\n"))
		if _, _, ok := o.claimRejection([]byte(expectationFailedResponse)); ok {
			t.Error("claimed a 417 written for a request without an unsupported Expect header")
		}
	})

	t.Run("claimed once", func(t *testing.T) {
		o, _ := newTestObserver()
		o.feed([]byte(badRequest))
		if _, _, ok := o.claimRejection([]byte(expectationFailedResponse)); !ok {
			t.Fatal("first claim failed")
		}
		if _, _, ok := o.claimRejection([]byte(expectationFailedResponse)); ok {
			t.Error("second claim succeeded, want exactly one record per rejection")
		}
	})
}

func TestUnsupportedExpect(t *testing.T) {
	for _, tc := range []struct {
		values []string
		want   bool
	}{
		{nil, false},
		{[]string{""}, false},
		{[]string{"100-continue"}, false},
		{[]string{"100-Continue"}, false},
		{[]string{"100-continue, a-pony"}, false},
		{[]string{"a-pony, 100-continue"}, false},
		{[]string{"a-pony,100-continue"}, false},
		{[]string{"a-pony\t100-continue"}, false},
		{[]string{"a-pony"}, true},
		{[]string{"x100-continue"}, true},
		{[]string{"100-continued"}, true},
		{[]string{"100-continue-a-pony"}, true},
		{[]string{"a-pony", "100-continue"}, true},
		{[]string{"100-continue", "a-pony"}, false},
	} {
		h := http.Header{}
		for _, v := range tc.values {
			h.Add("Expect", v)
		}
		if got := unsupportedExpect(h); got != tc.want {
			t.Errorf("unsupportedExpect(%q) = %v, want %v", tc.values, got, tc.want)
		}
	}
}

// TestObservedConnForwardsTrafficUnchanged checks that the wrapper passes bytes
// through unmodified in both directions and keeps CloseWrite, which net/http
// uses to half-close, and that observing a connection starts no goroutine.
func TestObservedConnForwardsTrafficUnchanged(t *testing.T) {
	before := runtime.NumGoroutine()
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	observed := ObserveListener(listener, logger)

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := observed.Accept()
		if err != nil {
			close(accepted)
			return
		}
		accepted <- conn
	}()
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	conn := <-accepted
	if conn == nil {
		t.Fatal("accept failed")
	}
	if _, ok := conn.(interface{ CloseWrite() error }); !ok {
		t.Error("observed connection hides CloseWrite")
	}

	request := "GET /a HTTP/1.1\r\nHost: t\r\nExpect: a-pony\r\n\r\n"
	if _, err := io.WriteString(client, request); err != nil {
		t.Fatalf("write request: %v", err)
	}
	received := make([]byte, len(request))
	if _, err := io.ReadFull(conn, received); err != nil || string(received) != request {
		t.Fatalf("server read %q, %v; want the request unchanged", received, err)
	}
	if _, err := io.WriteString(conn, expectationFailedResponse); err != nil {
		t.Fatalf("write response: %v", err)
	}
	response := make([]byte, len(expectationFailedResponse))
	if _, err := io.ReadFull(client, response); err != nil || string(response) != expectationFailedResponse {
		t.Fatalf("client read %q, %v; want the response unchanged", response, err)
	}
	if err := conn.(interface{ CloseWrite() error }).CloseWrite(); err != nil {
		t.Errorf("CloseWrite: %v", err)
	}
	if n, err := client.Read(make([]byte, 1)); n != 0 || err != io.EOF {
		t.Errorf("client read after CloseWrite = %d, %v; want EOF", n, err)
	}
	if got := strings.Count(logs.String(), `"msg":"http_request"`); got != 1 {
		t.Errorf("http_request records = %d, want exactly 1: %s", got, logs.String())
	}
	conn.Close()
	client.Close()

	// The observer is plain per-connection state: no goroutine outlives the
	// connection. Allow the accept goroutine and the runtime a moment to settle.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before {
		t.Errorf("goroutines = %d after closing the connection, want at most %d", after, before)
	}
}

func TestConnContextPairsOnlyObservedConnections(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	plain, other := net.Pipe()
	defer plain.Close()
	defer other.Close()
	if obs := observerFrom(ConnContext(t.Context(), plain)); obs != nil {
		t.Error("an unobserved connection was given an observer")
	}
	observed := &observedConn{Conn: plain, obs: newConnObserver(logger)}
	if obs := observerFrom(ConnContext(t.Context(), observed)); obs != observed.obs {
		t.Error("an observed connection was not paired with its observer")
	}
}

func TestParseDirectErrorResponse(t *testing.T) {
	const headers = "\r\nContent-Type: text/plain; charset=utf-8\r\nConnection: close\r\n\r\n"
	for _, tc := range []struct {
		name       string
		write      string
		wantStatus int
		wantOK     bool
	}{
		{"400", "HTTP/1.1 400 Bad Request" + headers + "400 Bad Request", 400, true},
		{"400 with a detail", "HTTP/1.1 400 Bad Request: missing required Host header" + headers + "400 Bad Request: missing required Host header", 400, true},
		{"431", "HTTP/1.1 431 Request Header Fields Too Large" + headers + "431 Request Header Fields Too Large", 431, true},
		{"501", "HTTP/1.1 501 Not Implemented" + headers + "Unsupported transfer encoding", 501, true},
		{"505 with a detail", "HTTP/1.1 505 HTTP Version Not Supported: unsupported protocol version" + headers, 505, true},
		{"status line only without the headers yet", "HTTP/1.1 400 Bad Request\r\n", 0, false},
		{"a handler response carries Date", "HTTP/1.1 400 Bad Request\r\nDate: Thu, 05 Nov 2026 00:00:00 GMT\r\nContent-Length: 4\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nnope", 0, false},
		{"handler response with the same headers and Date", "HTTP/1.1 400 Bad Request\r\nContent-Type: text/plain; charset=utf-8\r\nConnection: close\r\nDate: Thu, 05 Nov 2026 00:00:00 GMT\r\n\r\n", 0, false},
		{"handler response with Content-Length", "HTTP/1.1 400 Bad Request\r\nContent-Length: 4\r\nContent-Type: text/plain; charset=utf-8\r\nConnection: close\r\n\r\n", 0, false},
		{"other protocol version", "HTTP/1.0 400 Bad Request" + headers, 0, false},
		{"success status", "HTTP/1.1 200 OK" + headers, 0, false},
		{"status text of another code", "HTTP/1.1 400 Not Implemented" + headers, 0, false},
		{"unknown status", "HTTP/1.1 499 " + headers, 0, false},
		{"unterminated detail", "HTTP/1.1 400 Bad Request: detail", 0, false},
		{"body continuation", "xxHTTP/1.1 400 Bad Request" + headers, 0, false},
		{"short write", "HTTP/1.1 4", 0, false},
		{"not a number", "HTTP/1.1 4x0 Bad Request" + headers, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, ok := parseDirectErrorResponse([]byte(tc.write))
			if ok != tc.wantOK || status != tc.wantStatus {
				t.Errorf("parseDirectErrorResponse = %d, %v; want %d, %v", status, ok, tc.wantStatus, tc.wantOK)
			}
		})
	}
}

func TestRejectionReasonFor(t *testing.T) {
	for status, want := range map[int]RejectionReason{
		400: RejectionMalformedRequest,
		431: RejectionHeaderTooLarge,
		501: RejectionUnsupportedTransferEncoding,
		505: RejectionUnsupportedProtocolVersion,
		414: RejectionOther,
		503: RejectionOther,
	} {
		if got := rejectionReasonFor(status); got != want {
			t.Errorf("rejectionReasonFor(%d) = %q, want %q", status, got, want)
		}
	}
}

const directBadRequest = "HTTP/1.1 400 Bad Request\r\nContent-Type: text/plain; charset=utf-8\r\nConnection: close\r\n\r\n400 Bad Request"

func TestClaimDirectRejection(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(o *connObserver)
		write  string
		wantOK bool
	}{
		{"no pending request needed", func(*connObserver) {}, directBadRequest, true},
		{"handler running", func(o *connObserver) { o.inFlight = 1 }, directBadRequest, false},
		{"hijacked", func(o *connObserver) { o.markHijacked() }, directBadRequest, false},
		{"already claimed", func(o *connObserver) { o.rejected = true }, directBadRequest, false},
		{"parser gave up on the head", func(o *connObserver) { o.stop() }, directBadRequest, true},
		{"not a direct error response", func(*connObserver) {}, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, _ := newTestObserver()
			tc.setup(o)
			_, _, _, ok := o.claimDirectRejection([]byte(tc.write))
			if ok != tc.wantOK {
				t.Errorf("claimDirectRejection = %v, want %v", ok, tc.wantOK)
			}
		})
	}

	t.Run("claimed once", func(t *testing.T) {
		o, _ := newTestObserver()
		reason, status, _, ok := o.claimDirectRejection([]byte(directBadRequest))
		if !ok || reason != RejectionMalformedRequest || status != 400 {
			t.Fatalf("first claim = %q, %d, %v", reason, status, ok)
		}
		if _, _, _, ok := o.claimDirectRejection([]byte(directBadRequest)); ok {
			t.Error("second claim succeeded, want exactly one record per connection")
		}
	})
}

func TestClaimReadTimeout(t *testing.T) {
	now := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		setup func(o *connObserver)
		// deadline is relative to the moment it is set.
		deadline time.Duration
		wantOK   bool
	}{
		{"connection that sent nothing", func(*connObserver) {}, time.Second, true},
		{"partial head", func(o *connObserver) { o.feed([]byte("GET / HTTP/1.1\r\nHo")) }, time.Second, true},
		{"idle wait after a request", func(o *connObserver) { o.feed([]byte("GET / HTTP/1.1\r\nHost: t\r\n\r\n")) }, time.Second, false},
		{"partial head after a request", func(o *connObserver) {
			o.feed([]byte("GET / HTTP/1.1\r\nHost: t\r\n\r\nGET / HTTP/1.1\r\nHo"))
		}, time.Second, true},
		{"read interrupted by a deadline in the past", func(*connObserver) {}, -time.Second, false},
		{"no deadline set", func(*connObserver) {}, 0, false},
		{"request body", func(o *connObserver) { o.feed([]byte("POST / HTTP/1.1\r\nHost: t\r\nContent-Length: 9\r\n\r\nab")) }, time.Second, false},
		{"handler running", func(o *connObserver) { o.inFlight = 1 }, time.Second, false},
		{"hijacked", func(o *connObserver) { o.markHijacked() }, time.Second, false},
		{"already claimed", func(o *connObserver) { o.rejected = true }, time.Second, false},
		{"parser stopped", func(o *connObserver) { o.feed([]byte("GARBAGE\r\n\r\n")) }, time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, _ := newTestObserver()
			o.now = func() time.Time { return now }
			tc.setup(o)
			if tc.deadline != 0 {
				o.setReadDeadline(now.Add(tc.deadline))
			}
			if _, ok := o.claimReadTimeout(); ok != tc.wantOK {
				t.Errorf("claimReadTimeout = %v, want %v", ok, tc.wantOK)
			}
		})
	}
}

func TestRejectionStart(t *testing.T) {
	clock := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	o, _ := newTestObserver()
	o.now = func() time.Time { return clock }
	o.acceptedAt = clock
	// Nothing arrived yet: the connection start counts.
	clock = clock.Add(2 * time.Second)
	if _, _, start, ok := o.claimDirectRejection([]byte(directBadRequest)); !ok || !start.Equal(o.acceptedAt) {
		t.Errorf("start = %v, %v; want the accept time %v", start, ok, o.acceptedAt)
	}

	o, _ = newTestObserver()
	o.now = func() time.Time { return clock }
	o.acceptedAt = clock.Add(-time.Hour)
	o.feed([]byte("GET / HTTP/1.1\r\n"))
	first := clock
	clock = clock.Add(time.Second)
	if _, ok := o.claimReadTimeout(); ok {
		t.Fatal("claimed a timeout without a deadline")
	}
	o.setReadDeadline(clock.Add(time.Second))
	if start, ok := o.claimReadTimeout(); !ok || !start.Equal(first) {
		t.Errorf("start = %v, %v; want the first byte %v", start, ok, first)
	}
}

func TestLogRejectedRecordShape(t *testing.T) {
	o, logs := newTestObserver()
	clock := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	o.now = func() time.Time { return clock.Add(1500 * time.Millisecond) }
	o.logRejected(RejectionHeaderReadTimeout, 0, clock)
	var record map[string]any
	if err := json.Unmarshal(logs.Bytes(), &record); err != nil {
		t.Fatalf("log is not JSON: %v: %s", err, logs.String())
	}
	want := map[string]any{"level": "INFO", "msg": "http_request_rejected", "reason": "header_read_timeout", "status": float64(0), "duration_ms": float64(1500)}
	for key, value := range want {
		if record[key] != value {
			t.Errorf("%s = %v, want %v", key, record[key], value)
		}
	}
	for _, key := range []string{"path", "method", "bytes"} {
		if _, ok := record[key]; ok {
			t.Errorf("record has %q, want none", key)
		}
	}
}

func TestConnStateMarksOnlyObservedHijackedConnections(t *testing.T) {
	client, serverSide := net.Pipe()
	defer client.Close()
	defer serverSide.Close()
	observed := &observedConn{Conn: serverSide, obs: newConnObserver(slog.New(slog.NewJSONHandler(io.Discard, nil)))}
	ConnState(observed, http.StateActive)
	if observed.obs.hijacked {
		t.Fatal("an active connection was marked hijacked")
	}
	ConnState(observed, http.StateHijacked)
	if !observed.obs.hijacked {
		t.Error("a hijacked connection was not marked")
	}
	// A connection that ObserveListener did not wrap is left alone.
	ConnState(serverSide, http.StateHijacked)
}
