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
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
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
	post := func(path string) string {
		return "POST " + path + " HTTP/1.1\r\nHost: t\r\nContent-Length: 0\r\n\r\n"
	}
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
			"leading CRLF after a POST is skipped",
			post("/a") + "\r\n" + get("/b"),
			[]pendingSummary{{"POST", "/a", false}, {"GET", "/b", false}},
		},
		{
			"four mixed CR and LF bytes after a POST are skipped",
			post("/a") + "\r\n\r\n" + get("/b") + "\n\r\r\n" + get("/c"),
			[]pendingSummary{{"POST", "/a", false}, {"GET", "/b", false}},
		},
		{
			"each POST allows its own four leading bytes",
			post("/a") + "\r\n\r\n" + post("/b") + "\n\n\n\n" + get("/c"),
			[]pendingSummary{{"POST", "/a", false}, {"POST", "/b", false}, {"GET", "/c", false}},
		},
		{
			"five leading CR and LF bytes after a POST stop parsing",
			post("/a") + "\r\n\r\n\n" + get("/b"),
			[]pendingSummary{{"POST", "/a", false}},
		},
		{
			"leading CRLF after a POST with a body is skipped",
			"POST /a HTTP/1.1\r\nHost: t\r\nContent-Length: 2\r\n\r\nhi\r\n" + get("/b"),
			[]pendingSummary{{"POST", "/a", false}, {"GET", "/b", false}},
		},
		{
			"leading CRLF after a GET stops parsing",
			get("/a") + "\r\n" + get("/b"),
			[]pendingSummary{{"GET", "/a", false}},
		},
		{
			"leading CRLF after a POST then an unsupported expectation is queued",
			post("/a") + "\r\n" + "GET /b HTTP/1.1\r\nHost: t\r\nExpect: a-pony\r\n\r\n" + get("/c"),
			[]pendingSummary{{"POST", "/a", false}, {"GET", "/b", true}},
		},
		{
			"invalid chunk size stops parsing",
			"POST /a HTTP/1.1\r\nHost: t\r\nTransfer-Encoding: chunked\r\n\r\nzz\r\nabc\r\n0\r\n\r\n" + get("/b"),
			[]pendingSummary{{"POST", "/a", false}},
		},
		{
			"bare LF chunk size stops parsing",
			"POST /a HTTP/1.1\r\nHost: t\r\nTransfer-Encoding: chunked\r\n\r\n3\nabc\r\n0\r\n\r\n" + get("/b"),
			[]pendingSummary{{"POST", "/a", false}},
		},
		{
			"CR before the chunk-size terminator stops parsing",
			"POST /a HTTP/1.1\r\nHost: t\r\nTransfer-Encoding: chunked\r\n\r\n3\r\r\nabc\r\n0\r\n\r\n" + get("/b"),
			[]pendingSummary{{"POST", "/a", false}},
		},
		{
			"bare LF last-chunk size stops parsing",
			"POST /a HTTP/1.1\r\nHost: t\r\nTransfer-Encoding: chunked\r\n\r\n3\r\nabc\r\n0\n\r\n" + get("/b"),
			[]pendingSummary{{"POST", "/a", false}},
		},
		{
			"CR inside a chunk size line stops parsing",
			"POST /a HTTP/1.1\r\nHost: t\r\nTransfer-Encoding: chunked\r\n\r\n3\r;x\r\nabc\r\n0\r\n\r\n" + get("/b"),
			[]pendingSummary{{"POST", "/a", false}},
		},
		{
			"chunk size with trailing space and tab is skipped",
			"POST /a HTTP/1.1\r\nHost: t\r\nTransfer-Encoding: chunked\r\n\r\n3 \r\nabc\r\n3\t\r\nabc\r\n0 \t\r\n\r\n" + get("/b"),
			[]pendingSummary{{"POST", "/a", false}, {"GET", "/b", false}},
		},
		{
			"chunk size with extension and trailing whitespace is skipped",
			"POST /a HTTP/1.1\r\nHost: t\r\nTransfer-Encoding: chunked\r\n\r\n3;e=1 \r\nabc\r\n0;e=2\r\n\r\n" + get("/b"),
			[]pendingSummary{{"POST", "/a", false}, {"GET", "/b", false}},
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
		{"handler still running", func(o *connObserver) { o.responding = true }, expectationFailedResponse, false},
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

	t.Run("response of a handler still being written", func(t *testing.T) {
		// net/http flushes the buffered end of a handler's response after the
		// handler returned. A handler controls those bytes, so they are not a
		// response of net/http even when they look like one.
		o, _ := newTestObserver()
		o.feed([]byte("GET /a HTTP/1.1\r\nHost: t\r\n\r\n" + badRequest))
		o.handlerStarted()
		o.handlerFinished()
		if _, _, ok := o.claimRejection([]byte(expectationFailedResponse)); ok {
			t.Fatal("claimed bytes of the handler's response written after the handler returned")
		}
		o.responseFinished()
		if _, _, ok := o.claimRejection([]byte(expectationFailedResponse)); !ok {
			t.Error("did not claim the 417 that net/http writes once the handler's response is complete")
		}
	})

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
		{"handler running", func(o *connObserver) { o.responding = true }, directBadRequest, false},
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

	t.Run("response of a handler still being written", func(t *testing.T) {
		// The buffered end of a handler's response body is written after the
		// handler returned; it must not be taken for a response of net/http.
		o, _ := newTestObserver()
		o.handlerStarted()
		o.handlerFinished()
		if _, _, _, ok := o.claimDirectRejection([]byte(directBadRequest)); ok {
			t.Fatal("claimed bytes of the handler's response written after the handler returned")
		}
		o.responseFinished()
		if _, _, _, ok := o.claimDirectRejection([]byte(directBadRequest)); !ok {
			t.Error("did not claim the response that net/http writes once the handler's response is complete")
		}
	})

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
	const postRequest = "POST / HTTP/1.1\r\nHost: t\r\nContent-Length: 0\r\n\r\n"
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
		{"idle wait after a POST", func(o *connObserver) { o.feed([]byte(postRequest)) }, time.Second, false},
		// The skipped bytes are part of the next head that net/http is reading, so
		// they count as a partly received head although the head buffer is empty.
		{"one skipped CR after a POST", func(o *connObserver) { o.feed([]byte(postRequest + "\r")) }, time.Second, true},
		{"four skipped CR and LF bytes after a POST", func(o *connObserver) { o.feed([]byte(postRequest + "\r\n\r\n")) }, time.Second, true},
		{"skipped bytes after a POST in separate reads", func(o *connObserver) {
			o.feed([]byte(postRequest))
			o.feed([]byte("\r"))
			o.feed([]byte("\n"))
		}, time.Second, true},
		{"idle wait after a head that followed skipped bytes", func(o *connObserver) {
			o.feed([]byte(postRequest + "\r\n" + "GET / HTTP/1.1\r\nHost: t\r\n\r\n"))
		}, time.Second, false},
		{"read interrupted by a deadline in the past", func(*connObserver) {}, -time.Second, false},
		{"no deadline set", func(*connObserver) {}, 0, false},
		{"request body", func(o *connObserver) { o.feed([]byte("POST / HTTP/1.1\r\nHost: t\r\nContent-Length: 9\r\n\r\nab")) }, time.Second, false},
		{"handler running", func(o *connObserver) { o.responding = true }, time.Second, false},
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

// TestRejectionStartOfAnAlreadyBufferedPartialRequest checks that the start of
// a request whose first bytes arrived together with the previous request is not
// moved by a later byte of the same request.
func TestRejectionStartOfAnAlreadyBufferedPartialRequest(t *testing.T) {
	const get = "GET / HTTP/1.1\r\nHost: t\r\n\r\n"
	origin := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		// arrival is what the first read holds; more arrives nine seconds later.
		arrival string
		// handlers is the number of handler calls that finish before that.
		handlers  int
		wantStart time.Duration
	}{
		{"partial head after a complete request", get + "GET / HTTP/1.1\r\nHo", 1, time.Millisecond},
		{"partial head after two pipelined requests", get + get + "GET / HTTP/1.1\r\nHo", 2, 2 * time.Millisecond},
		// Nothing of the next request arrived before the handler finished, so
		// its first byte starts it.
		{"nothing buffered when the handler finishes", get, 1, 9 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := origin
			o, _ := newTestObserver()
			o.now = func() time.Time { return clock }
			o.acceptedAt = origin.Add(-time.Hour)
			o.feed([]byte(tc.arrival))
			for range tc.handlers {
				clock = clock.Add(time.Millisecond)
				o.handlerStarted()
				o.handlerFinished()
			}
			o.responseFinished()
			clock = origin.Add(9 * time.Second)
			o.feed([]byte("s"))
			o.setReadDeadline(clock.Add(time.Second))
			clock = clock.Add(time.Second)
			start, ok := o.claimReadTimeout()
			if !ok || !start.Equal(origin.Add(tc.wantStart)) {
				t.Errorf("start = %v, %v; want %v after the first byte", start, ok, tc.wantStart)
			}
		})
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

func TestConnStateEndsTheResponseOnIdle(t *testing.T) {
	client, serverSide := net.Pipe()
	defer client.Close()
	defer serverSide.Close()
	observed := &observedConn{Conn: serverSide, obs: newConnObserver(slog.New(slog.NewJSONHandler(io.Discard, nil)))}
	observed.obs.handlerStarted()
	observed.obs.handlerFinished()
	ConnState(observed, http.StateActive)
	if !observed.obs.responding {
		t.Fatal("the response ended before net/http reported the connection idle")
	}
	ConnState(observed, http.StateIdle)
	if observed.obs.responding {
		t.Error("the response did not end when net/http reported the connection idle")
	}
	// A connection that ObserveListener did not wrap is left alone.
	ConnState(serverSide, http.StateIdle)
}

// lockedBuffer is a log destination that the server goroutines and the test
// read and write concurrently.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// serveObserved serves handler behind the request logger on a real net/http
// server whose listener is observed, configured like newServer in
// cmd/accounts-api, and returns its address and logs.
func serveObserved(t *testing.T, handler http.HandlerFunc) (string, *lockedBuffer) {
	t.Helper()
	return serveObservedWith(t, handler, nil)
}

// serveObservedWith is serveObserved with a hook that adjusts the server, for
// example to set the timeouts, before it starts serving.
func serveObservedWith(t *testing.T, handler http.HandlerFunc, configure func(*http.Server)) (string, *lockedBuffer) {
	t.Helper()
	logs := &lockedBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, nil))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{
		Handler:        logRequests(logger, handler),
		ConnContext:    ConnContext,
		ConnState:      ConnState,
		MaxHeaderBytes: MaxHeaderBytes,
	}
	if configure != nil {
		configure(srv)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(ObserveListener(listener, logger))
	}()
	t.Cleanup(func() {
		_ = srv.Close()
		<-done
	})
	return listener.Addr().String(), logs
}

func countRecords(logs *lockedBuffer, msg string) int {
	return strings.Count(logs.String(), `"msg":"`+msg+`"`)
}

// TestHandlerResponseBytesAreNeverARejection writes, as the body of a handler's
// own response, the bytes that net/http writes for a rejection. net/http
// flushes the buffered end of that body after the handler returned, with the
// connection already past the handler call, so it must not be logged as a
// rejection next to the request's own http_request record.
func TestHandlerResponseBytesAreNeverARejection(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		// request is sent after the first one in the same write when non-empty.
		pipelined string
	}{
		{"mimics a 400", directBadRequest, ""},
		{"mimics a 417 for a pipelined request", expectationFailedResponse, "GET /bad HTTP/1.1\r\nHost: t\r\nExpect: a-pony\r\n\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr, logs := serveObserved(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/probe" {
					return
				}
				w.Header().Set("Content-Length", strconv.Itoa(len(tc.body)))
				w.WriteHeader(http.StatusOK)
				// Send the headers on their own, so that the body is the first
				// bytes of the final write that net/http makes after the handler.
				if err := http.NewResponseController(w).Flush(); err != nil {
					t.Errorf("flush: %v", err)
				}
				_, _ = io.WriteString(w, tc.body)
			})
			conn, err := net.Dial("tcp", addr)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			request := "GET /probe HTTP/1.1\r\nHost: t\r\n"
			if tc.pipelined == "" {
				request += "Connection: close\r\n"
			}
			if _, err := io.WriteString(conn, request+"\r\n"+tc.pipelined); err != nil {
				t.Fatalf("write: %v", err)
			}
			response, _ := io.ReadAll(conn)
			if !bytes.Contains(response, []byte(tc.body)) {
				t.Fatalf("response = %q, want the handler's body", response)
			}
			if tc.pipelined != "" {
				// The pipelined request is the 417 that net/http answers itself, and
				// it is logged once, by the observer, after the handler's response.
				if got := countRecords(logs, "http_request"); got != 2 {
					t.Errorf("http_request records = %d, want 2: %s", got, logs.String())
				}
			} else if got := countRecords(logs, "http_request"); got != 1 {
				t.Errorf("http_request records = %d, want 1: %s", got, logs.String())
			}
			if got := countRecords(logs, "http_request_rejected"); got != 0 {
				t.Errorf("http_request_rejected records = %d, want none: %s", got, logs.String())
			}
		})
	}
}

// TestLeadingCRLFAfterAPostKeepsTheConnectionObserved sends raw bytes to a real
// net/http server. net/http discards up to four leading CR or LF bytes after a
// POST, so it answers the request that follows (here a 417) and the observer
// must still log that 417; with five bytes or after another method net/http
// answers 400, which is an http_request_rejected record.
func TestLeadingCRLFAfterAPostKeepsTheConnectionObserved(t *testing.T) {
	const (
		post   = "POST /post HTTP/1.1\r\nHost: t\r\nContent-Length: 0\r\n\r\n"
		get    = "GET /get HTTP/1.1\r\nHost: t\r\n\r\n"
		expect = "GET /missing HTTP/1.1\r\nHost: t\r\nExpect: a-pony\r\n\r\n"
	)
	for _, tc := range []struct {
		name string
		// writes are sent one by one over one connection.
		writes []string
		// statuses are the response statuses the client must read, in order.
		statuses []string
		// records are the substrings that the http_request records contain in
		// order; rejected is the number of http_request_rejected records.
		records  []string
		rejected int
	}{
		{
			name:     "POST, CRLF, unsupported expectation",
			writes:   []string{post, "\r\n", expect},
			statuses: []string{"200", "417"},
			records:  []string{`"path":"/post","status":200`, `"path":"/missing","status":417`},
		},
		{
			name:     "POST, CRLF, normal request",
			writes:   []string{post, "\r\n", get, get},
			statuses: []string{"200", "200", "200"},
			records:  []string{`"path":"/post"`, `"path":"/get"`, `"path":"/get"`},
		},
		{
			name:     "POST, four bytes, unsupported expectation",
			writes:   []string{post, "\r\n\r\n", expect},
			statuses: []string{"200", "417"},
			records:  []string{`"path":"/post"`, `"path":"/missing","status":417`},
		},
		{
			name:     "POST, five bytes",
			writes:   []string{post, "\r\n\r\n\n", expect},
			statuses: []string{"200", "400"},
			records:  []string{`"path":"/post"`},
			rejected: 1,
		},
		{
			name:     "CRLF as the first bytes",
			writes:   []string{"\r\n", expect},
			statuses: []string{"400"},
			rejected: 1,
		},
		{
			name:     "GET, CRLF",
			writes:   []string{get, "\r\n", expect},
			statuses: []string{"200", "400"},
			records:  []string{`"path":"/get"`},
			rejected: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr, logs := serveObserved(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, "ok")
			})
			conn, err := net.Dial("tcp", addr)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			for _, w := range tc.writes {
				if _, err := io.WriteString(conn, w); err != nil {
					t.Fatalf("write: %v", err)
				}
				// Separate reads, so the parsing cannot depend on the writes
				// arriving together.
				time.Sleep(30 * time.Millisecond)
			}
			// Every case ends with a response that closes the connection, except
			// the keep-alive one, which the client ends itself.
			if tc.name == "POST, CRLF, normal request" {
				time.Sleep(100 * time.Millisecond)
				_ = conn.(*net.TCPConn).CloseWrite()
			}
			response, _ := io.ReadAll(conn)
			// A handler body has no line end, so find the status lines anywhere.
			var got []string
			for _, m := range regexp.MustCompile(`HTTP/1\.1 (\d{3}) `).FindAllStringSubmatch(string(response), -1) {
				got = append(got, m[1])
			}
			if !reflect.DeepEqual(got, tc.statuses) {
				t.Errorf("response statuses = %v, want %v: %q", got, tc.statuses, response)
			}
			// The records are written before the responses, but the logger of the
			// last request may still be finishing when the client sees the close.
			deadline := time.Now().Add(2 * time.Second)
			for countRecords(logs, "http_request") < len(tc.records) && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			var lines []string
			for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
				if strings.Contains(line, `"msg":"http_request"`) {
					lines = append(lines, line)
				}
			}
			if len(lines) != len(tc.records) {
				t.Fatalf("http_request records = %d, want %d: %s", len(lines), len(tc.records), logs.String())
			}
			for i, want := range tc.records {
				if !strings.Contains(lines[i], want) {
					t.Errorf("record %d = %s, want it to contain %s", i, lines[i], want)
				}
			}
			if got := countRecords(logs, "http_request_rejected"); got != tc.rejected {
				t.Errorf("http_request_rejected records = %d, want %d: %s", got, tc.rejected, logs.String())
			}
		})
	}
}

// TestLeadingCRLFAfterAPostThenStallIsAHeaderReadTimeout stalls a real net/http
// server after a POST and four leading CR and LF bytes. net/http waits for four
// bytes of the next request under IdleTimeout, discards them and then reads the
// head under ReadHeaderTimeout, so the stall is a header read timeout although
// the observer's head buffer is empty (the bytes were skipped). The same stall
// without the extra bytes is the keep-alive wait and is not logged.
func TestLeadingCRLFAfterAPostThenStallIsAHeaderReadTimeout(t *testing.T) {
	const (
		headerTimeout = 200 * time.Millisecond
		post          = "POST /post HTTP/1.1\r\nHost: t\r\nContent-Length: 0\r\n\r\n"
	)
	for _, tc := range []struct {
		name string
		// idleTimeout is long enough that only ReadHeaderTimeout can expire in time
		// when net/http reads a head, and short enough for the idle case to expire.
		idleTimeout time.Duration
		writes      []string
		wantLogged  bool
	}{
		{"POST, four leading bytes, stall", time.Hour, []string{post, "\r\n\r\n"}, true},
		{"POST, stall", 3 * headerTimeout, []string{post}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr, logs := serveObservedWith(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, "ok")
			}, func(srv *http.Server) {
				srv.ReadHeaderTimeout = headerTimeout
				srv.IdleTimeout = tc.idleTimeout
			})
			conn, err := net.Dial("tcp", addr)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			for _, w := range tc.writes {
				if _, err := io.WriteString(conn, w); err != nil {
					t.Fatalf("write: %v", err)
				}
				// Separate reads, so the parsing cannot depend on the writes
				// arriving together.
				time.Sleep(30 * time.Millisecond)
			}
			// net/http closes the connection without a response once the timeout
			// expires, which the client sees as the end of the stream after the
			// response to the POST.
			response, _ := io.ReadAll(conn)
			if got := strings.Count(string(response), "HTTP/1.1 "); got != 1 {
				t.Fatalf("responses = %d, want only the one to the POST: %q", got, response)
			}
			// The timed-out read is logged before net/http closes the connection;
			// allow the logger a moment.
			deadline := time.Now().Add(2 * time.Second)
			for countRecords(logs, "http_request_rejected") == 0 && time.Now().Before(deadline) && tc.wantLogged {
				time.Sleep(10 * time.Millisecond)
			}
			got := countRecords(logs, "http_request_rejected")
			if tc.wantLogged {
				if got != 1 || !strings.Contains(logs.String(), `"reason":"header_read_timeout"`) {
					t.Errorf("want one header_read_timeout record, got %d: %s", got, logs.String())
				}
			} else if got != 0 {
				t.Errorf("http_request_rejected records = %d, want none for the keep-alive wait: %s", got, logs.String())
			}
		})
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
