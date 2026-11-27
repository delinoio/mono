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
		{"empty data", "data\n\n", []Event{{Name: "message", Data: []byte{}}}},
		{"empty data lines", "data:\ndata: ok\ndata:\n\n", []Event{{Name: "message", Data: []byte("\nok\n")}}},
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

func TestReadEventsPayloadLimit(t *testing.T) {
	const limit = 1 << 20
	first := strings.Repeat("é", limit/4)
	second := strings.Repeat("b", limit-len(first)-1)
	for _, tc := range []struct {
		name   string
		suffix string
	}{
		{name: "at limit"},
		{name: "over limit from data", suffix: "b"},
		{name: "over limit from separator", suffix: "\rdata:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := "id: cursor\ndata: before\n\nevent: large\r\ndata: " + first +
				"\r\ndata: " + second + tc.suffix + "\r\n\r\ndata: after\n\n"
			var got []Event
			err := ReadEvents(strings.NewReader(body), func(e Event) error {
				got = append(got, e)
				return nil
			})
			if tc.suffix != "" {
				if err == nil || err.Error() != fmt.Sprintf("event stream payload exceeds %d bytes", limit) {
					t.Errorf("ReadEvents returned %v, want a payload limit error", err)
				}
				if len(got) != 1 || got[0].ID != "cursor" || string(got[0].Data) != "before" {
					t.Errorf("want only the preceding event, got %d events", len(got))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := []Event{
				{ID: "cursor", Name: "message", Data: []byte("before")},
				{ID: "cursor", Name: "large", Data: []byte(first + "\n" + second)},
				{ID: "cursor", Name: "message", Data: []byte("after")},
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("want three ordered events with ID carryover and complete payloads, got %d events", len(got))
			}
		})
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

func TestStreamErrorBodyDeadline(t *testing.T) {
	const budget = 150 * time.Millisecond
	for _, path := range []string{"/diagnostic-timeout", "/parent-cancellation", "/healthy"} {
		t.Run(path[1:], func(t *testing.T) {
			requestCanceled := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/healthy" {
					w.Header().Set("Content-Type", "text/event-stream")
					w.(http.Flusher).Flush()
					time.Sleep(2 * budget)
					fmt.Fprint(w, "data: delayed event\n\n")
					return
				}
				w.Header().Set("Retry-After", "7")
				w.WriteHeader(http.StatusServiceUnavailable)
				fmt.Fprint(w, "partial diagnostic")
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				close(requestCanceled)
			}))
			t.Cleanup(srv.Close)
			deadline, stop := context.WithTimeout(t.Context(), 5*time.Second)
			defer stop()
			ctx, cancel := context.WithCancel(deadline)
			defer cancel()
			c := Client{Vendor: "DEVIN", BaseURL: srv.URL, StreamErrorBodyTimeout: budget}
			if path == "/parent-cancellation" {
				c.StreamErrorBodyTimeout = time.Minute
				c.HTTP = &http.Client{Transport: cancelOnBodyRead{cancel: cancel}}
			}

			var data string
			err := c.Stream(ctx, "watch_queue", path, nil, nil, func(e Event) error {
				data = string(e.Data)
				return nil
			})
			if path == "/healthy" {
				if err != nil || data != "delayed event" {
					t.Fatalf("healthy stream: data %q, error %v", data, err)
				}
				return
			}
			if deadline.Err() != nil || (path == "/diagnostic-timeout" && ctx.Err() != nil) {
				t.Fatalf("read returned only after caller timeout: %v", err)
			}
			want := StatusError{Vendor: "DEVIN", Op: "watch_queue", Status: http.StatusServiceUnavailable, RetryAfter: 7 * time.Second}
			if got, ok := AsStatus(err); !ok || *got != want {
				t.Fatalf("status error = %#v, want %+v", err, want)
			}
			select {
			case <-requestCanceled:
			case <-deadline.Done():
				t.Fatal("blocked diagnostic read did not cancel the HTTP request")
			}
		})
	}
}

type cancelOnBodyRead struct{ cancel context.CancelFunc }

func (tr cancelOnBodyRead) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err == nil {
		resp.Body = cancelingBody{ReadCloser: resp.Body, cancel: tr.cancel}
	}
	return resp, err
}

type cancelingBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b cancelingBody) Read(p []byte) (int, error) {
	b.cancel()
	return b.ReadCloser.Read(p)
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
