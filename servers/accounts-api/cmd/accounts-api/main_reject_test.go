// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/delinoio/mono/servers/accounts-api/internal/server"
)

// These tests send raw bytes that net/http rejects before it calls the handler
// to a server built by newServer and served by serve, and check the
// http_request_rejected record next to the unchanged response.

// rejectedRecords decodes the http_request_rejected records among the JSON log
// lines.
func rejectedRecords(t *testing.T, logs *syncBuffer) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("log line is not JSON: %q: %v", line, err)
		}
		if record["msg"] == "http_request_rejected" {
			records = append(records, record)
		}
	}
	return records
}

// assertRejectedRecord checks the shape of a http_request_rejected record: no
// path, method or bytes, and a non-negative duration.
func assertRejectedRecord(t *testing.T, record map[string]any, reason server.RejectionReason, status int) {
	t.Helper()
	if record["level"] != "INFO" || record["reason"] != string(reason) || record["status"] != float64(status) {
		t.Errorf("record = %v, want an INFO http_request_rejected with reason %q and status %d", record, reason, status)
	}
	if duration, ok := record["duration_ms"].(float64); !ok || duration < 0 {
		t.Errorf("duration_ms = %v, want a non-negative number", record["duration_ms"])
	}
	for _, field := range []string{"path", "method", "bytes"} {
		if _, ok := record[field]; ok {
			t.Errorf("record = %v, want no %q field: it is not known before the request parses", record, field)
		}
	}
}

// startRejectLogServer serves newServer's production construction on a
// loopback listener with the header read and idle timeouts shrunk, so the tests
// can observe both timeouts.
func startRejectLogServer(t *testing.T, headerTimeout, idleTimeout time.Duration) (string, *syncBuffer) {
	t.Helper()
	logs := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, nil))
	srv := newServer(server.NewHandler(logger), logger)
	srv.ReadHeaderTimeout = headerTimeout
	srv.IdleTimeout = idleTimeout

	listener := newTestListener(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, noStop, srv, listener, logger, time.Second) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return listener.Addr().String(), logs
}

func TestNewServerLogsRequestsRejectedBeforeTheHandler(t *testing.T) {
	for _, tc := range []struct {
		name       string
		request    string
		wantStatus int
		wantReason server.RejectionReason
		// wantPrefix is the start of the response of net/http, which the
		// observer must leave unchanged.
		wantPrefix string
	}{
		{"malformed request line", "GARBAGE\r\n\r\n", 400, server.RejectionMalformedRequest, "HTTP/1.1 400 Bad Request\r\nContent-Type: text/plain; charset=utf-8\r\nConnection: close\r\n\r\n400 Bad Request"},
		{"missing Host header", "GET /healthz HTTP/1.1\r\n\r\n", 400, server.RejectionMalformedRequest, "HTTP/1.1 400 Bad Request: missing required Host header\r\n"},
		{"invalid header name", "GET /healthz HTTP/1.1\r\nHost: test\r\nBad Header: x\r\n\r\n", 400, server.RejectionMalformedRequest, "HTTP/1.1 400 Bad Request: invalid header name\r\n"},
		{"leading blank line", "\r\n\r\nGET /healthz HTTP/1.1\r\nHost: test\r\n\r\n", 400, server.RejectionMalformedRequest, "HTTP/1.1 400 Bad Request\r\n"},
		{"oversized head", "GET /healthz HTTP/1.1\r\nHost: test\r\nX-Fill: " + strings.Repeat("a", server.MaxHeaderBytes+8192) + "\r\n\r\n", 431, server.RejectionHeaderTooLarge, "HTTP/1.1 431 Request Header Fields Too Large\r\n"},
		{"unsupported transfer encoding", "POST /healthz HTTP/1.1\r\nHost: test\r\nTransfer-Encoding: gzip\r\n\r\n", 501, server.RejectionUnsupportedTransferEncoding, "HTTP/1.1 501 Not Implemented\r\nContent-Type: text/plain; charset=utf-8\r\nConnection: close\r\n\r\nUnsupported transfer encoding"},
		{"unsupported protocol version", "GET /healthz HTTP/2.0\r\nHost: test\r\n\r\n", 505, server.RejectionUnsupportedProtocolVersion, "HTTP/1.1 505 HTTP Version Not Supported: unsupported protocol version\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr, logs := startRequestLogServer(t)
			conn := dialRaw(t, addr)
			// The server may close while the client is still writing.
			go func() { _, _ = io.WriteString(conn, tc.request) }()
			response := readUntilClosed(t, conn)
			if !strings.HasPrefix(response, tc.wantPrefix) {
				t.Errorf("response = %.200q, want the unchanged net/http response starting %q", response, tc.wantPrefix)
			}
			if got := statusOf(t, response); got != tc.wantStatus {
				t.Errorf("status = %d, want %d", got, tc.wantStatus)
			}

			records := rejectedRecords(t, logs)
			if len(records) != 1 {
				t.Fatalf("got %d http_request_rejected records, want exactly 1: %s", len(records), logs.String())
			}
			assertRejectedRecord(t, records[0], tc.wantReason, tc.wantStatus)
			if records := requestRecords(t, logs); len(records) != 0 {
				t.Errorf("http_request records = %v, want none for a request that never reached the handler", records)
			}
			for _, secret := range []string{"Bad Header", "GARBAGE", "X-Fill", "gzip"} {
				if strings.Contains(logs.String(), secret) {
					t.Errorf("logs contain %q: %s", secret, logs.String())
				}
			}
		})
	}
}

func TestNewServerLogsHeaderReadTimeout(t *testing.T) {
	const headerTimeout = 200 * time.Millisecond
	for _, tc := range []struct {
		name string
		// send is written before the connection stalls.
		send string
	}{
		{"connection that sends nothing", ""},
		{"partial request head", "GET /healthz HTTP/1.1\r\nHost: test\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr, logs := startRejectLogServer(t, headerTimeout, time.Minute)
			conn := dialRaw(t, addr)
			if tc.send != "" {
				if _, err := io.WriteString(conn, tc.send); err != nil {
					t.Fatalf("write: %v", err)
				}
			}
			elapsed, response := expectClosedWithin(t, conn, 5*time.Second)
			if response != "" {
				t.Errorf("response = %q, want none: net/http closes without a response", response)
			}
			if elapsed < headerTimeout/2 {
				t.Errorf("connection closed after %v, want it to wait for ReadHeaderTimeout", elapsed)
			}

			records := rejectedRecords(t, logs)
			if len(records) != 1 {
				t.Fatalf("got %d http_request_rejected records, want exactly 1: %s", len(records), logs.String())
			}
			assertRejectedRecord(t, records[0], server.RejectionHeaderReadTimeout, 0)
			if duration := records[0]["duration_ms"].(float64); duration < float64(headerTimeout.Milliseconds())*0.5 {
				t.Errorf("duration_ms = %v, want about the %v timeout", duration, headerTimeout)
			}
			if records := requestRecords(t, logs); len(records) != 0 {
				t.Errorf("http_request records = %v, want none", records)
			}
		})
	}
}

func TestNewServerIdleKeepAliveTimeoutIsNotLogged(t *testing.T) {
	const idleTimeout = 200 * time.Millisecond
	addr, logs := startRejectLogServer(t, time.Minute, idleTimeout)
	conn := dialRaw(t, addr)
	reader := bufio.NewReader(conn)
	if _, err := io.WriteString(conn, "GET /healthz HTTP/1.1\r\nHost: test\r\n\r\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if status, body := readResponse(t, conn, reader); status != http.StatusOK || body != healthzBody {
		t.Fatalf("healthz = %d %q", status, body)
	}
	// The connection stays open for the next request until IdleTimeout.
	elapsed, rest := expectClosedWithin(t, conn, 5*time.Second)
	if rest != "" || elapsed < idleTimeout/2 {
		t.Fatalf("after %v the connection sent %q, want a silent close after the idle timeout", elapsed, rest)
	}
	time.Sleep(50 * time.Millisecond)
	if records := rejectedRecords(t, logs); len(records) != 0 {
		t.Errorf("records = %v, want none for an idle keep-alive close", records)
	}
	if records := requestRecords(t, logs); len(records) != 1 {
		t.Errorf("got %d http_request records, want exactly 1: %s", len(records), logs.String())
	}
}

func TestNewServerLogsPartialHeadTimeoutAfterKeepAliveRequest(t *testing.T) {
	const headerTimeout = 200 * time.Millisecond
	addr, logs := startRejectLogServer(t, headerTimeout, time.Minute)
	conn := dialRaw(t, addr)
	reader := bufio.NewReader(conn)
	if _, err := io.WriteString(conn, "GET /healthz HTTP/1.1\r\nHost: test\r\n\r\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if status, _ := readResponse(t, conn, reader); status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if _, err := io.WriteString(conn, "GET /healthz HTTP/1.1\r\nHost: test\r\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if rest, err := io.ReadAll(reader); err != nil || len(rest) != 0 {
		t.Fatalf("read = %q, %v; want a silent close", rest, err)
	}
	records := rejectedRecords(t, logs)
	if len(records) != 1 {
		t.Fatalf("got %d http_request_rejected records, want exactly 1: %s", len(records), logs.String())
	}
	assertRejectedRecord(t, records[0], server.RejectionHeaderReadTimeout, 0)
	if requests := requestRecords(t, logs); len(requests) != 1 {
		t.Errorf("got %d http_request records, want exactly 1 for the answered request: %s", len(requests), logs.String())
	}
}

// TestNewServerPartialHeadBufferedWithAnAnsweredRequestKeepsItsStart sends a
// complete request and the start of the next head in one write, then the rest
// of its Host header line late in the header read timeout. The record measures the
// time since net/http started to wait for the head (the end of the first
// response), not since that late byte.
func TestNewServerPartialHeadBufferedWithAnAnsweredRequestKeepsItsStart(t *testing.T) {
	const headerTimeout = 1500 * time.Millisecond
	addr, logs := startRejectLogServer(t, headerTimeout, time.Minute)
	conn := dialRaw(t, addr)
	reader := bufio.NewReader(conn)
	if _, err := io.WriteString(conn, "GET /healthz HTTP/1.1\r\nHost: test\r\n\r\nGET /healthz HTTP/1.1\r\nHost: te"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if status, _ := readResponse(t, conn, reader); status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	time.Sleep(headerTimeout * 2 / 3)
	if _, err := io.WriteString(conn, "st\r\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if rest, err := io.ReadAll(reader); err != nil || len(rest) != 0 {
		t.Fatalf("read = %q, %v; want a silent close", rest, err)
	}
	records := rejectedRecords(t, logs)
	if len(records) != 1 {
		t.Fatalf("got %d http_request_rejected records, want exactly 1: %s", len(records), logs.String())
	}
	assertRejectedRecord(t, records[0], server.RejectionHeaderReadTimeout, 0)
	// The late bytes arrived about a third of the timeout before the close.
	if duration := records[0]["duration_ms"].(float64); duration < float64(headerTimeout.Milliseconds())*0.9 {
		t.Errorf("duration_ms = %v, want about the %v timeout", duration, headerTimeout)
	}
}

func TestNewServerNormalRequestLogsNoRejection(t *testing.T) {
	addr, logs := startRequestLogServer(t)
	response := exchange(t, addr, "GET /healthz HTTP/1.1\r\nHost: test\r\nConnection: close\r\n\r\n")
	if !strings.HasPrefix(response, "HTTP/1.1 200 OK\r\n") {
		t.Fatalf("response = %q", response)
	}
	records := requestRecords(t, logs)
	if len(records) != 1 {
		t.Fatalf("got %d http_request records, want exactly 1: %s", len(records), logs.String())
	}
	assertRequestRecord(t, records[0], "GET", "/healthz", http.StatusOK, len(healthzBody))
	if records := rejectedRecords(t, logs); len(records) != 0 {
		t.Errorf("records = %v, want no rejection for a request the handler answered", records)
	}
}

// TestNewServerHandlerAnswerIsNotARejection checks that a 400 that the handler
// writes (the router rejects the request target "*") is one http_request record
// and no http_request_rejected record.
func TestNewServerHandlerAnswerIsNotARejection(t *testing.T) {
	addr, logs := startRequestLogServer(t)
	response := exchange(t, addr, "GET * HTTP/1.1\r\nHost: test\r\nConnection: close\r\n\r\n")
	if got := statusOf(t, response); got != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 from the router: %q", got, response)
	}
	records := requestRecords(t, logs)
	if len(records) != 1 {
		t.Fatalf("got %d http_request records, want exactly 1: %s", len(records), logs.String())
	}
	assertRequestRecord(t, records[0], "GET", "*", http.StatusBadRequest, 0)
	if records := rejectedRecords(t, logs); len(records) != 0 {
		t.Errorf("records = %v, want none: the handler answered", records)
	}
}

func TestNewServerKeepAliveSequenceEndingInMalformedRequestLogsEveryRequestOnce(t *testing.T) {
	const get = "GET /healthz HTTP/1.1\r\nHost: test\r\n\r\n"
	addr, logs := startRequestLogServer(t)
	response := exchange(t, addr, get+get+"GARBAGE\r\n\r\n")
	if got := strings.Count(response, "HTTP/1.1 200 OK"); got != 2 || !strings.HasSuffix(response, "400 Bad Request") {
		t.Errorf("response = %q, want two 200 responses and then the 400", response)
	}
	requests := requestRecords(t, logs)
	if len(requests) != 2 {
		t.Fatalf("got %d http_request records, want exactly 2: %s", len(requests), logs.String())
	}
	assertRequestRecord(t, requests[0], "GET", "/healthz", http.StatusOK, len(healthzBody))
	assertRequestRecord(t, requests[1], "GET", "/healthz", http.StatusOK, len(healthzBody))
	rejected := rejectedRecords(t, logs)
	if len(rejected) != 1 {
		t.Fatalf("got %d http_request_rejected records, want exactly 1: %s", len(rejected), logs.String())
	}
	assertRejectedRecord(t, rejected[0], server.RejectionMalformedRequest, http.StatusBadRequest)
}

func TestNewServerOversizedHeadAfterBodyLogsRejectionOnce(t *testing.T) {
	addr, logs := startRequestLogServer(t)
	huge := "GET /healthz HTTP/1.1\r\nHost: test\r\nX-Fill: " + strings.Repeat("a", server.MaxHeaderBytes+8192) + "\r\n\r\n"
	conn := dialRaw(t, addr)
	go func() { _, _ = io.WriteString(conn, postWithBody("hello")+huge) }()
	response := readUntilClosed(t, conn)
	if !strings.HasPrefix(response, "HTTP/1.1 405 ") || !strings.Contains(response, "HTTP/1.1 431 ") {
		t.Fatalf("response = %.200q, want a 405 and then the 431", response)
	}
	if requests := requestRecords(t, logs); len(requests) != 1 {
		t.Errorf("got %d http_request records, want exactly 1: %s", len(requests), logs.String())
	}
	rejected := rejectedRecords(t, logs)
	if len(rejected) != 1 {
		t.Fatalf("got %d http_request_rejected records, want exactly 1: %s", len(rejected), logs.String())
	}
	assertRejectedRecord(t, rejected[0], server.RejectionHeaderTooLarge, http.StatusRequestHeaderFieldsTooLarge)
}

// TestNewServerStalledBodyIsNotAHeaderReadTimeout checks that the read timeout
// of a request body, which is not a header timeout, is not logged as one.
func TestNewServerStalledBodyIsNotAHeaderReadTimeout(t *testing.T) {
	addr, logs := startShortTimeoutServer(t, 300*time.Millisecond)
	conn := dialRaw(t, addr)
	if _, err := io.WriteString(conn, "POST /healthz HTTP/1.1\r\nHost: test\r\nContent-Length: 100\r\n\r\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	expectClosedWithin(t, conn, 5*time.Second)
	time.Sleep(50 * time.Millisecond)
	if records := rejectedRecords(t, logs); len(records) != 0 {
		t.Errorf("records = %v, want none for a stalled request body", records)
	}
}

// TestNewServerHijackedConnectionIsNeverLogged hijacks a connection and lets
// the handler's own read time out, which must not be reported as a header read
// timeout of net/http.
func TestNewServerHijackedConnectionIsNeverLogged(t *testing.T) {
	logs := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, nil))
	hijacked := make(chan error, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := http.NewResponseController(w).Hijack()
		if err != nil {
			hijacked <- err
			return
		}
		defer conn.Close()
		// The handler owns the connection: its own read timeout, and its own
		// bytes that look like a net/http error response.
		_ = conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		_, err = conn.Read(make([]byte, 1))
		_, _ = io.WriteString(conn, "HTTP/1.1 400 Bad Request\r\nContent-Type: text/plain; charset=utf-8\r\nConnection: close\r\n\r\n400 Bad Request")
		hijacked <- err
	})
	srv := newServer(handler, logger)
	listener := newTestListener(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, noStop, srv, listener, logger, time.Second) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	conn := dialRaw(t, listener.Addr().String())
	if _, err := io.WriteString(conn, "GET / HTTP/1.1\r\nHost: test\r\n\r\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	select {
	case err := <-hijacked:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("handler read error = %v, want a timeout", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not finish")
	}
	readUntilClosed(t, conn)
	time.Sleep(50 * time.Millisecond)
	if records := rejectedRecords(t, logs); len(records) != 0 {
		t.Errorf("records = %v, want none for a hijacked connection", records)
	}
}

func TestNewServerSetsConnState(t *testing.T) {
	if srv := newServer(http.NotFoundHandler(), newTestLogger()); srv.ConnState == nil {
		t.Error("ConnState is nil; a hijacked connection would be reported as a header read timeout and no rejection would be logged after a handler response")
	}
}
