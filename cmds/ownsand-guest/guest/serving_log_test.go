// SPDX-License-Identifier: FSL-1.1-ALv2

package guest

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeUnixListener reports a unix address and blocks in Accept until Close.
type fakeUnixListener struct {
	addr   net.Addr
	closed chan struct{}
	once   sync.Once
}

func newFakeUnixListener(path string) *fakeUnixListener {
	return &fakeUnixListener{addr: &net.UnixAddr{Name: path, Net: "unix"}, closed: make(chan struct{})}
}

func (l *fakeUnixListener) Accept() (net.Conn, error) {
	<-l.closed
	return nil, net.ErrClosed
}

func (l *fakeUnixListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *fakeUnixListener) Addr() net.Addr { return l.addr }

// runServingEvent runs the server on l until the serving event is logged,
// stops it and returns the logged JSON lines.
func runServingEvent(t *testing.T, l net.Listener) []map[string]any {
	t.Helper()
	out := &syncBuffer{}
	srv, err := New(Config{
		StateDir:          t.TempDir(),
		Version:           "test",
		Clock:             ClockModeIgnore,
		KeepaliveInterval: time.Hour,
		SweepInterval:     time.Hour,
		Logger:            slog.New(slog.NewJSONHandler(out, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- srv.Run(ctx, l) }()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(out.String(), "guest helper serving") {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("serving event was not logged")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after shutdown")
	}
	var events []map[string]any
	dec := json.NewDecoder(strings.NewReader(out.String()))
	for {
		var ev map[string]any
		if err := dec.Decode(&ev); err != nil {
			if err != io.EOF {
				t.Fatalf("decode log: %v", err)
			}
			break
		}
		events = append(events, ev)
	}
	return events
}

func servingEvent(t *testing.T, events []map[string]any) map[string]any {
	t.Helper()
	for _, ev := range events {
		if ev["msg"] == "guest helper serving" {
			return ev
		}
	}
	t.Fatalf("no serving event in %v", events)
	return nil
}

func TestServingEventOmitsUnixSocketPath(t *testing.T) {
	l := newFakeUnixListener("/fixture/guest.sock")
	events := runServingEvent(t, l)
	ev := servingEvent(t, events)
	if ev["network"] != "unix" {
		t.Fatalf("network = %v, want unix", ev["network"])
	}
	if _, ok := ev["address"]; ok {
		t.Fatalf("serving event has an address field: %v", ev)
	}
	for _, e := range events {
		raw, _ := json.Marshal(e)
		if strings.Contains(string(raw), "/fixture") || strings.Contains(string(raw), "guest.sock") {
			t.Fatalf("log event leaks the socket path: %s", raw)
		}
	}
}

func TestServingEventReportsTCPNetwork(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	events := runServingEvent(t, l)
	ev := servingEvent(t, events)
	if ev["network"] != "tcp" {
		t.Fatalf("network = %v, want tcp", ev["network"])
	}
	if _, ok := ev["address"]; ok {
		t.Fatalf("serving event has an address field: %v", ev)
	}
}
