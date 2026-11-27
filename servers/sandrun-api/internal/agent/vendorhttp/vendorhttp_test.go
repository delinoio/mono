// SPDX-License-Identifier: FSL-1.1-ALv2

package vendorhttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

func TestReadEvents(t *testing.T) {
	body := ": connected\n\nevent: created\nid: c1\ndata: {\"id\":\"a\",\ndata: \"x\":1}\n\nevent: heartbeat\ndata: {}\n\nid: c2\n\ndata: plain\n\n"
	var got []Event
	if err := ReadEvents(strings.NewReader(body), func(e Event) error { got = append(got, e); return nil }); err != nil {
		t.Fatal(err)
	}
	want := []Event{
		{ID: "c1", Name: "created", Data: []byte("{\"id\":\"a\",\n\"x\":1}")},
		{ID: "c1", Name: "heartbeat", Data: []byte("{}")},
		{ID: "c2", Name: "message", Data: []byte("plain")},
	}
	if len(got) != len(want) {
		t.Fatalf("events %+v", got)
	}
	for i := range want {
		if got[i].ID != want[i].ID || got[i].Name != want[i].Name || !bytes.Equal(got[i].Data, want[i].Data) {
			t.Fatalf("event %d: %+v, want %+v", i, got[i], want[i])
		}
	}
	stop := errors.New("stop")
	if err := ReadEvents(strings.NewReader(body), func(Event) error { return stop }); !errors.Is(err, stop) {
		t.Fatalf("callback error: %v", err)
	}
}

func TestReadEventsLineEndingsAndBOM(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want []Event
	}{
		{"LF", "data: ok\n\n", []Event{{Name: "message", Data: []byte("ok")}}},
		{"CRLF", "data: ok\r\n\r\n", []Event{{Name: "message", Data: []byte("ok")}}},
		{"CR", "data: ok\r\r", []Event{{Name: "message", Data: []byte("ok")}}},
		{
			"mixed with split CRLF",
			"event: created\r\nid: c1\rdata: first\n: comment\rdata: second\r\n\r\ndata: after\n\n",
			[]Event{
				{ID: "c1", Name: "created", Data: []byte("first\nsecond")},
				{ID: "c1", Name: "message", Data: []byte("after")},
			},
		},
		{"BOM before data", "\uFEFFdata: ok\n\n", []Event{{Name: "message", Data: []byte("ok")}}},
		{
			"BOM before event and in data",
			"\uFEFFevent: created\rdata: \uFEFFok\n\n",
			[]Event{{Name: "created", Data: []byte("\uFEFFok")}},
		},
		{"second leading BOM", "\uFEFF\uFEFFdata: ignored\n\n", nil},
		{"BOM at start of later line", ": comment\n\uFEFFdata: ignored\n\n", nil},
		{"incomplete at EOF", "data: incomplete\r", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []Event
			reader := iotest.OneByteReader(strings.NewReader(tc.body))
			if err := ReadEvents(reader, func(e Event) error {
				got = append(got, e)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("events = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestReadEventsDispatchesCRBeforeReadingAgain(t *testing.T) {
	stop := errors.New("stop after event")
	readErr := errors.New("unexpected read")
	reader := io.MultiReader(strings.NewReader("data: ok\r\r"), iotest.ErrReader(readErr))
	err := ReadEvents(reader, func(e Event) error {
		if e.Name != "message" || string(e.Data) != "ok" {
			t.Fatalf("unexpected event: %+v", e)
		}
		return stop
	})
	if !errors.Is(err, stop) {
		t.Fatalf("ReadEvents returned %v, want %v", err, stop)
	}
}

func TestReadEventsReaderErrorAndLineLimit(t *testing.T) {
	readErr := errors.New("failed to read stream")
	reader := io.MultiReader(strings.NewReader("data: incomplete\r"), iotest.ErrReader(readErr))
	unexpectedEvent := func(e Event) error {
		t.Fatalf("unexpected event: %+v", e)
		return nil
	}
	if err := ReadEvents(reader, unexpectedEvent); !errors.Is(err, readErr) {
		t.Fatalf("ReadEvents returned %v, want %v", err, readErr)
	}
	for _, ending := range []string{"\n", "\r\n", "\r"} {
		line := "data:" + strings.Repeat("a", maxEventLine-len("data:"))
		if err := ReadEvents(strings.NewReader(line+ending+ending), unexpectedEvent); err == nil {
			t.Errorf("ReadEvents accepted an oversized line ending in %q", ending)
		}
	}
}

func TestClientRequiresJSONForDestination(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"empty 200", http.StatusOK, ""},
		{"whitespace 200", http.StatusOK, " \t\r\n"},
		{"bodyless 204", http.StatusNoContent, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				if tc.body != "" {
					fmt.Fprint(w, tc.body)
				}
			}))
			defer srv.Close()
			c := &Client{Vendor: "TEST", BaseURL: srv.URL}
			var out struct{ Value string }
			err := c.Do(t.Context(), "get", http.MethodGet, "/", nil, nil, &out)
			if err == nil || err.Error() != "TEST get: decode response: empty body" {
				t.Fatalf("required JSON: got %v, want an empty-body decode error", err)
			}
			if err := c.Do(t.Context(), "release", http.MethodPost, "/", nil, nil, nil); err != nil {
				t.Fatalf("no destination: %v", err)
			}
		})
	}
}

func TestClientMalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"value":`)
	}))
	defer srv.Close()
	c := &Client{Vendor: "TEST", BaseURL: srv.URL}
	var out struct{ Value string }
	err := c.Do(t.Context(), "get", http.MethodGet, "/", nil, nil, &out)
	var syntaxErr *json.SyntaxError
	if !errors.As(err, &syntaxErr) || !strings.HasPrefix(err.Error(), "TEST get: decode response: ") {
		t.Fatalf("malformed JSON: got %v, want a wrapped JSON syntax error", err)
	}
}

type streamTransport func(*http.Request) (*http.Response, error)

func (f streamTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type streamBody struct {
	io.Reader
	reads  int
	closed bool
}

func (b *streamBody) Read(p []byte) (int, error) {
	b.reads++
	return b.Reader.Read(p)
}

func (b *streamBody) Close() error {
	b.closed = true
	return nil
}

func TestStreamMediaType(t *testing.T) {
	for _, tc := range []struct {
		name        string
		contentType string
		wantEvent   bool
	}{
		{name: "HTML", contentType: "text/html; vendor=private-header"},
		{name: "missing"},
		{name: "malformed", contentType: "text/event-stream; private-header"},
		{name: "SSE", contentType: "text/event-stream", wantEvent: true},
		{name: "SSE with parameters", contentType: "Text/Event-Stream; charset=utf-8", wantEvent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := "<html>Maintenance</html>"
			if tc.wantEvent {
				payload = "data: {}\n\n"
			}
			body := &streamBody{Reader: strings.NewReader(payload)}
			header := make(http.Header)
			if tc.contentType != "" {
				header.Set("Content-Type", tc.contentType)
			}
			c := &Client{
				Vendor: "TEST", BaseURL: "https://vendor.example", Auth: Bearer("test-credential"),
				HTTP: &http.Client{Transport: streamTransport(func(r *http.Request) (*http.Response, error) {
					if got := r.Header.Get("Accept"); got != "text/event-stream" {
						t.Errorf("Accept = %q, want text/event-stream", got)
					}
					return &http.Response{StatusCode: http.StatusOK, Header: header, Body: body}, nil
				})},
			}
			var events []Event
			err := c.Stream(t.Context(), "watch", "/stream", nil, nil, func(e Event) error {
				events = append(events, e)
				return nil
			})
			if !body.closed {
				t.Error("response body was left open")
			}
			if tc.wantEvent {
				if err != nil || len(events) != 1 || string(events[0].Data) != "{}" {
					t.Fatalf("stream returned error %v and events %+v, want one event and clean EOF", err, events)
				}
				return
			}
			if err == nil || err.Error() != "TEST watch: expected Content-Type text/event-stream" {
				t.Errorf("stream returned %v, want a sanitized media type error", err)
			}
			if body.reads != 0 || len(events) != 0 {
				t.Errorf("stream read the body %d times and dispatched %d events", body.reads, len(events))
			}
		})
	}
}

func TestStreamCanceledBeforeReadingResponse(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	body := &streamBody{Reader: strings.NewReader("data: {}\n\n")}
	c := &Client{
		Vendor: "TEST", BaseURL: "https://vendor.example",
		HTTP: &http.Client{Transport: streamTransport(func(*http.Request) (*http.Response, error) {
			cancel()
			return &http.Response{StatusCode: http.StatusOK, Body: body}, nil
		})},
	}
	err := c.Stream(ctx, "watch", "/stream", nil, nil, func(Event) error {
		t.Error("canceled stream dispatched an event")
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("stream returned %v, want context.Canceled", err)
	}
	if body.reads != 0 || !body.closed {
		t.Errorf("canceled stream read the body %d times, closed = %t", body.reads, body.closed)
	}
}

func TestClientSuccessfulJSONAndSSE(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/ok":
			fmt.Fprint(w, `{"value":"v"}`)
		case "/stream":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: a\nid: 1\ndata: {}\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := &Client{Vendor: "TEST", BaseURL: srv.URL, Auth: Bearer("secret-token")}
	ctx := context.Background()
	var out struct{ Value string }
	if err := c.Do(ctx, "get", http.MethodGet, "/ok", nil, nil, &out); err != nil || out.Value != "v" {
		t.Fatalf("ok: %v %+v", err, out)
	}
	var events []Event
	if err := c.Stream(ctx, "watch", "/stream", nil, nil, func(e Event) error { events = append(events, e); return nil }); err != nil || len(events) != 1 || events[0].ID != "1" {
		t.Fatalf("stream: %v %+v", err, events)
	}
}

func TestClientErrorsAndLogs(t *testing.T) {
	for _, tc := range []struct {
		name, vendor, body, wantCode string
	}{
		{"known nested code", "OPENAI_CODEX", `{"error":{"code":"invalid_api_key","message":"fixture-message"}}`, "invalid_api_key"},
		{"known top-level code", "OPENAI_CODEX", `{"code":"invalid_api_key","error":"fixture-error","message":"fixture-message"}`, "invalid_api_key"},
		{"unknown codes", "OPENAI_CODEX", `{"code":"fixture-code","message":"fixture-message","error":{"code":"fixture-nested-code","message":"fixture-nested-message"},"token":"fixture-response-token"}`, ""},
		{"object code", "OPENAI_CODEX", `{"code":{"code":"invalid_api_key","message":"fixture-message"}}`, ""},
		{"numeric code", "OPENAI_CODEX", `{"error":{"code":123,"message":"fixture-message"}}`, ""},
		{"message with known text", "OPENAI_CODEX", `{"message":"invalid_api_key","error":{"message":"invalid_api_key"}}`, ""},
		{"plain error with known text", "OPENAI_CODEX", `{"error":"invalid_api_key"}`, ""},
		{"different case", "OPENAI_CODEX", `{"code":"INVALID_API_KEY","error":{"Code":"invalid_api_key"}}`, ""},
		{"extra whitespace in code", "OPENAI_CODEX", `{"code":"invalid_api_key "}`, ""},
		{"malformed JSON", "OPENAI_CODEX", `{"code":"invalid_api_key","error":"fixture-truncated"`, ""},
		{"plain body", "OPENAI_CODEX", "fixture-plain-body", ""},
		{"empty body", "OPENAI_CODEX", "", ""},
		{"oversized body", "OPENAI_CODEX", `{"code":"invalid_api_key"}` + strings.Repeat(" ", maxErrorBody) + "fixture-trailing-body", ""},
		{"cursor", "CURSOR", `{"code":"invalid_api_key","message":"fixture-message"}`, ""},
		{"devin", "DEVIN", `{"error":{"code":"invalid_api_key","message":"fixture-message"}}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer fixture-credential" {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				w.Header().Set("Retry-After", "7")
				w.WriteHeader(http.StatusConflict)
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()

			for _, op := range []string{"claim", "watch"} {
				t.Run(op, func(t *testing.T) {
					var logs bytes.Buffer
					c := &Client{Vendor: tc.vendor, BaseURL: srv.URL, Auth: Bearer("fixture-credential"), Logger: slog.New(slog.NewJSONHandler(&logs, nil))}
					query := map[string][]string{"q": {"fixture-query"}}
					var err error
					switch op {
					case "claim":
						var out struct{ Value string }
						err = c.Do(t.Context(), op, http.MethodPost, "/", query, map[string]string{"data": "fixture-request"}, &out)
					case "watch":
						err = c.Stream(t.Context(), op, "/", query, nil, func(e Event) error {
							t.Fatalf("unexpected event: %+v", e)
							return nil
						})
					}

					se, ok := AsStatus(err)
					wantStatus := StatusError{Vendor: tc.vendor, Op: op, Status: http.StatusConflict, Code: tc.wantCode, RetryAfter: 7 * time.Second}
					if !ok || *se != wantStatus || Status(err) != http.StatusConflict {
						t.Fatalf("status error = %#v, want %+v", err, wantStatus)
					}
					wantError := fmt.Sprintf("%s %s: HTTP %d", tc.vendor, op, http.StatusConflict)
					if tc.wantCode != "" {
						wantError += " " + tc.wantCode
					}
					if err.Error() != wantError {
						t.Errorf("formatted error = %q, want %q", err, wantError)
					}

					var gotLog map[string]any
					if err := json.Unmarshal(logs.Bytes(), &gotLog); err != nil {
						t.Fatalf("decode warning: %v", err)
					}
					delete(gotLog, slog.TimeKey)
					wantLog := map[string]any{
						"level": "WARN", "msg": "vendor_api_error", "vendor": tc.vendor,
						"op": op, "status": float64(http.StatusConflict),
					}
					if tc.wantCode != "" {
						wantLog["code"] = tc.wantCode
					}
					if !reflect.DeepEqual(gotLog, wantLog) {
						t.Errorf("warning = %v, want %v", gotLog, wantLog)
					}
				})
			}
		})
	}
}
