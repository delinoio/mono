// SPDX-License-Identifier: FSL-1.1-ALv2

package relay

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/packages/ownsand-sdk-go/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
	"github.com/delinoio/mono/servers/ownsand-api/internal/auth"
	"github.com/delinoio/mono/servers/ownsand-api/internal/rpc"
)

// maxQueuedFrames bounds the frames queued toward one caller. Payload frames
// are also bounded by the credit window; this bound stops an agent that
// floods frames that use no credit.
const maxQueuedFrames = 4096

// byteTracker records when the last byte and the last complete frame of an
// agent relay connection arrived, so that the 90-second idle rule counts any
// byte and the acceptance timer pauses while a frame is in transit.
type byteTracker struct {
	lastByte  atomic.Int64
	lastFrame atomic.Int64
}

func (t *byteTracker) inTransit() bool { return t.lastByte.Load() > t.lastFrame.Load() }

type countingBody struct {
	io.ReadCloser
	t *byteTracker
}

func (b countingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.t.lastByte.Store(time.Now().UnixNano())
	}
	return n, err
}

type trackerKey struct{}
type controllerKey struct{}

// Middleware wraps the HTTP handler: it counts the received bytes of agent
// relay connections, and gives relay RPCs a response controller (for the
// stalled-reader write deadline) and a read deadline for the request upload.
func (m *Manager) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == ownsandv1connect.RelayAgentServiceConnectProcedure:
			t := &byteTracker{}
			now := time.Now().UnixNano()
			t.lastByte.Store(now)
			t.lastFrame.Store(now)
			r.Body = countingBody{r.Body, t}
			r = r.WithContext(context.WithValue(r.Context(), trackerKey{}, t))
		case strings.HasPrefix(r.URL.Path, "/"+ownsandv1connect.RelayServiceName+"/"):
			rc := http.NewResponseController(w)
			_ = rc.SetReadDeadline(time.Now().Add(m.Limits.RequestDeadline))
			r = r.WithContext(context.WithValue(r.Context(), controllerKey{}, rc))
		}
		next.ServeHTTP(w, r)
	})
}

// agentConn is one agent relay connection.
type agentConn struct {
	m       *Manager
	device  string
	gen     uint64
	tracker *byteTracker
	out     chan *ownsandv1.DataFrame
	done    chan struct{}
	once    sync.Once
	reason  atomic.Value

	mu        sync.Mutex
	streams   map[uint64]*relayStream
	nextID    uint64
	peerKinds map[ownsandv1.DataFrameKind]bool
	lastSend  atomic.Int64
}

func (c *agentConn) close(reason string) {
	c.once.Do(func() {
		c.reason.Store(reason)
		close(c.done)
	})
}

// send queues a frame toward the agent. A full queue means the agent stopped
// reading; the connection is closed.
func (c *agentConn) send(f *ownsandv1.DataFrame) bool {
	select {
	case <-c.done:
		return false
	case c.out <- f:
		return true
	default:
		c.close("send queue full")
		return false
	}
}

// AgentRelayService implements ownsandv1connect.RelayAgentServiceHandler.
type AgentRelayService struct{ M *Manager }

var _ ownsandv1connect.RelayAgentServiceHandler = AgentRelayService{}

// Connect registers the agent relay connection of the credential's device,
// bound to its current agent channel generation.
func (s AgentRelayService) Connect(ctx context.Context, stream *connect.BidiStream[ownsandv1.DataFrame, ownsandv1.DataFrame]) error {
	m := s.M
	p, ok := auth.FromContext(ctx)
	if !ok || p.Kind != ownsandv1.PrincipalKind_PRINCIPAL_KIND_DEVICE {
		return auth.ErrMissingCredential
	}
	rpc.SetLogOrganization(ctx, p.OrganizationID)
	ch, ok := m.Hub.Current(p.ID)
	if !ok || !m.Hub.Ready() {
		return connect.NewError(connect.CodeUnavailable, errors.New("the device has no current agent channel on this instance"))
	}
	t, _ := ctx.Value(trackerKey{}).(*byteTracker)
	if t == nil {
		t = &byteTracker{}
	}
	c := &agentConn{m: m, device: p.ID, gen: ch.Generation, tracker: t, out: make(chan *ownsandv1.DataFrame, m.Limits.MaxPendingFrames),
		done: make(chan struct{}), streams: map[uint64]*relayStream{}}
	m.mu.Lock()
	old := m.conns[c.device]
	m.conns[c.device] = c
	waiters := m.connWaiters[c.device]
	delete(m.connWaiters, c.device)
	m.mu.Unlock()
	for _, w := range waiters {
		close(w)
	}
	if old != nil {
		old.close("replaced by a new relay connection")
	}
	// The agent channel may have closed while registering.
	if cur, ok := m.Hub.Current(c.device); !ok || cur.Generation != c.gen {
		c.close("agent channel closed")
	}
	start := time.Now()
	m.Logger.Info("relay_connection_opened", slog.String("device_id", c.device), slog.Uint64("generation", c.gen), slog.Bool("replaced", old != nil))

	c.send(datapath.NewFrame(0, "", func(f *ownsandv1.DataFrame) {
		f.Payload = &ownsandv1.DataFrame_SupportedKinds{SupportedKinds: &ownsandv1.SupportedFrameKinds{Kinds: datapath.SupportedKinds()}}
	}))
	recvDone := make(chan error, 1)
	go func() {
		for {
			f, err := stream.Receive()
			if err != nil {
				recvDone <- err
				return
			}
			t.lastFrame.Store(time.Now().UnixNano())
			c.handle(f)
		}
	}()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	c.lastSend.Store(time.Now().UnixNano())
loop:
	for {
		select {
		case f := <-c.out:
			if err := stream.Send(f); err != nil {
				c.close("send failed")
				break loop
			}
			c.lastSend.Store(time.Now().UnixNano())
		case <-tick.C:
			now := time.Now()
			if now.Sub(time.Unix(0, t.lastByte.Load())) >= m.Limits.ConnectionReceiveIdle {
				c.close("receive idle")
				break loop
			}
			if now.Sub(time.Unix(0, c.lastSend.Load())) >= m.Limits.ConnectionKeepalive {
				c.send(keepalive(false))
			}
		case <-recvDone:
			c.close("agent closed the connection")
			break loop
		case <-c.done:
			break loop
		case <-ctx.Done():
			c.close("request ended")
			break loop
		}
	}
	m.mu.Lock()
	if m.conns[c.device] == c {
		delete(m.conns, c.device)
	}
	m.mu.Unlock()
	c.mu.Lock()
	streams := len(c.streams)
	for _, st := range c.streams {
		st.fail(connect.NewError(connect.CodeUnavailable, errors.New("the agent relay connection closed")))
	}
	c.streams = map[uint64]*relayStream{}
	c.mu.Unlock()
	reason, _ := c.reason.Load().(string)
	m.Logger.Info("relay_connection_closed", slog.String("device_id", c.device), slog.String("reason", reason),
		slog.Int("rpcs_ended", streams), slog.Duration("duration", time.Since(start)))
	return nil
}

func keepalive(echo bool) *ownsandv1.DataFrame {
	return datapath.NewFrame(0, "", func(f *ownsandv1.DataFrame) {
		f.Payload = &ownsandv1.DataFrame_Keepalive{Keepalive: &ownsandv1.FrameKeepalive{EchoRequested: echo}}
	})
}

func knownKind(k ownsandv1.DataFrameKind) bool {
	for _, x := range datapath.SupportedKinds() {
		if x == k {
			return true
		}
	}
	return false
}

// handle routes one frame from the agent. It never blocks.
func (c *agentConn) handle(f *ownsandv1.DataFrame) {
	if f.GetStreamId() == 0 {
		switch p := f.GetPayload().(type) {
		case *ownsandv1.DataFrame_Keepalive:
			if p.Keepalive.GetEchoRequested() {
				c.send(keepalive(false))
			}
		case *ownsandv1.DataFrame_SupportedKinds:
			kinds := map[ownsandv1.DataFrameKind]bool{}
			for _, k := range p.SupportedKinds.GetKinds() {
				kinds[k] = true
			}
			c.mu.Lock()
			c.peerKinds = kinds
			c.mu.Unlock()
		}
		// Unknown connection-level kinds are ignored.
		return
	}
	c.mu.Lock()
	st := c.streams[f.GetStreamId()]
	if st == nil {
		c.mu.Unlock()
		// Frames of a closed or unknown stream are dropped.
		return
	}
	kind := datapath.KindOf(f)
	if !knownKind(kind) || f.GetPayload() == nil {
		c.mu.Unlock()
		c.send(datapath.NewFrame(st.id, st.sandbox, func(x *ownsandv1.DataFrame) {
			x.Payload = &ownsandv1.DataFrame_Error{Error: &ownsandv1.FrameError{Code: connect.CodeUnimplemented.String(), Message: "unknown frame kind"}}
		}))
		c.endStream(st, connect.NewError(connect.CodeUnimplemented, errors.New("the agent sent an unknown frame kind")))
		return
	}
	if w, ok := f.GetPayload().(*ownsandv1.DataFrame_RelayWindowUpdate); ok {
		st.credit += w.RelayWindowUpdate.GetAddedBytes()
		c.mu.Unlock()
		st.notify()
		return
	}
	if datapath.PayloadUsesCredit(f) {
		size := datapath.FrameSize(f)
		if size > st.window {
			c.mu.Unlock()
			// Sending beyond the window breaks the protocol.
			c.close("agent exceeded its credit window")
			return
		}
		st.window -= size
	}
	if len(st.queue) >= maxQueuedFrames {
		c.mu.Unlock()
		c.close("agent flooded a stream")
		return
	}
	st.queue = append(st.queue, f)
	c.mu.Unlock()
	st.notify()
}

// endStream removes a stream that the agent ended.
func (c *agentConn) endStream(st *relayStream, err error) {
	c.mu.Lock()
	delete(c.streams, st.id)
	c.mu.Unlock()
	st.fail(err)
}

// waitConn waits up to the connection wait for an agent relay connection of
// the device bound to generation gen.
func (m *Manager) waitConn(ctx context.Context, device string, gen uint64) (*agentConn, error) {
	deadline := time.NewTimer(m.Limits.ConnectionWait)
	defer deadline.Stop()
	for {
		m.mu.Lock()
		c := m.conns[device]
		if c != nil && c.gen == gen {
			select {
			case <-c.done:
			default:
				m.mu.Unlock()
				return c, nil
			}
		}
		w := make(chan struct{})
		m.connWaiters[device] = append(m.connWaiters[device], w)
		m.mu.Unlock()
		select {
		case <-w:
		case <-deadline.C:
			return nil, connect.NewError(connect.CodeUnavailable, errors.New("the device has no relay connection"))
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}
