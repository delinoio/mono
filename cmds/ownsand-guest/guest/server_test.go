// SPDX-License-Identifier: FSL-1.1-ALv2

package guest

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
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
