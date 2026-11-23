// SPDX-License-Identifier: FSL-1.1-ALv2

package testenv

import (
	"context"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	"github.com/delinoio/mono/go/ownsand/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// RelayStream is one relay stream that the fake agent serves.
type RelayStream struct {
	ID        uint64
	GrantID   string
	SandboxID string
	relay     *FakeRelay
}

// Send sends a payload on the stream.
func (s *RelayStream) Send(payload proto.Message) {
	s.relay.SendFrame(datapath.MustNewFrame(s.ID, s.SandboxID, payload))
}

// FakeRelay is the fake agent's RelayAgentService.Connect connection.
type FakeRelay struct {
	agent  *FakeAgent
	stream *connect.BidiStreamForClient[ownsandv1.ConnectRequest, ownsandv1.ConnectResponse]
	cancel context.CancelFunc
	sendMu sync.Mutex
	// Window is the credit the fake agent grants per stream toward itself.
	Window int64
	// OnRequest serves the first request frame of a stream.
	OnRequest func(s *RelayStream, f *ownsandv1.DataFrame)

	mu       sync.Mutex
	paused   chan struct{}
	frames   []*ownsandv1.DataFrame
	changed  chan struct{}
	closed   chan struct{}
	streams  map[uint64]*RelayStream
	received map[uint64]bool
}

// ConnectRelay opens the relay connection with a fresh device session
// credential.
func (a *FakeAgent) ConnectRelay(onRequest func(s *RelayStream, f *ownsandv1.DataFrame)) *FakeRelay {
	a.T.Helper()
	cred := a.Env.SessionCredential(a.Device)
	ctx, cancel := context.WithCancel(context.Background())
	r := &FakeRelay{agent: a, cancel: cancel, Window: 8 << 20, OnRequest: onRequest, changed: make(chan struct{}), closed: make(chan struct{}),
		streams: map[uint64]*RelayStream{}, received: map[uint64]bool{}}
	r.stream = a.Env.Clients(cred).RelayAgent.Connect(ctx)
	r.SendFrame(datapath.MustNewFrame(0, "", &ownsandv1.RelayKeepalive{}))
	go r.read()
	a.T.Cleanup(r.Close)
	return r
}

// SetOnRequest replaces the request hook while the connection runs.
func (r *FakeRelay) SetOnRequest(fn func(s *RelayStream, f *ownsandv1.DataFrame)) {
	r.mu.Lock()
	r.OnRequest = fn
	r.mu.Unlock()
}

// SendFrame sends one frame.
func (r *FakeRelay) SendFrame(f *ownsandv1.DataFrame) {
	r.sendMu.Lock()
	defer r.sendMu.Unlock()
	_ = r.stream.Send(&ownsandv1.ConnectRequest{Frame: f})
}

// Close closes the connection.
func (r *FakeRelay) Close() {
	r.ResumeReads()
	r.cancel()
	r.sendMu.Lock()
	_ = r.stream.CloseRequest()
	r.sendMu.Unlock()
	_ = r.stream.CloseResponse()
	select {
	case <-r.closed:
	case <-time.After(5 * time.Second):
	}
}

// Closed is closed when the connection ends.
func (r *FakeRelay) Closed() <-chan struct{} { return r.closed }

// PauseReads stops the fake agent from reading its connection after the frame
// it is reading now, like an agent that stopped draining its network path.
// The control plane's writer then blocks once the HTTP/2 flow control window
// of the stream is full. Sending is not affected. ResumeReads undoes it.
func (r *FakeRelay) PauseReads() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.paused == nil {
		r.paused = make(chan struct{})
	}
}

// ResumeReads lets the fake agent read its connection again.
func (r *FakeRelay) ResumeReads() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.paused != nil {
		close(r.paused)
		r.paused = nil
	}
}

// waitResumed blocks while reads are paused.
func (r *FakeRelay) waitResumed() {
	r.mu.Lock()
	gate := r.paused
	r.mu.Unlock()
	if gate != nil {
		<-gate
	}
}

func (r *FakeRelay) read() {
	defer close(r.closed)
	for {
		r.waitResumed()
		resp, err := r.stream.Receive()
		if err != nil {
			return
		}
		f := resp.GetFrame()
		r.mu.Lock()
		r.frames = append(r.frames, f)
		ch := r.changed
		r.changed = make(chan struct{})
		s := r.streams[f.GetStreamId()]
		first := false
		if f.GetRelayStreamOpen() != nil {
			s = &RelayStream{ID: f.GetStreamId(), GrantID: f.GetRelayStreamOpen().GetGrantId(), SandboxID: f.GetSandboxId(), relay: r}
			r.streams[s.ID] = s
		} else if s != nil && datapath.IsRequestKind(f.GetKind()) && !r.received[s.ID] {
			r.received[s.ID] = true
			first = true
		}
		r.mu.Unlock()
		close(ch)
		switch {
		case f.GetRelayStreamOpen() != nil:
			r.SendFrame(datapath.MustNewFrame(s.ID, s.SandboxID, &ownsandv1.RelayWindowUpdate{IncrementBytes: r.Window}))
		case first:
			r.mu.Lock()
			hook := r.OnRequest
			r.mu.Unlock()
			if hook != nil {
				go hook(s, f)
			}
		}
	}
}

// Frames returns the frames received so far.
func (r *FakeRelay) Frames() []*ownsandv1.DataFrame {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*ownsandv1.DataFrame(nil), r.frames...)
}

// WaitFrame waits up to 10 seconds for a matching frame.
func (r *FakeRelay) WaitFrame(what string, match func(*ownsandv1.DataFrame) bool) *ownsandv1.DataFrame {
	t := r.agent.T
	t.Helper()
	deadline := time.After(10 * time.Second)
	seen := 0
	for {
		r.mu.Lock()
		frames := r.frames[seen:]
		ch := r.changed
		r.mu.Unlock()
		for _, f := range frames {
			seen++
			if match(f) {
				return f
			}
		}
		select {
		case <-ch:
		case <-deadline:
			t.Fatalf("timed out waiting for relay frame %s", what)
			return nil
		}
	}
}
