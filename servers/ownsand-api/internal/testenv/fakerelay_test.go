// SPDX-License-Identifier: FSL-1.1-ALv2

package testenv

import (
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/delinoio/mono/go/ownsand/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// newObserveRelay returns a FakeRelay that only has the bookkeeping state, so
// tests drive observe without a Connect stream.
func newObserveRelay() *FakeRelay {
	return &FakeRelay{
		changed:  make(chan struct{}),
		streams:  map[uint64]*RelayStream{},
		received: map[uint64]bool{},
	}
}

// feed passes one frame through observe, like read does, and reports whether
// the frame would dispatch OnRequest.
func feed(r *FakeRelay, streamID uint64, payload proto.Message) (dispatched bool) {
	f := datapath.MustNewFrame(streamID, "sandbox", payload)
	_, _, first, ch := r.observe(f)
	close(ch)
	return first
}

func hasBookkeeping(r *FakeRelay, id uint64) (stream, received bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, stream = r.streams[id]
	_, received = r.received[id]
	return stream, received
}

func TestObserveDispatchesTheFirstRequestOfAnActiveStreamOnce(t *testing.T) {
	r := newObserveRelay()
	feed(r, 1, &ownsandv1.RelayStreamOpen{GrantId: "grant"})
	if !feed(r, 1, &ownsandv1.ExecRequest{}) {
		t.Fatal("first request of an active stream was not dispatched")
	}
	if feed(r, 1, &ownsandv1.ExecRequest{}) {
		t.Fatal("second request of the stream was dispatched")
	}
}

func TestObserveRetiresStreamOnTerminalFrame(t *testing.T) {
	for _, tc := range []struct {
		name     string
		terminal proto.Message
	}{
		{"close", &ownsandv1.RelayStreamClose{}},
		{"error", &ownsandv1.ErrorDetail{Message: "failed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newObserveRelay()
			feed(r, 1, &ownsandv1.RelayStreamOpen{GrantId: "grant"})
			if stream, _ := hasBookkeeping(r, 1); !stream {
				t.Fatal("stream was not registered on open")
			}
			feed(r, 1, tc.terminal)
			if stream, received := hasBookkeeping(r, 1); stream || received {
				t.Fatalf("after the terminal frame: streams entry = %v, received entry = %v, want neither", stream, received)
			}
			if feed(r, 1, &ownsandv1.ExecRequest{}) {
				t.Fatal("a first request after the terminal frame was dispatched")
			}
			if stream, received := hasBookkeeping(r, 1); stream || received {
				t.Fatalf("request after the terminal frame recreated bookkeeping: streams = %v, received = %v", stream, received)
			}
			if got := len(r.Frames()); got != 3 {
				t.Fatalf("history has %d frames, want 3 including the terminal frame", got)
			}
			if !proto.Equal(r.Frames()[1], datapath.MustNewFrame(1, "sandbox", tc.terminal)) {
				t.Fatal("history does not hold the terminal frame at its position")
			}
		})
	}
}

func TestObserveRetiresStreamAfterItsFirstRequest(t *testing.T) {
	r := newObserveRelay()
	feed(r, 1, &ownsandv1.RelayStreamOpen{GrantId: "grant"})
	if !feed(r, 1, &ownsandv1.ExecRequest{}) {
		t.Fatal("first request was not dispatched")
	}
	if _, received := hasBookkeeping(r, 1); !received {
		t.Fatal("first request was not recorded")
	}
	feed(r, 1, &ownsandv1.RelayStreamClose{})
	if stream, received := hasBookkeeping(r, 1); stream || received {
		t.Fatalf("after close: streams = %v, received = %v, want neither", stream, received)
	}
}

func TestObserveTerminalFrameLeavesOtherStreamsActive(t *testing.T) {
	r := newObserveRelay()
	feed(r, 1, &ownsandv1.RelayStreamOpen{GrantId: "one"})
	feed(r, 2, &ownsandv1.RelayStreamOpen{GrantId: "two"})
	feed(r, 1, &ownsandv1.RelayStreamClose{})
	if feed(r, 1, &ownsandv1.ExecRequest{}) {
		t.Fatal("request on the closed stream was dispatched")
	}
	if !feed(r, 2, &ownsandv1.ExecRequest{}) {
		t.Fatal("request on the other stream was not dispatched")
	}
	if feed(r, 2, &ownsandv1.ExecRequest{}) {
		t.Fatal("other stream dispatched its first request twice")
	}
	if stream, _ := hasBookkeeping(r, 2); !stream {
		t.Fatal("closing stream 1 retired stream 2")
	}
}
