// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build linux || darwin

package agent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/testcp"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
)

func newTestConn(queueSize int, cancel context.CancelFunc) *conn {
	return &conn{
		gen:    1,
		queue:  make(chan *ownsandv1.AgentEnvelope, queueSize),
		done:   make(chan struct{}),
		cancel: cancel,
		logger: quietLogger(),
	}
}

func heartbeatEnvelope() *ownsandv1.AgentEnvelope {
	return &ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_Heartbeat{Heartbeat: &ownsandv1.Heartbeat{Sequence: 1}}}
}

func TestConnSendWithSpareCapacityQueuesWithoutCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := newTestConn(2, cancel)
	env := heartbeatEnvelope()
	if !c.Send(env) || !c.Send(env) {
		t.Fatal("Send with spare capacity returned false")
	}
	if len(c.queue) != 2 {
		t.Fatalf("queued %d envelopes, want 2", len(c.queue))
	}
	select {
	case <-c.Done():
		t.Fatal("channel closed with spare capacity")
	default:
	}
	if ctx.Err() != nil {
		t.Fatal("stream context canceled with spare capacity")
	}
}

func TestConnSendOnFullQueueCancelsStreamContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := newTestConn(1, cancel)
	env := heartbeatEnvelope()
	if !c.Send(env) {
		t.Fatal("first Send returned false")
	}
	if c.Send(env) {
		t.Fatal("Send on a full queue returned true")
	}
	select {
	case <-c.Done():
	default:
		t.Fatal("Send on a full queue did not close the connection")
	}
	if ctx.Err() == nil {
		t.Fatal("Send on a full queue did not cancel the stream context")
	}
	if c.Send(env) {
		t.Fatal("Send on a closed connection returned true")
	}
}

type blockedChannelSend struct {
	first   atomic.Bool
	marked  *ownsandv1.AgentEnvelope
	entered chan struct{}
	result  chan error
}

func (*blockedChannelSend) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc { return next }

func (*blockedChannelSend) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

func (b *blockedChannelSend) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		stream := next(ctx, spec)
		if spec.Procedure == ownsandv1connect.AgentServiceChannelProcedure && b.first.CompareAndSwap(false, true) {
			return &blockedChannelSendConn{StreamingClientConn: stream, ctx: ctx, send: b}
		}
		return stream
	}
}

type blockedChannelSendConn struct {
	connect.StreamingClientConn
	ctx  context.Context
	send *blockedChannelSend
}

func (c *blockedChannelSendConn) Send(msg any) error {
	if req, ok := msg.(*ownsandv1.ChannelRequest); ok && req.GetEnvelope() == c.send.marked {
		close(c.send.entered)
		<-c.ctx.Done()
		err := c.ctx.Err()
		c.send.result <- err
		return err
	}
	return c.StreamingClientConn.Send(msg)
}

// TestChannelSendQueueOverflowEndsBlockedStream checks that an overflow cancels
// a blocked send and prompts the next channel to report capabilities and full state.
func TestChannelSendQueueOverflowEndsBlockedStream(t *testing.T) {
	cp := testcp.New(t)
	st, id := enrollDevice(t, cp)
	blocked := &blockedChannelSend{
		marked:  heartbeatEnvelope(),
		entered: make(chan struct{}),
		result:  make(chan error, 1),
	}
	h := startWith(t, cp, st, id, func(o *Options) {
		control := *o.Control
		control.Agent = ownsandv1connect.NewAgentServiceClient(control.HTTP, control.APIURL, connect.WithInterceptors(blocked))
		o.Control = &control
		// Keep heartbeat traffic and acknowledgement timeouts out of this test.
		o.HeartbeatInterval = time.Hour
		o.AckTimeout = time.Hour
	})

	isCapabilityReport := func(e *ownsandv1.AgentEnvelope) bool { return e.GetCapabilityReport() != nil }
	isStateReport := func(e *ownsandv1.AgentEnvelope) bool { return e.GetStateReport() != nil }
	_, firstCapability := h.wait(0, "capability report of the first channel", isCapabilityReport)
	_, firstState := h.wait(firstCapability+1, "state report of the first channel", isStateReport)

	h.agent.mu.Lock()
	first := h.agent.conn
	h.agent.mu.Unlock()
	if first == nil || cp.Channels() != 1 {
		t.Fatal("the first connection is no longer active")
	}
	if !first.Send(blocked.marked) {
		t.Fatal("could not queue the marked envelope")
	}
	select {
	case <-blocked.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the writer did not enter Send for the marked envelope")
	}
	pending := heartbeatEnvelope()
	for i := range sendQueue {
		if !first.Send(pending) {
			t.Fatalf("Send rejected pending envelope %d of %d", i+1, sendQueue)
		}
	}
	if first.Send(pending) {
		t.Fatal("Send on a full queue returned true")
	}
	select {
	case <-first.Done():
	default:
		t.Fatal("the first connection is not closed")
	}
	select {
	case err := <-blocked.result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("blocked Send returned %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the overflow did not cancel the blocked send")
	}

	_, nextCapability := h.wait(firstState+1, "capability report of the next channel", isCapabilityReport)
	h.wait(nextCapability+1, "state report of the next channel", isStateReport)
	if n := cp.Channels(); n != 2 {
		t.Fatalf("control plane saw %d channels, want 2", n)
	}
	select {
	case err := <-h.errc:
		t.Fatalf("the agent stopped after the overflow: %v", err)
	default:
	}
}

// TestChannelAckTimeoutStillReconnects checks that a missing heartbeat
// acknowledgement still ends the channel and starts the next one.
func TestChannelAckTimeoutStillReconnects(t *testing.T) {
	cp := testcp.New(t)
	st, id := enrollDevice(t, cp)
	cp.SetAck(false, true)
	h := startWith(t, cp, st, id, func(o *Options) {
		o.AckTimeout = 500 * time.Millisecond
	})
	h.wait(0, "capability report", func(e *ownsandv1.AgentEnvelope) bool { return e.GetCapabilityReport() != nil })
	deadline := time.Now().Add(10 * time.Second)
	for cp.Channels() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("the agent did not reconnect after the acknowledgement timeout")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// syncBuffer is a log sink that the agent goroutines and the test share.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestStalledAuthenticationBacksOffAndRetries reproduces a control plane that
// accepts the first Authenticate call and never answers it while the HTTP/2
// connection stays healthy. The attempt must end at the authentication
// deadline, so that channelLoop logs its back-off and the second attempt
// opens the channel. The channel that it opens outlives the deadline: only
// the unary authentication calls are bound.
func TestStalledAuthenticationBacksOffAndRetries(t *testing.T) {
	const authTimeout = 300 * time.Millisecond
	cp := testcp.New(t)
	st, id := enrollDevice(t, cp)
	cp.StallNextAuthenticate()
	logs := &syncBuffer{}
	h := startWith(t, cp, st, id, func(o *Options) {
		o.Control.AuthTimeout = authTimeout
		o.Logger = slog.New(slog.NewJSONHandler(logs, nil))
	})

	h.wait(0, "capability report of the channel after the stalled attempt", func(e *ownsandv1.AgentEnvelope) bool { return e.GetCapabilityReport() != nil })
	out := logs.String()
	for _, want := range []string{`"msg":"authentication failed"`, `"msg":"agent channel closed; reconnecting"`, `"retry_in_ms"`, "no answer within"} {
		if !strings.Contains(out, want) {
			t.Errorf("agent log lacks %q:\n%s", want, out)
		}
	}
	if n := cp.Channels(); n != 1 {
		t.Fatalf("control plane saw %d channels, want 1", n)
	}

	// The channel context has no deadline: the channel is still the same
	// one long after the authentication deadline.
	time.Sleep(3 * authTimeout)
	if n := cp.Channels(); n != 1 {
		t.Fatalf("control plane saw %d channels after the authentication deadline, want 1", n)
	}
	select {
	case err := <-h.errc:
		t.Fatalf("the agent stopped: %v", err)
	default:
	}
}

// newURLRequestAgent builds an agent with a live channel connection and no
// store or runtime, enough for RequestURLs. The returned queue receives the
// signed URL request.
func newURLRequestAgent() (*Agent, chan *ownsandv1.AgentEnvelope) {
	queue := make(chan *ownsandv1.AgentEnvelope, 4)
	a := &Agent{
		urlWaiters: map[string]chan *ownsandv1.SignedUrlGrant{},
		conn: &conn{
			queue:  queue,
			done:   make(chan struct{}),
			logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		},
	}
	return a, queue
}

func (a *Agent) waiterCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.urlWaiters)
}

func TestRequestURLsReportsParentEnd(t *testing.T) {
	canceledWithCause := errors.New("shutting down")
	cases := []struct {
		name    string
		parent  func() (context.Context, context.CancelFunc)
		wantIs  []error
		wantNot []error
	}{
		{
			name: "canceled",
			parent: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx, func() {}
			},
			wantIs:  []error{context.Canceled},
			wantNot: []error{errURLGrantTimeout, context.DeadlineExceeded},
		},
		{
			name: "canceled with cause",
			parent: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancelCause(context.Background())
				cancel(canceledWithCause)
				return ctx, func() {}
			},
			wantIs:  []error{context.Canceled, canceledWithCause},
			wantNot: []error{errURLGrantTimeout},
		},
		{
			name: "parent deadline before the grant timeout",
			parent: func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), 20*time.Millisecond)
			},
			wantIs:  []error{context.DeadlineExceeded},
			wantNot: []error{errURLGrantTimeout, context.Canceled},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, queue := newURLRequestAgent()
			ctx, cancel := tc.parent()
			defer cancel()
			_, err := a.RequestURLs(ctx, "snap", ownsandv1.SignedUrlOperation_SIGNED_URL_OPERATION_DOWNLOAD, []int32{1})
			if err == nil {
				t.Fatal("RequestURLs returned no error")
			}
			for _, want := range tc.wantIs {
				if !errors.Is(err, want) {
					t.Errorf("error %q does not wrap %v", err, want)
				}
			}
			for _, not := range tc.wantNot {
				if errors.Is(err, not) {
					t.Errorf("error %q wraps %v", err, not)
				}
			}
			if len(queue) != 1 {
				t.Errorf("sent %d requests, want 1", len(queue))
			}
			if n := a.waiterCount(); n != 0 {
				t.Errorf("%d URL waiters left behind", n)
			}
		})
	}
}

func TestRequestURLsGrantTimeoutIsDistinct(t *testing.T) {
	a, _ := newURLRequestAgent()
	a.urlGrantTimeout = 20 * time.Millisecond
	_, err := a.RequestURLs(context.Background(), "snap", ownsandv1.SignedUrlOperation_SIGNED_URL_OPERATION_DOWNLOAD, []int32{1})
	if !errors.Is(err, errURLGrantTimeout) {
		t.Fatalf("error = %v, want the grant timeout", err)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("grant timeout %q wraps a context error", err)
	}
	if n := a.waiterCount(); n != 0 {
		t.Errorf("%d URL waiters left behind", n)
	}
}

func TestRequestURLsGrantAndRejectionRemoveWaiter(t *testing.T) {
	for _, reject := range []bool{false, true} {
		a, queue := newURLRequestAgent()
		go func() {
			env := <-queue
			req := env.GetSignedUrlRequest()
			g := &ownsandv1.SignedUrlGrant{RequestId: req.GetRequestId()}
			if reject {
				g.Error = &ownsandv1.ErrorDetail{Message: "no"}
			} else {
				g.PartUrls = []*ownsandv1.SignedPartUrl{{PartNumber: 1}}
			}
			a.deliverURLs(g)
		}()
		urls, err := a.RequestURLs(context.Background(), "snap", ownsandv1.SignedUrlOperation_SIGNED_URL_OPERATION_DOWNLOAD, []int32{1})
		if reject {
			if err == nil {
				t.Error("rejected request returned no error")
			}
		} else if err != nil || len(urls) != 1 {
			t.Errorf("urls = %v, err = %v", urls, err)
		}
		if n := a.waiterCount(); n != 0 {
			t.Errorf("reject=%v: %d URL waiters left behind", reject, n)
		}
	}
}

func TestRequestURLsWithoutChannelRemovesWaiter(t *testing.T) {
	a := &Agent{urlWaiters: map[string]chan *ownsandv1.SignedUrlGrant{}}
	if _, err := a.RequestURLs(context.Background(), "snap", ownsandv1.SignedUrlOperation_SIGNED_URL_OPERATION_DOWNLOAD, []int32{1}); err == nil {
		t.Fatal("RequestURLs returned no error without a channel")
	}
	if n := a.waiterCount(); n != 0 {
		t.Errorf("%d URL waiters left behind", n)
	}
}

type capabilityProbeRuntime struct {
	vmruntime.Runtime
	probe func(context.Context) vmruntime.Capabilities
}

func (r capabilityProbeRuntime) Capabilities(ctx context.Context) vmruntime.Capabilities {
	return r.probe(ctx)
}

func macOSCapabilities(model string) vmruntime.Capabilities {
	return vmruntime.Capabilities{
		GuestOSes:            []ownsandv1.GuestOs{ownsandv1.GuestOs_GUEST_OS_MACOS},
		BaseImages:           []ownsandv1.BaseImage{ownsandv1.BaseImage_BASE_IMAGE_MACOS},
		MacOSGuestsSupported: true,
		MacOSHardwareModels:  [][]byte{[]byte(model)},
	}
}

func TestHeartbeatRetriesDeferredCapabilities(t *testing.T) {
	cp := testcp.New(t)
	st, id := enrollDevice(t, cp)
	var free atomic.Int64
	var sample atomic.Pointer[vmruntime.Capabilities]
	var deferrals atomic.Int32
	initial := macOSCapabilities("previous")
	sample.Store(&initial)
	h := startWith(t, cp, st, id, func(o *Options) {
		o.FreeDisk = func() (int64, error) { return free.Load(), nil }
		o.Runtime = capabilityProbeRuntime{Runtime: o.Runtime, probe: func(context.Context) vmruntime.Capabilities {
			caps := *sample.Load()
			if caps.Deferred {
				deferrals.Add(1)
			}
			return caps
		}}
	})

	isCapability := func(e *ownsandv1.AgentEnvelope) bool { return e.GetCapabilityReport() != nil }
	isState := func(e *ownsandv1.AgentEnvelope) bool { return e.GetStateReport() != nil }
	isHeartbeat := func(e *ownsandv1.AgentEnvelope) bool { return e.GetHeartbeat() != nil }
	previous, first := h.wait(0, "initial capability report", isCapability)
	assertMacOSCapabilityModel(t, previous, []byte("previous"))
	_, state := h.wait(first+1, "initial state report", isState)
	var oldStatus Status
	if err := st.LoadStatus(&oldStatus); err != nil {
		t.Fatal(err)
	}

	sample.Store(&vmruntime.Capabilities{Deferred: true})
	free.Store(capabilityDiskDelta + 1)
	waitForCapabilityDeferrals(t, &deferrals, 2)
	h.wait(state+1, "heartbeat while capability refresh is deferred", isHeartbeat)
	var status Status
	if err := st.LoadStatus(&status); err != nil {
		t.Fatal(err)
	}
	if !status.CapabilitiesAt.Equal(oldStatus.CapabilitiesAt) ||
		!slices.Equal(status.BaseImages, []string{ownsandv1.BaseImage_BASE_IMAGE_MACOS.String()}) {
		t.Fatalf("capability status changed under contention: %+v", status)
	}
	if got, _ := cp.WaitFor(0, first+1, isCapability); got != nil {
		t.Fatalf("published a capability report while deferred: %v", got)
	}

	// The original disk trigger clears before publication ends. The deferred
	// refresh must still be due on the next heartbeat.
	free.Store(0)
	waitForCapabilityDeferrals(t, &deferrals, deferrals.Load()+2)
	replacement := macOSCapabilities("replacement")
	sample.Store(&replacement)
	report, next := h.wait(first+1, "replacement capability report", isCapability)
	assertMacOSCapabilityModel(t, report, []byte("replacement"))
	if got := report.GetCapabilityReport().GetCapabilities().GetFreeDiskBytes(); got != 0 {
		t.Fatalf("replacement report has %d free bytes, want 0", got)
	}
	if cp.Channels() != 1 {
		t.Fatalf("heartbeat refresh reconnected %d times", cp.Channels()-1)
	}

	sample.Store(&vmruntime.Capabilities{})
	free.Store(capabilityDiskDelta + 1)
	absent, _ := h.wait(next+1, "authoritative report with no image", isCapability)
	if caps := absent.GetCapabilityReport().GetCapabilities(); len(caps.GetBuiltBaseImages()) != 0 || len(caps.GetMacosHardwareModels()) != 0 {
		t.Fatalf("authoritative report retained the previous image: %v", caps)
	}
	status = Status{}
	if err := st.LoadStatus(&status); err != nil || len(status.BaseImages) != 0 {
		t.Fatalf("authoritative report did not clear the local status: %+v, %v", status, err)
	}
}

func TestChannelDefersInitialCapabilities(t *testing.T) {
	for _, stop := range []bool{false, true} {
		name := "resample"
		if stop {
			name = "cancel"
		}
		t.Run(name, func(t *testing.T) {
			cp := testcp.New(t)
			st, id := enrollDevice(t, cp)
			var ready atomic.Bool
			var complete atomic.Int32
			probes := make(chan struct{}, 2)
			h := startWith(t, cp, st, id, func(o *Options) {
				o.HeartbeatInterval = time.Hour
				o.Runtime = capabilityProbeRuntime{Runtime: o.Runtime, probe: func(context.Context) vmruntime.Capabilities {
					if !ready.Load() {
						select {
						case probes <- struct{}{}:
						default:
						}
						return vmruntime.Capabilities{Deferred: true}
					}
					if complete.Add(1) == 1 {
						return macOSCapabilities("replacement")
					}
					return vmruntime.Capabilities{}
				}}
			})
			for range 2 {
				select {
				case <-probes:
				case <-time.After(10 * time.Second):
					t.Fatal("initial capability sampling did not retry")
				}
			}
			if cp.Received() != 0 {
				t.Fatalf("received %d channel messages before authoritative sample", cp.Received())
			}
			if stop {
				h.stop()
				select {
				case err := <-h.errc:
					if err != nil {
						t.Fatalf("cancel deferred setup: %v", err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("deferred setup did not stop")
				}
				if cp.Received() != 0 {
					t.Fatalf("cancelled setup sent %d channel messages", cp.Received())
				}
				return
			}
			ready.Store(true)
			report, index := h.wait(0, "initial capability report", func(e *ownsandv1.AgentEnvelope) bool {
				return e.GetCapabilityReport() != nil
			})
			assertMacOSCapabilityModel(t, report, []byte("replacement"))
			state, _ := h.wait(index+1, "initial state report", func(e *ownsandv1.AgentEnvelope) bool {
				return e.GetStateReport() != nil
			})
			if got := state.GetStateReport().GetBuiltBaseImages(); !slices.Equal(got, report.GetCapabilityReport().GetCapabilities().GetBuiltBaseImages()) {
				t.Fatalf("state report images = %v, want the same images as the capability report", got)
			}
			if n := complete.Load(); n != 1 {
				t.Fatalf("channel setup used %d authoritative samples, want 1", n)
			}
		})
	}
}

func assertMacOSCapabilityModel(t *testing.T, env *ownsandv1.AgentEnvelope, model []byte) {
	t.Helper()
	caps := env.GetCapabilityReport().GetCapabilities()
	models := caps.GetMacosHardwareModels()
	if !slices.Contains(caps.GetBuiltBaseImages(), ownsandv1.BaseImage_BASE_IMAGE_MACOS) ||
		len(models) != 1 || !bytes.Equal(models[0], model) {
		t.Fatalf("capability report base images = %v, model count = %d, want a macOS image with the expected model", caps.GetBuiltBaseImages(), len(models))
	}
}

func waitForCapabilityDeferrals(t *testing.T, deferred *atomic.Int32, want int32) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for deferred.Load() < want {
		if time.Now().After(deadline) {
			t.Fatalf("observed %d capability deferrals, want at least %d", deferred.Load(), want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
