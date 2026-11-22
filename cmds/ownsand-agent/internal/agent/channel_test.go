// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build linux || darwin

package agent

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/testcp"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
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

// TestChannelSendQueueOverflowEndsBlockedStream reproduces a control plane
// that stops reading the channel: the HTTP/2 flow-control window fills up, the
// writer blocks inside stream.Send, and the send queue fills behind it. One
// more envelope must end the stream at once, not after the acknowledgement
// timeout, and the next channel must report capabilities and full state.
func TestChannelSendQueueOverflowEndsBlockedStream(t *testing.T) {
	cp := testcp.New(t)
	st, id := enrollDevice(t, cp)
	cp.StallNextChannelReads()
	h := startWith(t, cp, st, id, func(o *Options) {
		// Only a send queue overflow may end the first channel in this test.
		o.HeartbeatInterval = time.Hour
		o.AckTimeout = time.Hour
	})

	var first *conn
	deadline := time.Now().Add(10 * time.Second)
	for first == nil {
		if time.Now().After(deadline) {
			t.Fatal("the agent did not open a channel")
		}
		h.agent.mu.Lock()
		first = h.agent.conn
		h.agent.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}

	big := &ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_CommandResult{CommandResult: &ownsandv1.AgentCommandResult{
		CommandId: "stall",
		Error:     &ownsandv1.ErrorDetail{Message: strings.Repeat("x", 64<<10)},
	}}}
	accepted := 0
	for first.Send(big) {
		accepted++
		if accepted > 10*sendQueue {
			t.Fatal("the send queue never filled up")
		}
		if accepted%64 == 0 {
			// Give the writer time to block in the stalled stream.
			time.Sleep(time.Millisecond)
		}
	}
	if accepted <= sendQueue {
		t.Fatalf("accepted %d envelopes, want more than the queue size %d (the writer must have taken some before blocking)", accepted, sendQueue)
	}
	t.Logf("accepted %d envelopes before the overflow", accepted)

	// The second channel reports capabilities and full state, long before
	// the one-hour acknowledgement timeout.
	h.wait(0, "capability report of the next channel", func(e *ownsandv1.AgentEnvelope) bool { return e.GetCapabilityReport() != nil })
	h.wait(0, "state report of the next channel", func(e *ownsandv1.AgentEnvelope) bool { return e.GetStateReport() != nil })
	if n := cp.Channels(); n != 2 {
		t.Fatalf("control plane saw %d channels, want 2", n)
	}
	select {
	case <-first.Done():
	default:
		t.Fatal("the first connection is not closed")
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
	h := start(t, cp, st, id)
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
