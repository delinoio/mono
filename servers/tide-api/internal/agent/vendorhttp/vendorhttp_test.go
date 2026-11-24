// SPDX-License-Identifier: FSL-1.1-ALv2

package vendorhttp

import (
	"bytes"
	"context"
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

// TestClientErrorsAndLogs: a non-2xx answer is a StatusError with the code
// and error text, it is logged with the status, and neither the credential
// nor the request body nor query values reach the log.
func TestClientErrorsAndLogs(t *testing.T) {
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
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusConflict)
			fmt.Fprint(w, `{"code":"worker_in_use","error":"Worker in use: turn running","token":"leaked-token"}`)
		}
	}))
	defer srv.Close()
	var logs bytes.Buffer
	c := &Client{Vendor: "TEST", BaseURL: srv.URL, Auth: Bearer("secret-token"), Logger: slog.New(slog.NewJSONHandler(&logs, nil))}
	ctx := context.Background()
	var out struct{ Value string }
	if err := c.Do(ctx, "get", http.MethodGet, "/ok", nil, nil, &out); err != nil || out.Value != "v" {
		t.Fatalf("ok: %v %+v", err, out)
	}
	err := c.Do(ctx, "claim", http.MethodPost, "/claim", map[string][]string{"q": {"query-value"}}, map[string]string{"apiKey": "body-secret"}, nil)
	se, ok := AsStatus(err)
	if !ok || se.Status != http.StatusConflict || se.Code != "worker_in_use" || se.Text != "Worker in use: turn running" || se.RetryAfter != 7*time.Second || Status(err) != 409 {
		t.Fatalf("status error %#v", err)
	}
	for _, secret := range []string{"secret-token", "body-secret", "query-value", "leaked-token"} {
		if strings.Contains(logs.String(), secret) || strings.Contains(err.Error(), secret) {
			t.Fatalf("%q leaked: %s / %v", secret, logs.String(), err)
		}
	}
	if !strings.Contains(logs.String(), `"msg":"vendor_api_error"`) || !strings.Contains(logs.String(), `"status":409`) {
		t.Fatalf("log %s", logs.String())
	}
	var events []Event
	if err := c.Stream(ctx, "watch", "/stream", nil, nil, func(e Event) error { events = append(events, e); return nil }); err != nil || len(events) != 1 || events[0].ID != "1" {
		t.Fatalf("stream: %v %+v", err, events)
	}
}
