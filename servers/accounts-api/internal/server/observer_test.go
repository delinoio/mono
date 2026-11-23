// SPDX-License-Identifier: FSL-1.1-ALv2

package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"math/rand"
	"net"
	"net/http"
	"reflect"
	"regexp"
	"runtime"
	"slices"
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
			"connection close flag alone does not end the stream",
			"GET /a HTTP/1.1\r\nHost: t\r\nConnection: close\r\n\r\n" + get("/b"),
			[]pendingSummary{{"GET", "/a", false}, {"GET", "/b", false}},
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

// headOfSize returns a request head of exactly size bytes: one long X-Fill
// header, the shape that net/http accounts for byte by byte.
func headOfSize(path string, size int, extra string) string {
	prefix := "GET " + path + " HTTP/1.1\r\nHost: t\r\n" + extra + "X-Fill: "
	suffix := "\r\n\r\n"
	return prefix + strings.Repeat("a", size-len(prefix)-len(suffix)) + suffix
}

func TestObserverBoundsFollowUpHeadSize(t *testing.T) {
	const (
		first     = "GET /first HTTP/1.1\r\nHost: t\r\n\r\n"
		followUp  = "/follow"
		wantLimit = maxObservedFollowUpHeadBytes
	)
	t.Run("a follow-up head of exactly the limit is observed", func(t *testing.T) {
		o, _ := newTestObserver()
		o.feed([]byte(first))
		o.feed([]byte(headOfSize(followUp, wantLimit, "")))
		if got := summarize(o); len(got) != 2 || got[1].path != followUp {
			t.Errorf("pending = %v, want the follow-up head of exactly the limit to be observed after the first request", got)
		}
	})
	t.Run("one byte more stops the observer", func(t *testing.T) {
		o, _ := newTestObserver()
		o.feed([]byte(first))
		o.feed([]byte(headOfSize(followUp, wantLimit+1, "")))
		o.mu.Lock()
		defer o.mu.Unlock()
		if o.phase != phaseStopped || len(o.pending) != 1 {
			t.Errorf("phase = %v, pending = %d, want a stopped observer with only the first request queued", o.phase, len(o.pending))
		}
	})
	t.Run("the first head keeps the lower limit", func(t *testing.T) {
		o, _ := newTestObserver()
		o.feed([]byte(headOfSize("/big", maxObservedHeadBytes+1, "")))
		o.mu.Lock()
		defer o.mu.Unlock()
		if o.phase != phaseStopped {
			t.Errorf("phase = %v, want the first head of one byte over maxObservedHeadBytes to stop the observer", o.phase)
		}
	})
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
	var conn net.Conn
	select {
	case conn = <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the accepted connection")
	}
	if conn == nil {
		t.Fatal("accept failed")
	}
	// Close the accepted socket on every exit path, including a failed
	// assertion above the explicit close below. Closing twice is harmless.
	t.Cleanup(func() { _ = conn.Close() })
	// Bound every read and write so a stalled peer fails the test instead of
	// hanging it.
	ioDeadline := time.Now().Add(5 * time.Second)
	if err := conn.SetDeadline(ioDeadline); err != nil {
		t.Fatalf("set server deadline: %v", err)
	}
	if err := client.SetDeadline(ioDeadline); err != nil {
		t.Fatalf("set client deadline: %v", err)
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

// TestObservedConnStalledReadFailsWithTimeout checks that a read on an observed
// connection whose peer sends nothing fails with a bounded timeout error and
// that the connection is closed afterwards.
func TestObservedConnStalledReadFailsWithTimeout(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
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
	var conn net.Conn
	select {
	case conn = <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the accepted connection")
	}
	if conn == nil {
		t.Fatal("accept failed")
	}
	t.Cleanup(func() { _ = conn.Close() })

	if err := conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	start := time.Now()
	n, err := conn.Read(make([]byte, 1))
	var netErr net.Error
	if n != 0 || !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("stalled read = %d, %v; want a timeout error", n, err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("stalled read took %v, want it bounded by the deadline", elapsed)
	}

	if err := conn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// The peer sees the close as EOF, which shows the socket was released.
	if err := client.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set client read deadline: %v", err)
	}
	if n, err := client.Read(make([]byte, 1)); n != 0 || err != io.EOF {
		t.Errorf("client read after close = %d, %v; want EOF", n, err)
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
		{"partial head at a line boundary", func(o *connObserver) { o.feed([]byte("GET / HTTP/1.1\r\nHost: t\r\n")) }, time.Second, true},
		// A partial line is not claimed yet: net/http answers it with a 400 or
		// closes without a response (see TestPartialLineHeaderTimeoutWaitsForNetHTTP).
		{"partial head inside a line", func(o *connObserver) { o.feed([]byte("GET / HTTP/1.1\r\nHo")) }, time.Second, false},
		{"idle wait after a request", func(o *connObserver) { o.feed([]byte("GET / HTTP/1.1\r\nHost: t\r\n\r\n")) }, time.Second, false},
		{"partial head after a request, at a line boundary", func(o *connObserver) {
			o.feed([]byte("GET / HTTP/1.1\r\nHost: t\r\n\r\nGET / HTTP/1.1\r\nHost: t\r\n"))
		}, time.Second, true},
		{"partial head after a request, inside a line", func(o *connObserver) {
			o.feed([]byte("GET / HTTP/1.1\r\nHost: t\r\n\r\nGET / HTTP/1.1\r\nHo"))
		}, time.Second, false},
		{"idle wait after a POST", func(o *connObserver) { o.feed([]byte(postRequest)) }, time.Second, false},
		// net/http waits for four bytes of the next request under IdleTimeout and
		// reads the head under ReadHeaderTimeout only afterwards. The skipped
		// bytes are received bytes of that request although the head buffer is
		// empty, so four of them count and fewer do not.
		{"one skipped CR after a POST", func(o *connObserver) { o.feed([]byte(postRequest + "\r")) }, time.Second, false},
		{"three skipped bytes after a POST", func(o *connObserver) { o.feed([]byte(postRequest + "\r\n\r")) }, time.Second, false},
		{"four skipped CR and LF bytes after a POST", func(o *connObserver) { o.feed([]byte(postRequest + "\r\n\r\n")) }, time.Second, true},
		{"skipped bytes after a POST in separate reads", func(o *connObserver) {
			o.feed([]byte(postRequest))
			o.feed([]byte("\r"))
			o.feed([]byte("\n"))
		}, time.Second, false},
		{"four skipped bytes after a POST in separate reads", func(o *connObserver) {
			o.feed([]byte(postRequest))
			o.feed([]byte("\r"))
			o.feed([]byte("\n"))
			o.feed([]byte("\r\n"))
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

// TestClaimReadTimeoutFourByteRule checks the number of bytes of the next
// request after which a stall is a header read timeout rather than the
// keep-alive wait. A partial line is left pending instead of claimed, so the
// check looks at timeoutPending.
func TestClaimReadTimeoutFourByteRule(t *testing.T) {
	const get = "GET / HTTP/1.1\r\nHost: t\r\n\r\n"
	now := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name        string
		feed        string
		wantPending bool
	}{
		{"one byte after a request", get + "G", false},
		{"three bytes after a request", get + "GET", false},
		{"four bytes after a request", get + "GET ", true},
		{"more than four bytes after a request", get + "GET /he", true},
		{"one byte of the first request", "G", true},
		{"three bytes of the first request", "GET", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, _ := newTestObserver()
			o.now = func() time.Time { return now }
			o.feed([]byte(tc.feed))
			o.setReadDeadline(now.Add(time.Second))
			if _, ok := o.claimReadTimeout(); ok {
				t.Fatal("claimed a partial line at once, want it pending")
			}
			if o.timeoutPending != tc.wantPending {
				t.Errorf("timeoutPending = %v, want %v", o.timeoutPending, tc.wantPending)
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
		// arrival is what the first read holds; more arrives nine seconds later and
		// ends at a line boundary, so that the stall is claimed at once.
		arrival string
		more    string
		// handlers is the number of handler calls that finish before that.
		handlers  int
		wantStart time.Duration
	}{
		{"partial head after a complete request", get + "GET / HTTP/1.1\r\nHo", "st: t\r\n", 1, time.Millisecond},
		{"partial head after two pipelined requests", get + get + "GET / HTTP/1.1\r\nHo", "st: t\r\n", 2, 2 * time.Millisecond},
		// Nothing of the next request arrived before the handler finished, so
		// its first byte starts it.
		{"nothing buffered when the handler finishes", get, "GET / HTTP/1.1\r\n", 1, 9 * time.Second},
		// Leading CR and LF bytes after a POST are skipped, so they are in
		// neither the head buffer nor the pending requests, but they are the first
		// bytes of the next request.
		{"leading bytes skipped before the handler finished", "POST / HTTP/1.1\r\nHost: t\r\nContent-Length: 0\r\n\r\n\r\n", "GET / HTTP/1.1\r\n", 1, time.Millisecond},
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
			o.feed([]byte(tc.more))
			o.setReadDeadline(clock.Add(time.Second))
			clock = clock.Add(time.Second)
			start, ok := o.claimReadTimeout()
			if !ok || !start.Equal(origin.Add(tc.wantStart)) {
				t.Errorf("start = %v, %v; want %v after the first byte", start, ok, tc.wantStart)
			}
		})
	}
}

// TestLeadingCRLFDuringTheHandlerKeepsTheRejectionStart checks the start of a
// rejected request after leading CR and LF bytes that net/http skips after a
// POST. The bytes belong to the next request, so when they arrive while the
// handler still runs, the request starts when the handler ends, and a later
// byte must not move it. Skipped bytes that arrive after the handler, or none,
// leave the start unchanged.
func TestLeadingCRLFDuringTheHandlerKeepsTheRejectionStart(t *testing.T) {
	const post = "POST / HTTP/1.1\r\nHost: t\r\nContent-Length: 0\r\n\r\n"
	origin := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		// skippedAt is when "\r\n" arrives; a negative value means it never does.
		skippedAt time.Duration
		// wantStart is relative to origin.
		wantStart time.Duration
	}{
		{"skipped bytes during the handler", 5 * time.Millisecond, 10 * time.Millisecond},
		{"skipped bytes after the handler", 200 * time.Millisecond, 200 * time.Millisecond},
		{"no skipped bytes", -1, 500 * time.Millisecond},
	} {
		// run feeds the POST at origin, finishes its handler at origin+10ms, and
		// sends the later bytes at origin+500ms, then claims with claim.
		run := func(t *testing.T, claim func(o *connObserver, clock *time.Time) (time.Time, bool), later string) {
			clock := origin
			o, _ := newTestObserver()
			o.now = func() time.Time { return clock }
			o.acceptedAt = origin.Add(-time.Hour)
			o.feed([]byte(post))
			skipped := tc.skippedAt >= 0
			if skipped && tc.skippedAt < 10*time.Millisecond {
				clock = origin.Add(tc.skippedAt)
				o.feed([]byte("\r\n"))
			}
			clock = origin.Add(10 * time.Millisecond)
			o.handlerStarted()
			o.handlerFinished()
			o.responseFinished()
			if skipped && tc.skippedAt >= 10*time.Millisecond {
				clock = origin.Add(tc.skippedAt)
				o.feed([]byte("\r\n"))
			}
			clock = origin.Add(500 * time.Millisecond)
			o.feed([]byte(later))
			start, ok := claim(o, &clock)
			if !ok || !start.Equal(origin.Add(tc.wantStart)) {
				t.Errorf("start = %v, %v; want %v after the origin", start, ok, tc.wantStart)
			}
		}
		t.Run(tc.name+", rejected with a 400", func(t *testing.T) {
			run(t, func(o *connObserver, _ *time.Time) (time.Time, bool) {
				reason, status, start, ok := o.claimDirectRejection([]byte(directBadRequest))
				return start, ok && reason == RejectionMalformedRequest && status == http.StatusBadRequest
			}, "GARBAGE\r\n\r\n")
		})
		t.Run(tc.name+", header read timeout", func(t *testing.T) {
			run(t, func(o *connObserver, clock *time.Time) (time.Time, bool) {
				o.setReadDeadline(clock.Add(time.Second))
				*clock = clock.Add(time.Second)
				return o.claimReadTimeout()
			}, "GET / HTTP/1.1\r\n")
		})
	}
}

// TestPartialLineHeaderTimeoutWaitsForNetHTTP checks the header timeout inside
// a partial line. net/http answers such a line with a 400 or closes the
// connection without a response, and the observer learns which only from the
// write or from the close, so the timeout is not claimed when the read fails.
func TestPartialLineHeaderTimeoutWaitsForNetHTTP(t *testing.T) {
	origin := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	// pendingObserver returns an observer whose connection received a partial
	// line at origin, with a read deadline that expired at origin+1s.
	pendingObserver := func(t *testing.T) (*connObserver, *time.Time) {
		t.Helper()
		clock := origin
		o, _ := newTestObserver()
		o.now = func() time.Time { return clock }
		o.acceptedAt = origin.Add(-time.Hour)
		o.feed([]byte("GET /hea"))
		o.setReadDeadline(origin.Add(time.Second))
		clock = origin.Add(2 * time.Second)
		if _, ok := o.claimReadTimeout(); ok {
			t.Fatal("claimed a header timeout inside a partial line before net/http answered")
		}
		if !o.timeoutPending || !o.timeoutPendingStart.Equal(origin) {
			t.Fatalf("timeoutPending = %v, start = %v; want a pending timeout started at the first byte", o.timeoutPending, o.timeoutPendingStart)
		}
		if o.rejected {
			t.Fatal("marked the connection rejected while the timeout is pending")
		}
		return o, &clock
	}

	t.Run("net/http answers with a 400", func(t *testing.T) {
		o, _ := pendingObserver(t)
		reason, status, start, ok := o.claimDirectRejection([]byte(directBadRequest))
		if !ok || reason != RejectionMalformedRequest || status != http.StatusBadRequest || !start.Equal(origin) {
			t.Fatalf("claimDirectRejection = %q, %d, %v, %v; want a malformed_request 400 started at the first byte", reason, status, start, ok)
		}
		if o.timeoutPending {
			t.Error("the timeout is still pending after the 400 claimed the rejection")
		}
		if _, ok := o.claimPendingTimeout(); ok {
			t.Error("the close claimed a second record for the connection")
		}
	})

	t.Run("net/http closes without a response", func(t *testing.T) {
		o, _ := pendingObserver(t)
		start, ok := o.claimPendingTimeout()
		if !ok || !start.Equal(origin) {
			t.Fatalf("claimPendingTimeout = %v, %v; want the first byte %v", start, ok, origin)
		}
		if _, ok := o.claimPendingTimeout(); ok {
			t.Error("claimed the pending timeout twice")
		}
		if _, _, _, ok := o.claimDirectRejection([]byte(directBadRequest)); ok {
			t.Error("claimed a rejection after the timeout record was written")
		}
	})

	t.Run("a hijacked connection logs nothing", func(t *testing.T) {
		o, _ := pendingObserver(t)
		o.markHijacked()
		if _, ok := o.claimPendingTimeout(); ok {
			t.Error("claimed a pending timeout on a hijacked connection")
		}
	})

	t.Run("a later empty read timeout keeps the first start", func(t *testing.T) {
		o, clock := pendingObserver(t)
		*clock = origin.Add(3 * time.Second)
		if _, ok := o.claimReadTimeout(); ok {
			t.Fatal("claimed a second timeout")
		}
		if start, ok := o.claimPendingTimeout(); !ok || !start.Equal(origin) {
			t.Errorf("claimPendingTimeout = %v, %v; want %v", start, ok, origin)
		}
	})

	t.Run("nothing pending", func(t *testing.T) {
		o, _ := newTestObserver()
		if _, ok := o.claimPendingTimeout(); ok {
			t.Error("claimed a timeout that never happened")
		}
	})
}

// TestConnStateLogsAPendingHeaderTimeoutWhenTheConnectionCloses drives the
// ConnState hook of a wrapped connection: the pending timeout is logged once,
// with status 0 and the start of the request, when net/http reports the
// connection closed without a 400.
func TestConnStateLogsAPendingHeaderTimeoutWhenTheConnectionCloses(t *testing.T) {
	origin := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	clock := origin
	client, serverSide := net.Pipe()
	defer client.Close()
	defer serverSide.Close()
	var logs bytes.Buffer
	observed := &observedConn{Conn: serverSide, obs: newConnObserver(slog.New(slog.NewJSONHandler(&logs, nil)))}
	observed.obs.now = func() time.Time { return clock }
	observed.obs.acceptedAt = origin.Add(-time.Hour)
	observed.obs.feed([]byte("GET /hea"))
	observed.obs.setReadDeadline(origin.Add(time.Second))
	clock = origin.Add(2 * time.Second)
	if _, ok := observed.obs.claimReadTimeout(); ok {
		t.Fatal("claimed a header timeout inside a partial line")
	}

	ConnState(observed, http.StateClosed)
	ConnState(observed, http.StateClosed)
	var record map[string]any
	if lines := strings.Split(strings.TrimSpace(logs.String()), "\n"); len(lines) != 1 {
		t.Fatalf("got %d log lines, want exactly 1: %s", len(lines), logs.String())
	} else if err := json.Unmarshal([]byte(lines[0]), &record); err != nil {
		t.Fatalf("log is not JSON: %v: %s", err, logs.String())
	}
	want := map[string]any{"msg": "http_request_rejected", "reason": "header_read_timeout", "status": float64(0), "duration_ms": float64(2000)}
	for key, value := range want {
		if record[key] != value {
			t.Errorf("%s = %v, want %v", key, record[key], value)
		}
	}

	// A connection that ObserveListener did not wrap is left alone.
	ConnState(serverSide, http.StateClosed)
}

// TestRejectionStartIgnoresDrainedBodyBytes checks that the body of a request
// whose handler returned without reading it does not start the next request.
// net/http drains that body afterwards, and the drained bytes reach the
// observer after the handler finished, so a start stamped from them would lie
// between the end of the handler and the first byte of the rejected request.
func TestRejectionStartIgnoresDrainedBodyBytes(t *testing.T) {
	const head = "POST /a HTTP/1.1\r\nHost: t\r\n"
	origin := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		// request is the head of the POST; body is what arrives 90 ms after the
		// handler finished.
		request string
		body    string
	}{
		{"content length", head + "Content-Length: 5\r\n\r\n", "hello"},
		{"chunked with a trailer", head + "Transfer-Encoding: chunked\r\n\r\n", "5\r\nhello\r\n0\r\nX-T: 1\r\n\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := origin
			o, _ := newTestObserver()
			o.now = func() time.Time { return clock }
			o.acceptedAt = origin.Add(-time.Hour)
			o.feed([]byte(tc.request))
			clock = origin.Add(10 * time.Millisecond)
			o.handlerStarted()
			o.handlerFinished()
			o.responseFinished()
			clock = origin.Add(100 * time.Millisecond)
			o.feed([]byte(tc.body))
			clock = origin.Add(500 * time.Millisecond)
			o.feed([]byte("GARBAGE\r\n\r\n"))
			clock = origin.Add(501 * time.Millisecond)
			_, _, start, ok := o.claimDirectRejection([]byte(directBadRequest))
			if !ok {
				t.Fatal("did not claim the 400")
			}
			if want := origin.Add(500 * time.Millisecond); !start.Equal(want) {
				t.Errorf("start = %v after the origin, want %v: the drained body must not move it", start.Sub(origin), want.Sub(origin))
			}
		})
	}

	t.Run("a stalled partial head after the drained body", func(t *testing.T) {
		// The start of a partial head that arrives after the drained body is its
		// own first byte, and the timeout is measured from there.
		clock := origin
		o, _ := newTestObserver()
		o.now = func() time.Time { return clock }
		o.acceptedAt = origin.Add(-time.Hour)
		o.feed([]byte(head + "Content-Length: 5\r\n\r\n"))
		clock = origin.Add(10 * time.Millisecond)
		o.handlerStarted()
		o.handlerFinished()
		o.responseFinished()
		clock = origin.Add(100 * time.Millisecond)
		o.feed([]byte("hello"))
		clock = origin.Add(500 * time.Millisecond)
		o.feed([]byte("GET /next HTTP/1.1\r\nHost: t\r\n"))
		o.setReadDeadline(origin.Add(time.Second))
		clock = origin.Add(2 * time.Second)
		start, ok := o.claimReadTimeout()
		if !ok || !start.Equal(origin.Add(500*time.Millisecond)) {
			t.Errorf("start = %v, %v; want the first byte of the partial head at 500ms", start.Sub(origin), ok)
		}
	})

	t.Run("a body without a rejected request after it", func(t *testing.T) {
		// With only the drained body after the handler, nothing started the next
		// request, so the start falls back to the end of the last handler call.
		clock := origin
		o, _ := newTestObserver()
		o.now = func() time.Time { return clock }
		o.acceptedAt = origin.Add(-time.Hour)
		o.feed([]byte(head + "Content-Length: 5\r\n\r\n"))
		clock = origin.Add(10 * time.Millisecond)
		o.handlerStarted()
		o.handlerFinished()
		o.responseFinished()
		clock = origin.Add(100 * time.Millisecond)
		o.feed([]byte("hello"))
		_, _, start, ok := o.claimDirectRejection([]byte(directBadRequest))
		if want := origin.Add(10 * time.Millisecond); !ok || !start.Equal(want) {
			t.Errorf("start = %v, %v; want the end of the handler at %v", start.Sub(origin), ok, want.Sub(origin))
		}
	})
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
	if got, want := slices.Sorted(maps.Keys(record)), []string{"duration_ms", "level", "msg", "reason", "status", "time"}; !slices.Equal(got, want) {
		t.Errorf("record keys = %v, want %v: %v", got, want, record)
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
		// Mirrors newServer: without it net/http answers "OPTIONS *" itself,
		// which the observer cannot pair with a handler call.
		DisableGeneralOptionsHandler: true,
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
			response := readUntilServerClose(t, conn)
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

// requestRecord is the part of an http_request record that the 417 tests
// compare.
type requestRecord struct {
	Path   string `json:"path"`
	Status int    `json:"status"`
	Bytes  int64  `json:"bytes"`
}

// requestRecords returns the http_request records in logs, in log order.
func requestRecords(t *testing.T, logs *lockedBuffer) []requestRecord {
	t.Helper()
	var records []requestRecord
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		var entry struct {
			Msg string `json:"msg"`
			requestRecord
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("log line %q is not JSON: %v", line, err)
		}
		if entry.Msg == "http_request" {
			records = append(records, entry.requestRecord)
		}
	}
	return records
}

// TestObservedOptionsStarRequestThenUnsupportedExpect sends a server-wide
// "OPTIONS *" request and then a request with an unsupported Expect header on
// one connection. The observed server sets DisableGeneralOptionsHandler, as
// newServer does, so the handler sees "OPTIONS *" and the observer pairs the
// later 417 with its own request. With the field left false, net/http answers
// "OPTIONS *" itself, the observer keeps its entry queued and the 417 is lost.
func TestObservedOptionsStarRequestThenUnsupportedExpect(t *testing.T) {
	addr, logs := serveObserved(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	const request = "OPTIONS * HTTP/1.1\r\nHost: t\r\n\r\n" +
		"GET /b HTTP/1.1\r\nHost: t\r\nExpect: a-pony\r\n\r\n"
	if _, err := io.WriteString(conn, request); err != nil {
		t.Fatalf("write: %v", err)
	}
	response := readUntilServerClose(t, conn)
	text := string(response)
	i200 := strings.Index(text, "HTTP/1.1 200 ")
	i417 := strings.Index(text, "HTTP/1.1 417 ")
	if i200 < 0 || i417 < i200 || !strings.Contains(text[i417:], "Connection: close") {
		t.Fatalf("response = %q, want 200 and then 417 with Connection: close", text)
	}

	var records []struct {
		Msg    string `json:"msg"`
		Method string `json:"method"`
		Path   string `json:"path"`
		Status int    `json:"status"`
	}
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		var entry struct {
			Msg    string `json:"msg"`
			Method string `json:"method"`
			Path   string `json:"path"`
			Status int    `json:"status"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("log line %q is not JSON: %v", line, err)
		}
		records = append(records, entry)
	}
	if len(records) != 2 {
		t.Fatalf("records = %+v, want exactly two http_request records: %s", records, logs.String())
	}
	for i, want := range []struct {
		method, path string
		status       int
	}{{"OPTIONS", "*", 200}, {"GET", "/b", 417}} {
		got := records[i]
		if got.Msg != "http_request" || got.Method != want.method || got.Path != want.path || got.Status != want.status {
			t.Errorf("record %d = %+v, want http_request %s %s %d", i, got, want.method, want.path, want.status)
		}
	}
}

// TestHandlerExpectationFailedIsNotClaimedAsARejection pipelines a request with
// an unsupported Expect header behind a request whose handler answers 417
// itself. The Expect request is queued as the oldest pending request when the
// handler's response is flushed, so a handler 417 that carries Connection: close
// has the same bytes as the 417 that net/http writes for a rejection. The
// observer must not claim those bytes: net/http never read or answered the
// Expect request when the handler closes the connection.
func TestHandlerExpectationFailedIsNotClaimedAsARejection(t *testing.T) {
	const (
		teapot  = "GET /teapot HTTP/1.1\r\nHost: t\r\n\r\n"
		missing = "GET /missing HTTP/1.1\r\nHost: t\r\nExpect: a-pony\r\n\r\n"
	)
	for _, tc := range []struct {
		name string
		// handlerClose makes the /teapot handler send Connection: close with its 417.
		handlerClose bool
		// request is written to the connection in one write.
		request string
		// statuses is the number of 417 status lines the client must read.
		statuses int
		records  []requestRecord
	}{
		{
			name:         "handler 417 with Connection close logs only the handler's request",
			handlerClose: true,
			request:      teapot + missing,
			statuses:     1,
			records:      []requestRecord{{Path: "/teapot", Status: 417, Bytes: 0}},
		},
		{
			name:         "handler 417 without Connection close still logs the request that net/http rejects",
			handlerClose: false,
			request:      teapot + missing,
			statuses:     2,
			records: []requestRecord{
				{Path: "/teapot", Status: 417, Bytes: 0},
				{Path: "/missing", Status: 417, Bytes: 0},
			},
		},
		{
			name:     "a lone Expect request is logged once",
			request:  missing,
			statuses: 1,
			records:  []requestRecord{{Path: "/missing", Status: 417, Bytes: 0}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr, logs := serveObserved(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/teapot" {
					return
				}
				if tc.handlerClose {
					w.Header().Set("Connection", "close")
				}
				w.WriteHeader(http.StatusExpectationFailed)
			})
			conn, err := net.Dial("tcp", addr)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			if _, err := io.WriteString(conn, tc.request); err != nil {
				t.Fatalf("write: %v", err)
			}
			// net/http closes the connection after a 417 with Connection: close, and
			// after its own 417, so the read ends at EOF.
			response := readUntilServerClose(t, conn)
			if got := bytes.Count(response, []byte("HTTP/1.1 417 Expectation Failed\r\n")); got != tc.statuses {
				t.Errorf("417 status lines = %d, want %d: %q", got, tc.statuses, response)
			}
			// The observer logs a net/http rejection before it writes the response,
			// and the request logger logs after the handler returns, which is before
			// net/http flushes the handler's response. The client has read to EOF, so
			// every record exists; the connection teardown is the only thing left.
			if got := requestRecords(t, logs); !reflect.DeepEqual(got, tc.records) {
				t.Errorf("http_request records = %+v, want %+v: %s", got, tc.records, logs.String())
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
//
// A case sends no bytes after the bytes that net/http answers with a closing
// response. net/http closes the socket right after a 400 without reading the
// rest, so bytes that arrive later make the server's OS reset the connection.
// Windows then discards the response that the client has not read yet, and
// the client reads nothing.
func TestLeadingCRLFAfterAPostKeepsTheConnectionObserved(t *testing.T) {
	const (
		post   = "POST /post HTTP/1.1\r\nHost: t\r\nContent-Length: 0\r\n\r\n"
		get    = "GET /get HTTP/1.1\r\nHost: t\r\n\r\n"
		expect = "GET /missing HTTP/1.1\r\nHost: t\r\nExpect: a-pony\r\n\r\n"
	)
	for _, tc := range []struct {
		name string
		// writes are sent one by one over one connection. The last write
		// completes the request that net/http answers with a closing response.
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
			// net/http discards four bytes and rejects the fifth as an empty
			// request line, before any request that follows.
			name:     "POST, five bytes",
			writes:   []string{post, "\r\n\r\n\n"},
			statuses: []string{"200", "400"},
			records:  []string{`"path":"/post"`},
			rejected: 1,
		},
		{
			// net/http rejects the empty request line at once.
			name:     "CRLF as the first bytes",
			writes:   []string{"\r\n"},
			statuses: []string{"400"},
			rejected: 1,
		},
		{
			// net/http waits for four bytes before it reads the request that
			// follows a keep-alive response, so it reads the CRLF together with
			// the expectation request and then rejects the empty request line.
			// No bytes arrive after the rejection.
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
			response, err := io.ReadAll(conn)
			if err != nil {
				t.Errorf("read: %v after %q", err, response)
			}
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

// readUntilServerClose reads from conn until the server closes the connection
// and fails the test on any other end of the stream. io.ReadAll returns a nil
// error only for io.EOF, so a non-nil error is the connection deadline that the
// tests set, which means that the server held the connection open, or a reset.
// Ignoring it would let a server that never closes pass as a server that
// closes, so the failure carries the bytes that arrived before it.
func readUntilServerClose(t *testing.T, conn net.Conn) []byte {
	t.Helper()
	response, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read until the server closed the connection: %v after %q", err, response)
	}
	return response
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
			response := readUntilServerClose(t, conn)
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

// TestStallWithFewerThanFourHeadBytesIsNotAHeaderReadTimeout stalls a real
// net/http server after a served request. net/http waits for the first four
// bytes of the next request under IdleTimeout and arms ReadHeaderTimeout only
// once it has them, so a stall after one to three bytes (leading CR and LF bytes
// after a POST included) is the keep-alive wait and is not logged, while four
// bytes or more are a header read timeout. The close time shows which timer
// fired: IdleTimeout is short and ReadHeaderTimeout six times longer.
func TestStallWithFewerThanFourHeadBytesIsNotAHeaderReadTimeout(t *testing.T) {
	const (
		idleTimeout   = 200 * time.Millisecond
		headerTimeout = 1200 * time.Millisecond
		get           = "GET /get HTTP/1.1\r\nHost: t\r\n\r\n"
		post          = "POST /post HTTP/1.1\r\nHost: t\r\nContent-Length: 0\r\n\r\n"
	)
	for _, tc := range []struct {
		name string
		// served is the request that is answered first; empty for a fresh
		// connection.
		served string
		// stall is sent after the served request, then the client goes quiet.
		stall string
		// wantReason is the reason of the one record that is logged; empty when
		// the stall is the keep-alive wait and nothing is logged. A partial line
		// that is not a valid start of a line gets the 400 of net/http when the
		// timer expires, which is logged as the malformed request it is.
		wantReason string
		wantStatus int
	}{
		{"GET, one byte", get, "G", "", 0},
		{"GET, two bytes", get, "GE", "", 0},
		{"GET, three bytes", get, "GET", "", 0},
		{"GET, four bytes", get, "GET ", string(RejectionMalformedRequest), 400},
		{"GET, request line at a line boundary", get, "GET / HTTP/1.1\r\n", string(RejectionHeaderReadTimeout), 0},
		{"POST, one leading CR", post, "\r", "", 0},
		{"POST, CR LF", post, "\r\n", "", 0},
		{"POST, CR LF CR", post, "\r\n\r", "", 0},
		{"POST, four leading CR and LF bytes", post, "\r\n\r\n", string(RejectionHeaderReadTimeout), 0},
		{"GET, no next byte", get, "", "", 0},
		{"fresh connection, nothing", "", "", string(RejectionHeaderReadTimeout), 0},
		{"fresh connection, one byte", "", "G", string(RejectionMalformedRequest), 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			addr, logs := serveObservedWith(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, "ok")
			}, func(srv *http.Server) {
				srv.ReadHeaderTimeout = headerTimeout
				srv.IdleTimeout = idleTimeout
			})
			conn, err := net.Dial("tcp", addr)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
			reader := bufio.NewReader(conn)
			if tc.served != "" {
				if _, err := io.WriteString(conn, tc.served); err != nil {
					t.Fatalf("write: %v", err)
				}
				response, err := http.ReadResponse(reader, nil)
				if err != nil {
					t.Fatalf("read response: %v", err)
				}
				_, _ = io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
			}
			stalled := time.Now()
			if tc.stall != "" {
				if _, err := io.WriteString(conn, tc.stall); err != nil {
					t.Fatalf("write: %v", err)
				}
			}
			// net/http closes the connection without a response when the timer
			// expires, which the client sees as the end of the stream.
			rest, _ := io.ReadAll(reader)
			if tc.wantStatus == 0 && len(rest) != 0 {
				t.Fatalf("unexpected bytes after the stall: %q", rest)
			}
			if tc.wantStatus != 0 && !strings.HasPrefix(string(rest), "HTTP/1.1 "+strconv.Itoa(tc.wantStatus)+" ") {
				t.Fatalf("response after the stall = %q, want a %d", rest, tc.wantStatus)
			}
			elapsed := time.Since(stalled)
			// The timed-out read is logged before net/http closes the connection;
			// allow the logger a moment.
			time.Sleep(50 * time.Millisecond)
			got := countRecords(logs, "http_request_rejected")
			if tc.wantReason != "" {
				if elapsed < headerTimeout-200*time.Millisecond {
					t.Errorf("connection closed after %v, want the ReadHeaderTimeout of %v to have fired", elapsed, headerTimeout)
				}
				if got != 1 || !strings.Contains(logs.String(), `"reason":"`+tc.wantReason+`"`) {
					t.Errorf("want one %s record, got %d: %s", tc.wantReason, got, logs.String())
				}
				return
			}
			if elapsed >= headerTimeout-200*time.Millisecond {
				t.Errorf("connection closed after %v, want the IdleTimeout of %v to have fired", elapsed, idleTimeout)
			}
			if got != 0 {
				t.Errorf("http_request_rejected records = %d, want none for the keep-alive wait: %s", got, logs.String())
			}
		})
	}
}

// TestLeadingCRLFDuringTheHandlerKeepsTheRejectedRequestStart sends raw bytes to
// a real net/http server. A leading CR arrives while the POST handler still
// runs, so net/http skips it as the start of the next request. The request that
// follows 700 ms after the handler ended is rejected with a 400. Its logged
// duration starts when the handler ended, not at the late bytes that net/http
// does not skip.
func TestLeadingCRLFDuringTheHandlerKeepsTheRejectedRequestStart(t *testing.T) {
	const post = "POST /slow HTTP/1.1\r\nHost: t\r\nContent-Length: 0\r\n\r\n"
	addr, logs := serveObserved(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = io.WriteString(w, "ok")
	})
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	write := func(s string) {
		t.Helper()
		if _, err := io.WriteString(conn, s); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	write(post)
	time.Sleep(50 * time.Millisecond)
	// One byte per write: net/http reads a single byte in the background while
	// the handler runs.
	write("\r")
	// Wait for the response, so the handler has ended when the next bytes go.
	var response []byte
	buf := make([]byte, 512)
	for !strings.HasSuffix(string(response), "ok") {
		n, err := conn.Read(buf)
		response = append(response, buf[:n]...)
		if err != nil {
			t.Fatalf("read the first response: %v (%q)", err, response)
		}
	}
	time.Sleep(300 * time.Millisecond)
	write("\n")
	time.Sleep(400 * time.Millisecond)
	write("GARBAGE\r\n\r\n")
	rest := readUntilServerClose(t, conn)
	response = append(response, rest...)
	if !strings.Contains(string(response), "HTTP/1.1 200 ") || !strings.Contains(string(response), "HTTP/1.1 400 ") {
		t.Fatalf("responses = %q, want a 200 and then a 400", response)
	}
	deadline := time.Now().Add(2 * time.Second)
	for countRecords(logs, "http_request_rejected") == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := countRecords(logs, "http_request_rejected"); got != 1 {
		t.Fatalf("http_request_rejected records = %d, want 1: %s", got, logs.String())
	}
	var record struct {
		Reason     string  `json:"reason"`
		Status     int     `json:"status"`
		DurationMS float64 `json:"duration_ms"`
	}
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if strings.Contains(line, `"msg":"http_request_rejected"`) {
			if err := json.Unmarshal([]byte(line), &record); err != nil {
				t.Fatalf("decode %s: %v", line, err)
			}
		}
	}
	if record.Reason != string(RejectionMalformedRequest) || record.Status != http.StatusBadRequest {
		t.Errorf("record = %+v, want a malformed_request 400", record)
	}
	// The handler ended about 300 ms after the POST and the rejection came about
	// 700 ms after that. Without the fix the start is the "\n" byte that arrived
	// 300 ms after the handler ended, so the logged duration is about 400 ms.
	if record.DurationMS < 600 {
		t.Errorf("duration_ms = %v, want at least 600 from the end of the handler: %s", record.DurationMS, logs.String())
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

// rejectedRecord is the part of an http_request_rejected record that the
// duration tests compare.
type rejectedRecord struct {
	Reason     string `json:"reason"`
	Status     int    `json:"status"`
	DurationMS int64  `json:"duration_ms"`
}

// rejectedRecords returns the http_request_rejected records in logs, in log
// order.
func rejectedRecords(t *testing.T, logs *lockedBuffer) []rejectedRecord {
	t.Helper()
	var records []rejectedRecord
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		var entry struct {
			Msg string `json:"msg"`
			rejectedRecord
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("log line %q is not JSON: %v", line, err)
		}
		if entry.Msg == "http_request_rejected" {
			records = append(records, entry.rejectedRecord)
		}
	}
	return records
}

// TestDrainedRequestBodyDoesNotMoveRejectionStart sends a request whose handler
// never reads the body, so net/http drains the body when it arrives, and then
// rejects the next request. The logged duration is the time since the first
// byte of the rejected request, not since the drained body.
func TestDrainedRequestBodyDoesNotMoveRejectionStart(t *testing.T) {
	const (
		gap           = 300 * time.Millisecond
		headerTimeout = 400 * time.Millisecond
		postHead      = "POST /post HTTP/1.1\r\nHost: t\r\nContent-Length: 5\r\n\r\n"
	)
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "0")
	}
	dial := func(t *testing.T, addr string) net.Conn {
		t.Helper()
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		return conn
	}
	write := func(t *testing.T, conn net.Conn, s string) {
		t.Helper()
		if _, err := io.WriteString(conn, s); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	waitForRejection := func(t *testing.T, logs *lockedBuffer) []rejectedRecord {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for countRecords(logs, "http_request_rejected") == 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		return rejectedRecords(t, logs)
	}

	t.Run("malformed request", func(t *testing.T) {
		addr, logs := serveObserved(t, handler)
		conn := dial(t, addr)
		write(t, conn, postHead)
		time.Sleep(gap)
		write(t, conn, "hello")
		time.Sleep(gap)
		write(t, conn, "GARBAGE\r\n\r\n")
		_, _ = io.ReadAll(conn)
		records := waitForRejection(t, logs)
		if len(records) != 1 || records[0].Status != http.StatusBadRequest || records[0].Reason != string(RejectionMalformedRequest) {
			t.Fatalf("rejected records = %+v, want one malformed_request 400: %s", records, logs.String())
		}
		if records[0].DurationMS > 50 {
			t.Errorf("duration_ms = %d, want at most 50: the rejected request started when its own bytes arrived, not %v earlier with the drained body", records[0].DurationMS, gap)
		}
		// The answered POST is logged too, so the body was drained on a
		// keep-alive connection.
		if got := countRecords(logs, "http_request"); got != 1 {
			t.Errorf("http_request records = %d, want 1: %s", got, logs.String())
		}
	})

	t.Run("header read timeout", func(t *testing.T) {
		addr, logs := serveObservedWith(t, handler, func(srv *http.Server) {
			srv.ReadHeaderTimeout = headerTimeout
		})
		conn := dial(t, addr)
		write(t, conn, postHead)
		time.Sleep(gap)
		write(t, conn, "hello")
		time.Sleep(gap)
		// The tail ends at a line boundary, so net/http closes the connection
		// without a response when the header timeout expires.
		write(t, conn, "GET /next HTTP/1.1\r\nHost: t\r\n")
		_, _ = io.ReadAll(conn)
		records := waitForRejection(t, logs)
		if len(records) != 1 || records[0].Status != 0 || records[0].Reason != string(RejectionHeaderReadTimeout) {
			t.Fatalf("rejected records = %+v, want one header_read_timeout: %s", records, logs.String())
		}
		// The timeout runs from the first byte of the partial head, and the
		// drained body arrived gap before it.
		if ms := records[0].DurationMS; ms < headerTimeout.Milliseconds()-50 || ms > (headerTimeout+gap/2).Milliseconds() {
			t.Errorf("duration_ms = %d, want about %d: the drained body must not count", ms, headerTimeout.Milliseconds())
		}
	})
}

// TestKeepAliveHeadBeyondTheFirstHeadLimit sends, on a kept-alive connection to
// a real net/http server, a head that is up to 4096 bytes larger than the
// limit for the first head of a connection. net/http does not charge the bytes
// that its keep-alive wait already buffered to that head, so it serves the
// request, and the observer must keep parsing the connection: the request that
// follows is logged, and so is a stall after it.
func TestKeepAliveHeadBeyondTheFirstHeadLimit(t *testing.T) {
	const headerTimeout = 300 * time.Millisecond
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "0")
	}
	dial := func(t *testing.T, addr string) (net.Conn, *bufio.Reader) {
		t.Helper()
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		return conn, bufio.NewReader(conn)
	}
	// roundTrip writes one request in a single write and reads its response.
	roundTrip := func(t *testing.T, conn net.Conn, reader *bufio.Reader, request string) int {
		t.Helper()
		if _, err := io.WriteString(conn, request); err != nil {
			t.Fatalf("write: %v", err)
		}
		resp, err := http.ReadResponse(reader, nil)
		if err != nil {
			t.Fatalf("read response: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	waitFor := func(logs *lockedBuffer, msg string, want int) {
		deadline := time.Now().Add(3 * time.Second)
		for countRecords(logs, msg) < want && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
	}
	const (
		first = "GET /first HTTP/1.1\r\nHost: t\r\n\r\n"
		pony  = "GET /missing HTTP/1.1\r\nHost: t\r\nExpect: a-pony\r\n\r\n"
	)

	for _, over := range []int{1, 904, 4096} {
		size := maxObservedHeadBytes + over
		t.Run("later 417 is logged, head "+strconv.Itoa(over)+" bytes over the first head limit", func(t *testing.T) {
			addr, logs := serveObserved(t, handler)
			conn, reader := dial(t, addr)
			if got := roundTrip(t, conn, reader, first); got != http.StatusOK {
				t.Fatalf("first status = %d, want 200", got)
			}
			if got := roundTrip(t, conn, reader, headOfSize("/big", size, "")); got != http.StatusOK {
				t.Fatalf("big status = %d, want 200 (net/http serves a head of %d bytes here)", got, size)
			}
			if got := roundTrip(t, conn, reader, pony); got != http.StatusExpectationFailed {
				t.Fatalf("pony status = %d, want 417", got)
			}
			waitFor(logs, "http_request", 3)
			if got := strings.Count(logs.String(), `"path":"/missing"`); got != 1 || !strings.Contains(logs.String(), `"status":417`) {
				t.Errorf("want exactly one /missing record with status 417, got %d: %s", got, logs.String())
			}
			if got := countRecords(logs, "http_request"); got != 3 {
				t.Errorf("http_request records = %d, want 3: %s", got, logs.String())
			}
			if got := countRecords(logs, "http_request_rejected"); got != 0 {
				t.Errorf("http_request_rejected records = %d, want none: %s", got, logs.String())
			}
		})
	}

	t.Run("later header read timeout is logged", func(t *testing.T) {
		addr, logs := serveObservedWith(t, handler, func(srv *http.Server) {
			srv.ReadHeaderTimeout = headerTimeout
		})
		conn, reader := dial(t, addr)
		if got := roundTrip(t, conn, reader, first); got != http.StatusOK {
			t.Fatalf("first status = %d, want 200", got)
		}
		if got := roundTrip(t, conn, reader, headOfSize("/big", maxObservedHeadBytes+5000-4096, "")); got != http.StatusOK {
			t.Fatalf("big status = %d, want 200", got)
		}
		if _, err := io.WriteString(conn, "GET /stall HTTP/1.1\r\nHost: t\r\n"); err != nil {
			t.Fatalf("write: %v", err)
		}
		_, _ = io.ReadAll(reader)
		waitFor(logs, "http_request_rejected", 1)
		records := rejectedRecords(t, logs)
		if len(records) != 1 || records[0].Reason != string(RejectionHeaderReadTimeout) || records[0].Status != 0 {
			t.Fatalf("rejected records = %+v, want one header_read_timeout with status 0: %s", records, logs.String())
		}
	})

	t.Run("a head past the follow-up limit is a 431", func(t *testing.T) {
		addr, logs := serveObserved(t, handler)
		conn, reader := dial(t, addr)
		if got := roundTrip(t, conn, reader, first); got != http.StatusOK {
			t.Fatalf("first status = %d, want 200", got)
		}
		// The 431 closes the connection, so the request after the head never
		// reaches net/http and must not be logged as a 417.
		request := headOfSize("/huge", maxObservedFollowUpHeadBytes+1, "") + pony
		if got := roundTrip(t, conn, reader, request); got != http.StatusRequestHeaderFieldsTooLarge {
			t.Fatalf("status = %d, want 431", got)
		}
		waitFor(logs, "http_request_rejected", 1)
		records := rejectedRecords(t, logs)
		if len(records) != 1 || records[0].Reason != string(RejectionHeaderTooLarge) || records[0].Status != http.StatusRequestHeaderFieldsTooLarge {
			t.Fatalf("rejected records = %+v, want one request_header_too_large 431: %s", records, logs.String())
		}
		if strings.Contains(logs.String(), `"path":"/missing"`) {
			t.Errorf("a /missing record was written for a request behind a rejected head: %s", logs.String())
		}
	})

	t.Run("a fresh connection keeps the first head limit", func(t *testing.T) {
		for _, tc := range []struct {
			size int
			want int
		}{
			{maxObservedHeadBytes, http.StatusOK},
			{maxObservedHeadBytes + 1, http.StatusRequestHeaderFieldsTooLarge},
		} {
			addr, logs := serveObserved(t, handler)
			conn, reader := dial(t, addr)
			if got := roundTrip(t, conn, reader, headOfSize("/big", tc.size, "")); got != tc.want {
				t.Errorf("size %d: status = %d, want %d: %s", tc.size, got, tc.want, logs.String())
			}
		}
	})

	t.Run("a stall in the middle of a line after a large head is a malformed request", func(t *testing.T) {
		addr, logs := serveObservedWith(t, handler, func(srv *http.Server) {
			srv.ReadHeaderTimeout = headerTimeout
		})
		conn, reader := dial(t, addr)
		if got := roundTrip(t, conn, reader, first); got != http.StatusOK {
			t.Fatalf("first status = %d, want 200", got)
		}
		if got := roundTrip(t, conn, reader, headOfSize("/big", maxObservedHeadBytes+904, "")); got != http.StatusOK {
			t.Fatalf("big status = %d, want 200", got)
		}
		if _, err := io.WriteString(conn, "GET /stall HTTP/1.1\r\nHos"); err != nil {
			t.Fatalf("write: %v", err)
		}
		_, _ = io.ReadAll(reader)
		waitFor(logs, "http_request_rejected", 1)
		records := rejectedRecords(t, logs)
		if len(records) != 1 || records[0].Reason != string(RejectionMalformedRequest) || records[0].Status != http.StatusBadRequest {
			t.Fatalf("rejected records = %+v, want one malformed_request 400: %s", records, logs.String())
		}
	})
}
