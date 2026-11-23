// SPDX-License-Identifier: FSL-1.1-ALv2

package server

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// dialObserved connects to addr with a bounded deadline and closes the
// connection when the test ends.
func dialObserved(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	// A generous bound that only fires when a test is already failing.
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	return conn
}

// TestObserverKeepsObservingWhenOnlyTheCloseFlagIsSet sends a first request
// whose parsed Close flag is true although net/http keeps the connection, and
// then a request that net/http rejects with 417. ReadRequest sets the flag
// from every Connection header value, but net/http decides the reuse of an
// HTTP/1.0 connection from the first value only, so the flag alone must not
// stop the observer: the 417 still gets exactly one http_request record.
func TestObserverKeepsObservingWhenOnlyTheCloseFlagIsSet(t *testing.T) {
	const (
		first = "GET /first HTTP/1.0\r\nHost: t\r\nConnection: keep-alive\r\nConnection: close\r\n\r\n"
		pony  = "GET /missing HTTP/1.1\r\nHost: t\r\nExpect: a-pony\r\n\r\n"
	)
	addr, logs := serveObserved(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	conn := dialObserved(t, addr)
	if _, err := io.WriteString(conn, first); err != nil {
		t.Fatalf("write first request: %v", err)
	}
	// Wait for the first response before the next request, so that the
	// second one reuses an open connection and is not pipelined.
	head := readResponseHead(t, conn)
	if !strings.Contains(head, " 200 ") || !strings.Contains(strings.ToLower(head), "connection: keep-alive") {
		t.Fatalf("first response = %q, want a 200 that keeps the connection alive", head)
	}
	if _, err := io.WriteString(conn, pony); err != nil {
		t.Fatalf("write second request: %v", err)
	}
	rest := string(readUntilServerClose(t, conn))
	if !strings.Contains(rest, "HTTP/1.1 417 ") || !strings.Contains(rest, "Connection: close") {
		t.Fatalf("second response = %q, want 417 with Connection: close", rest)
	}

	records := methodRecords(t, logs)
	if len(records) != 2 {
		t.Fatalf("http_request records = %+v, want 2: %s", records, logs.String())
	}
	if got := records[0]; got.Method != "GET" || got.Path != "/first" || got.Status != 200 {
		t.Errorf("record 0 = %+v, want GET /first 200", got)
	}
	if got := records[1]; got.Method != "GET" || got.Path != "/missing" || got.Status != 417 || got.Bytes != 0 {
		t.Errorf("record 1 = %+v, want GET /missing 417 with 0 bytes", got)
	}
	if n := countRecords(logs, "http_request_rejected"); n != 0 {
		t.Errorf("http_request_rejected records = %d, want 0: %s", n, logs.String())
	}
}

// TestObserverLogsNoRejectionBehindARealClose pipelines a request with an
// unsupported Expect header behind a request that really closes the
// connection. net/http reads nothing after the close, so the second request
// is never answered and must produce no record of its own.
func TestObserverLogsNoRejectionBehindARealClose(t *testing.T) {
	addr, logs := serveObserved(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	conn := dialObserved(t, addr)
	const request = "GET /a HTTP/1.1\r\nHost: t\r\nConnection: close\r\n\r\n" +
		"GET /b HTTP/1.1\r\nHost: t\r\nExpect: a-pony\r\n\r\n"
	if _, err := io.WriteString(conn, request); err != nil {
		t.Fatalf("write: %v", err)
	}
	response := string(readUntilServerClose(t, conn))
	if !strings.Contains(response, "HTTP/1.1 200 ") || strings.Contains(response, " 417 ") {
		t.Fatalf("response = %q, want only a 200", response)
	}
	records := methodRecords(t, logs)
	if len(records) != 1 || records[0].Path != "/a" || records[0].Status != 200 {
		t.Fatalf("http_request records = %+v, want only GET /a 200: %s", records, logs.String())
	}
	if n := countRecords(logs, "http_request_rejected"); n != 0 {
		t.Errorf("http_request_rejected records = %d, want 0: %s", n, logs.String())
	}
}

// readResponseHead reads one response head and its empty body from conn. The
// test handlers write an empty body, so the head ends the response.
func readResponseHead(t *testing.T, conn net.Conn) string {
	t.Helper()
	var head []byte
	buf := make([]byte, 1)
	for !strings.HasSuffix(string(head), "\r\n\r\n") {
		if _, err := conn.Read(buf); err != nil {
			t.Fatalf("read response head: %v after %q", err, head)
		}
		head = append(head, buf[0])
	}
	return string(head)
}

type methodRecord struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Status int    `json:"status"`
	Bytes  int64  `json:"bytes"`
}

// methodRecords returns the http_request records in logs, with their method,
// in log order.
func methodRecords(t *testing.T, logs *lockedBuffer) []methodRecord {
	t.Helper()
	var records []methodRecord
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		var entry struct {
			Msg string `json:"msg"`
			methodRecord
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("log line %q is not JSON: %v", line, err)
		}
		if entry.Msg == "http_request" {
			records = append(records, entry.methodRecord)
		}
	}
	return records
}
