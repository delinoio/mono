// SPDX-License-Identifier: FSL-1.1-ALv2

package guest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/delinoio/mono/go/ownsand/datapath"
	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
)

const testSandbox = "0192f4b6-7c3a-7def-8abc-0123456789ab"

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type harness struct {
	t      *testing.T
	srv    *Server
	client ownsandv1connect.GuestServiceClient
	clock  *fakeClock
}

func newHarness(t *testing.T, mutate func(*Config)) *harness {
	t.Helper()
	clock := &fakeClock{now: time.Now()}
	cfg := Config{StateDir: t.TempDir(), Version: "test", Clock: ClockModeIgnore, Now: clock.Now, KeepaliveInterval: time.Hour, SweepInterval: time.Hour}
	if mutate != nil {
		mutate(&cfg)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Loopback TCP: a unix socket path under a deep TMPDIR can exceed the
	// 108-byte limit.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.Run(ctx, l)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		srv.execs.wg.Wait()
	})
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	hc := &http.Client{Transport: &http.Transport{
		Protocols: &protocols,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", l.Addr().String())
		},
	}}
	return &harness{t: t, srv: srv, client: ownsandv1connect.NewGuestServiceClient(hc, "http://guest"), clock: clock}
}

// stream is one Serve call.
type stream struct {
	t *testing.T
	s *connect.BidiStreamForClient[ownsandv1.ServeRequest, ownsandv1.ServeResponse]
}

func (h *harness) open(ctx context.Context) *stream {
	return &stream{t: h.t, s: h.client.Serve(ctx)}
}

func (s *stream) send(id uint64, payload proto.Message) {
	s.t.Helper()
	f := datapath.MustNewFrame(id, testSandbox, payload)
	if err := s.s.Send(&ownsandv1.ServeRequest{Frame: f}); err != nil {
		s.t.Fatal(err)
	}
}

func (s *stream) recv() *ownsandv1.DataFrame {
	s.t.Helper()
	resp, err := s.s.Receive()
	if err != nil {
		s.t.Fatalf("receive: %v", err)
	}
	return resp.GetFrame()
}

func newID(t *testing.T) string {
	id, err := uuidv7.NewString()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// runExec sends an exec request on a new stream and collects its events up
// to the final event.
func (h *harness) runExec(req *ownsandv1.ExecRequest) []*ownsandv1.ExecEvent {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s := h.open(ctx)
	defer s.s.CloseRequest()
	s.send(1, req)
	return s.collectExec()
}

func (s *stream) collectExec() []*ownsandv1.ExecEvent {
	s.t.Helper()
	var evs []*ownsandv1.ExecEvent
	for {
		f := s.recv()
		if f.GetError() != nil {
			s.t.Fatalf("error frame: %v", f.GetError())
		}
		ev := f.GetExecEvent()
		if ev.GetKeepalive() != nil {
			continue
		}
		evs = append(evs, ev)
		switch ev.GetEvent().(type) {
		case *ownsandv1.ExecEvent_Exited, *ownsandv1.ExecEvent_Failed:
			return evs
		}
	}
}

func outputs(evs []*ownsandv1.ExecEvent) (stdout, stderr string) {
	for _, ev := range evs {
		stdout += string(ev.GetStdout().GetData())
		stderr += string(ev.GetStderr().GetData())
	}
	return
}

func errorCode(err error) connect.Code {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return ce.Code()
	}
	return connect.CodeUnknown
}

func TestCheckReadyAndSetClock(t *testing.T) {
	h := newHarness(t, nil)
	resp, err := h.client.CheckReady(context.Background(), connect.NewRequest(&ownsandv1.CheckReadyRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Msg.GetGuestOs() != GuestOS() || resp.Msg.GetHelperVersion() != "test" {
		t.Fatalf("check ready %v", resp.Msg)
	}
	if _, err := h.client.SetClock(context.Background(), connect.NewRequest(&ownsandv1.SetClockRequest{Now: timestamppb.Now()})); err != nil {
		t.Fatalf("set clock in ignore mode: %v", err)
	}
	if _, err := h.client.SetClock(context.Background(), connect.NewRequest(&ownsandv1.SetClockRequest{})); errorCode(err) != connect.CodeInvalidArgument {
		t.Fatalf("set clock without time: %v", err)
	}
}

func TestUnknownKindGetsErrorFrame(t *testing.T) {
	h := newHarness(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s := h.open(ctx)
	f := &ownsandv1.DataFrame{StreamId: 7, SandboxId: testSandbox, Kind: ownsandv1.DataFrameKind(99)}
	if err := s.s.Send(&ownsandv1.ServeRequest{Frame: f}); err != nil {
		t.Fatal(err)
	}
	got := s.recv()
	if got.GetStreamId() != 7 || got.GetError().GetCode() != ownsandv1.ErrorCode_ERROR_CODE_UNIMPLEMENTED {
		t.Fatalf("got %v", got)
	}
	s.send(8, &ownsandv1.ExecEvent{})
	if got := s.recv(); got.GetError().GetCode() != ownsandv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT {
		t.Fatalf("non-request kind: %v", got)
	}
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) records(t *testing.T) []map[string]any {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(b.buf.Bytes()), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal(line, &rec); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

func findRecord(recs []map[string]any, msg string) map[string]any {
	for _, r := range recs {
		if r["msg"] == msg {
			return r
		}
	}
	return nil
}

func TestSetClockLogsAppliedOffset(t *testing.T) {
	base := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		offset time.Duration
	}{
		{"guest behind", 5 * time.Second},
		{"guest ahead", -3 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := &lockedBuffer{}
			var mu sync.Mutex
			var calls []time.Time
			h := newHarness(t, func(cfg *Config) {
				cfg.Clock = ClockModeSet
				cfg.Logger = slog.New(slog.NewJSONHandler(logs, nil))
				cfg.Now = func() time.Time { return base }
				cfg.setClock = func(tm time.Time) error {
					mu.Lock()
					defer mu.Unlock()
					calls = append(calls, tm)
					return nil
				}
			})
			want := base.Add(tc.offset)
			if _, err := h.client.SetClock(context.Background(), connect.NewRequest(&ownsandv1.SetClockRequest{Now: timestamppb.New(want)})); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(calls) != 1 || !calls[0].Equal(want) {
				t.Fatalf("setter calls %v, want one call with %v", calls, want)
			}
			rec := findRecord(logs.records(t), "clock set")
			if rec == nil {
				t.Fatal("no clock set record")
			}
			if got := rec["offset_ms"]; got != float64(tc.offset.Milliseconds()) {
				t.Fatalf("offset_ms = %v, want %d", got, tc.offset.Milliseconds())
			}
		})
	}
}

func TestSetClockSetterErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code connect.Code
	}{
		{"unsupported", errors.ErrUnsupported, connect.CodeUnimplemented},
		{"other", errors.New("boom"), connect.CodeInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := &lockedBuffer{}
			h := newHarness(t, func(cfg *Config) {
				cfg.Clock = ClockModeSet
				cfg.Logger = slog.New(slog.NewJSONHandler(logs, nil))
				cfg.setClock = func(time.Time) error { return tc.err }
			})
			_, err := h.client.SetClock(context.Background(), connect.NewRequest(&ownsandv1.SetClockRequest{Now: timestamppb.Now()}))
			if errorCode(err) != tc.code {
				t.Fatalf("code = %v, want %v", errorCode(err), tc.code)
			}
			recs := logs.records(t)
			if findRecord(recs, "set clock failed") == nil {
				t.Fatal("no set clock failed record")
			}
			if findRecord(recs, "clock set") != nil {
				t.Fatal("unexpected clock set record")
			}
		})
	}
}

func TestSetClockIgnoreModeNeverCallsSetter(t *testing.T) {
	logs := &lockedBuffer{}
	var called atomic.Bool
	h := newHarness(t, func(cfg *Config) {
		cfg.Clock = ClockModeIgnore
		cfg.Logger = slog.New(slog.NewJSONHandler(logs, nil))
		cfg.setClock = func(time.Time) error { called.Store(true); return nil }
	})
	if _, err := h.client.SetClock(context.Background(), connect.NewRequest(&ownsandv1.SetClockRequest{Now: timestamppb.Now()})); err != nil {
		t.Fatal(err)
	}
	if called.Load() {
		t.Fatal("setter called in ignore mode")
	}
	if findRecord(logs.records(t), "set clock ignored") == nil {
		t.Fatal("no set clock ignored record")
	}
}
