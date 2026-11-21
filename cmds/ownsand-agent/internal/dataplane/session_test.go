// SPDX-License-Identifier: Apache-2.0

//go:build linux || darwin

package dataplane

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/delinoio/mono/go/ownsand/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// loopbackOnly keeps ICE on the loopback interface, so tests do not wait
// for connectivity checks on every host interface.
func loopbackOnly(name string) bool { return name == "lo" || name == "lo0" }

// caller is a pion offerer like the SDK: one ordered, reliable data
// channel, non-trickle.
type caller struct {
	t      *testing.T
	pc     *webrtc.PeerConnection
	dc     *webrtc.DataChannel
	frames chan *ownsandv1.DataFrame
	open   chan struct{}
	closed chan struct{}
}

func newCaller(t *testing.T, label string) (*caller, string) {
	t.Helper()
	var se webrtc.SettingEngine
	se.SetIncludeLoopbackCandidate(true)
	se.SetInterfaceFilter(loopbackOnly)
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	pc, err := webrtc.NewAPI(webrtc.WithSettingEngine(se)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	ordered := true
	dc, err := pc.CreateDataChannel(label, &webrtc.DataChannelInit{Ordered: &ordered})
	if err != nil {
		t.Fatal(err)
	}
	c := &caller{t: t, pc: pc, dc: dc, frames: make(chan *ownsandv1.DataFrame, 1024), open: make(chan struct{}), closed: make(chan struct{})}
	dec := datapath.NewDecoder()
	dc.OnOpen(func() { close(c.open) })
	dc.OnClose(func() { close(c.closed) })
	dc.OnMessage(func(m webrtc.DataChannelMessage) {
		fs, err := dec.Push(m.Data)
		if err != nil {
			t.Errorf("decode: %v", err)
		}
		for _, f := range fs {
			c.frames <- f
		}
	})
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	<-gathered
	return c, pc.LocalDescription().SDP
}

func (c *caller) accept(answer *ownsandv1.SignalingAnswer) {
	c.t.Helper()
	if answer.GetError() != nil {
		c.t.Fatalf("answer error %v", answer.GetError())
	}
	if err := c.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer.GetSdp()}); err != nil {
		c.t.Fatal(err)
	}
	select {
	case <-c.open:
	case <-time.After(15 * time.Second):
		c.t.Fatal("data channel did not open")
	}
}

func (c *caller) send(f *ownsandv1.DataFrame) {
	msgs, err := datapath.EncodeMessages(f)
	if err != nil {
		c.t.Fatal(err)
	}
	for _, m := range msgs {
		if err := c.dc.Send(m); err != nil {
			c.t.Fatal(err)
		}
	}
}

func (c *caller) next() *ownsandv1.DataFrame {
	c.t.Helper()
	select {
	case f := <-c.frames:
		return f
	case <-time.After(20 * time.Second):
		c.t.Fatal("no frame from the agent")
	}
	return nil
}

func (c *caller) waitClosed() {
	c.t.Helper()
	select {
	case <-c.closed:
	case <-time.After(15 * time.Second):
		c.t.Fatal("data channel did not close")
	}
}

func (w *world) session(t *testing.T, dp *DataPlane, ch *fakeChannel, expiresIn time.Duration) (*caller, string) {
	c, sdp := newCaller(t, DataChannelLabel)
	id := newID(t)
	c.accept(dp.Offer(context.Background(), ch, &ownsandv1.SignalingOffer{SessionId: id, SandboxId: w.sandbox, ExpiresIn: durationpb.New(expiresIn), Sdp: sdp}))
	return c, id
}

func newPlane(w *world, timing Timing) *DataPlane {
	dp := New(Options{Dispatcher: w.d, Sandboxes: w.m, Logger: logger(), Timing: timing, IncludeLoopback: true, InterfaceFilter: loopbackOnly})
	w.t.Cleanup(dp.Close)
	return dp
}

func TestSessionServesExec(t *testing.T) {
	w := newWorld(t)
	dp := newPlane(w, Timing{})
	ch := newChannel(1)
	dp.ChannelOpened(ch)
	c, _ := w.session(t, dp, ch, time.Hour)
	req := w.execReq("/bin/sh", "-c", "head -c 100000 /dev/zero | tr '\\0' a; printf done")
	c.send(datapath.MustNewFrame(5, w.sandbox, req))
	var out []byte
	for {
		f := c.next()
		if f.GetStreamId() != 5 {
			t.Fatalf("frame on stream %d", f.GetStreamId())
		}
		ev := f.GetExecEvent()
		out = append(out, ev.GetStdout().GetData()...)
		if ev.GetExited() != nil {
			break
		}
	}
	if len(out) != 100004 || string(out[len(out)-4:]) != "done" {
		t.Fatalf("stdout %d bytes", len(out))
	}
	// A frame for another sandbox is rejected.
	c.send(datapath.MustNewFrame(6, newID(t), w.execReq("/bin/true")))
	if f := c.next(); f.GetError().GetCode() != ownsandv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED {
		t.Fatalf("other sandbox %v", f)
	}
	// A non-request frame gets an error.
	c.send(datapath.MustNewFrame(7, w.sandbox, &ownsandv1.ExecEvent{}))
	if f := c.next(); f.GetError().GetCode() != ownsandv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT {
		t.Fatalf("non-request %v", f)
	}
}

func TestSessionEndRules(t *testing.T) {
	w := newWorld(t)
	cases := []struct {
		name   string
		reason ownsandv1.SessionEndReason
		end    func(dp *DataPlane, ch *fakeChannel, id string)
		expiry time.Duration
	}{
		{"revoked", ownsandv1.SessionEndReason_SESSION_END_REASON_REVOKED, func(dp *DataPlane, ch *fakeChannel, id string) {
			dp.Revoke(ch, &ownsandv1.SessionRevocation{RevocationId: "r", Target: &ownsandv1.SessionRevocation_SessionId{SessionId: id}})
		}, time.Hour},
		{"channel lost", ownsandv1.SessionEndReason_SESSION_END_REASON_AGENT_CHANNEL_LOST, func(dp *DataPlane, ch *fakeChannel, _ string) {
			dp.ChannelLost(ch)
		}, time.Hour},
		{"expired", ownsandv1.SessionEndReason_SESSION_END_REASON_EXPIRED, func(*DataPlane, *fakeChannel, string) {}, 2 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dp := newPlane(w, Timing{})
			ch := newChannel(1)
			dp.ChannelOpened(ch)
			c, id := w.session(t, dp, ch, tc.expiry)
			c.send(datapath.MustNewFrame(1, w.sandbox, w.execReq("/bin/sh", "-c", "sleep 30")))
			if f := c.next(); f.GetExecEvent().GetStarted() == nil {
				t.Fatalf("first frame %v", f)
			}
			tc.end(dp, ch, id)
			for {
				f := c.next()
				if f.GetExecEvent().GetKeepalive() != nil {
					continue
				}
				if f.GetSessionEnd().GetReason() != tc.reason || f.GetStreamId() != 1 {
					t.Fatalf("frame %v", f)
				}
				break
			}
			c.waitClosed()
		})
	}
}

func TestSessionIdleAndLabel(t *testing.T) {
	w := newWorld(t)
	dp := newPlane(w, Timing{SessionIdle: time.Second})
	ch := newChannel(1)
	dp.ChannelOpened(ch)
	c, _ := w.session(t, dp, ch, time.Hour)
	c.waitClosed()

	other, sdp := newCaller(t, "something-else")
	answer := dp.Offer(context.Background(), ch, &ownsandv1.SignalingOffer{SessionId: newID(t), SandboxId: w.sandbox, ExpiresIn: durationpb.New(time.Hour), Sdp: sdp})
	if err := other.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer.GetSdp()}); err != nil {
		t.Fatal(err)
	}
	other.waitClosed()

	// Offers for a sandbox that is not running are rejected.
	if a := dp.Offer(context.Background(), ch, &ownsandv1.SignalingOffer{SessionId: newID(t), SandboxId: newID(t), ExpiresIn: durationpb.New(time.Hour), Sdp: sdp}); a.GetError().GetCode() != ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION {
		t.Fatalf("unknown sandbox %v", a)
	}
}

// A revocation ends a session whose operation waits for a stalled data
// channel to drain: the end interrupts the waiting send instead of waiting
// for its lock.
func TestSessionEndInterruptsBackpressuredSend(t *testing.T) {
	w := newWorld(t)
	dp := newPlane(w, Timing{})
	ch := newChannel(1)
	dp.ChannelOpened(ch)
	c, id := w.session(t, dp, ch, time.Hour)
	// The caller stops reading, so the agent's send buffer fills up.
	gate := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	t.Cleanup(release)
	c.dc.OnMessage(func(webrtc.DataChannelMessage) { <-gate })
	c.send(datapath.MustNewFrame(1, w.sandbox, w.execReq("/bin/sh", "-c", "head -c 100000000 /dev/zero")))
	dp.mu.Lock()
	s := dp.sessions[id]
	dp.mu.Unlock()
	waitFor(t, "a stalled send", func() bool {
		s.mu.Lock()
		dc := s.dc
		s.mu.Unlock()
		return dc != nil && dc.BufferedAmount() > maxBuffered
	})
	done := make(chan struct{})
	go func() {
		dp.Revoke(ch, &ownsandv1.SessionRevocation{RevocationId: "r", Target: &ownsandv1.SessionRevocation_SessionId{SessionId: id}})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("revocation blocked behind a backpressured send")
	}
	dp.mu.Lock()
	_, kept := dp.sessions[id]
	dp.mu.Unlock()
	if kept {
		t.Fatal("revoked session kept")
	}
	release()
}

// silentSTUN is a loopback UDP socket that never answers, so the ICE
// gathering of an offer that uses it as STUN server waits for the STUN
// timeout of pion (5 seconds) instead of finishing at once. That keeps an
// offer in its gathering step for a test.
func silentSTUN(t *testing.T) string {
	t.Helper()
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return "stun:" + conn.LocalAddr().String()
}

// newGatheringPlane returns a data plane whose offers gather candidates
// until the STUN timeout.
func newGatheringPlane(w *world, stunURL string) *DataPlane {
	dp := New(Options{Dispatcher: w.d, Sandboxes: w.m, Logger: logger(), IncludeLoopback: true, InterfaceFilter: loopbackOnly})
	dp.Configure([]string{stunURL})
	w.t.Cleanup(dp.Close)
	return dp
}

// startOffer starts an offer on ch in a goroutine and returns its session ID
// and the channel of its answer.
func (w *world) startOffer(t *testing.T, dp *DataPlane, ch *fakeChannel) (*caller, string, <-chan *ownsandv1.SignalingAnswer) {
	t.Helper()
	c, sdp := newCaller(t, DataChannelLabel)
	id := newID(t)
	answers := make(chan *ownsandv1.SignalingAnswer, 1)
	go func() {
		answers <- dp.Offer(context.Background(), ch, &ownsandv1.SignalingOffer{SessionId: id, SandboxId: w.sandbox, ExpiresIn: durationpb.New(time.Hour), Sdp: sdp})
	}()
	return c, id, answers
}

// waitGathering waits until the offer of the session ID gathers candidates
// and returns its session.
func waitGathering(t *testing.T, dp *DataPlane, id string) *session {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		dp.mu.Lock()
		s := dp.sessions[id]
		dp.mu.Unlock()
		if s != nil && s.pc.ICEGatheringState() == webrtc.ICEGatheringStateGathering {
			return s
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("offer did not start to gather candidates")
	return nil
}

func (d *DataPlane) sessionCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.sessions)
}

func requireNoSession(t *testing.T, dp *DataPlane) {
	t.Helper()
	if n := dp.sessionCount(); n != 0 {
		t.Fatalf("%d sessions in the session map, want 0", n)
	}
}

// requireEndedAnswer fails unless the offer returns an error answer without
// an SDP body before the silent STUN server times out its gathering.
func requireEndedAnswer(t *testing.T, answers <-chan *ownsandv1.SignalingAnswer) {
	t.Helper()
	select {
	case a := <-answers:
		if a.GetError() == nil || a.GetSdp() != "" {
			t.Fatalf("answer %v, want an error answer without an SDP body", a)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("offer did not end before the gathering timeout")
	}
}

// A revocation that arrives while the offer gathers candidates ends the
// session with REVOKED and the offer returns an error answer.
func TestRevokeDuringOfferGathering(t *testing.T) {
	w := newWorld(t)
	dp := newGatheringPlane(w, silentSTUN(t))
	ch := newChannel(1)
	dp.ChannelOpened(ch)
	_, id, answers := w.startOffer(t, dp, ch)
	s := waitGathering(t, dp, id)
	dp.Revoke(ch, &ownsandv1.SessionRevocation{RevocationId: "r", Target: &ownsandv1.SessionRevocation_SessionId{SessionId: id}})
	requireEndedAnswer(t, answers)
	requireNoSession(t, dp)
	s.mu.Lock()
	reason := s.endReason
	s.mu.Unlock()
	if reason != ownsandv1.SessionEndReason_SESSION_END_REASON_REVOKED {
		t.Fatalf("end reason %v", reason)
	}
	if st := s.pc.ConnectionState(); st != webrtc.PeerConnectionStateClosed {
		t.Fatalf("peer connection state %v", st)
	}
}

// Close ends a session whose offer gathers candidates: none exists after
// Close returns.
func TestCloseDuringOfferGathering(t *testing.T) {
	w := newWorld(t)
	dp := newGatheringPlane(w, silentSTUN(t))
	ch := newChannel(1)
	dp.ChannelOpened(ch)
	_, id, answers := w.startOffer(t, dp, ch)
	waitGathering(t, dp, id)
	dp.Close()
	requireNoSession(t, dp)
	requireEndedAnswer(t, answers)
	requireNoSession(t, dp)
}

// A lost channel cancels its pending offer and closes its peer connection;
// an offer on a live channel continues and gets its answer.
func TestChannelLostDuringOfferGathering(t *testing.T) {
	w := newWorld(t)
	dp := newGatheringPlane(w, silentSTUN(t))
	lost, live := newChannel(1), newChannel(2)
	dp.ChannelOpened(lost)
	dp.ChannelOpened(live)
	_, lostID, lostAnswers := w.startOffer(t, dp, lost)
	liveCaller, liveID, liveAnswers := w.startOffer(t, dp, live)
	lostSession := waitGathering(t, dp, lostID)
	waitGathering(t, dp, liveID)
	lost.Close()
	dp.ChannelLost(lost)
	requireEndedAnswer(t, lostAnswers)
	dp.mu.Lock()
	_, lostKept := dp.sessions[lostID]
	_, liveKept := dp.sessions[liveID]
	dp.mu.Unlock()
	if lostKept || !liveKept {
		t.Fatalf("lost session in map: %v, live session in map: %v", lostKept, liveKept)
	}
	if st := lostSession.pc.ConnectionState(); st != webrtc.PeerConnectionStateClosed {
		t.Fatalf("peer connection state %v", st)
	}
	select {
	case a := <-liveAnswers:
		liveCaller.accept(a)
	case <-time.After(15 * time.Second):
		t.Fatal("offer on the live channel did not return")
	}
}

// An offer whose channel closes without a ChannelLost call ends too.
func TestChannelDoneDuringOfferGathering(t *testing.T) {
	w := newWorld(t)
	dp := newGatheringPlane(w, silentSTUN(t))
	ch := newChannel(1)
	dp.ChannelOpened(ch)
	_, id, answers := w.startOffer(t, dp, ch)
	waitGathering(t, dp, id)
	ch.Close()
	requireEndedAnswer(t, answers)
	requireNoSession(t, dp)
}

// A channel that closed before the offer started cancels it before it
// gathers.
func TestOfferOnClosedChannel(t *testing.T) {
	w := newWorld(t)
	dp := newPlane(w, Timing{})
	ch := newChannel(1)
	dp.ChannelOpened(ch)
	ch.Close()
	_, sdp := newCaller(t, DataChannelLabel)
	a := dp.Offer(context.Background(), ch, &ownsandv1.SignalingOffer{SessionId: newID(t), SandboxId: w.sandbox, ExpiresIn: durationpb.New(time.Hour), Sdp: sdp})
	if a.GetError() == nil || a.GetSdp() != "" {
		t.Fatalf("answer %v", a)
	}
	requireNoSession(t, dp)
}

// A failed answer leaves no session in the map.
func TestFailedOfferLeavesNoSession(t *testing.T) {
	w := newWorld(t)
	dp := newPlane(w, Timing{})
	ch := newChannel(1)
	dp.ChannelOpened(ch)
	a := dp.Offer(context.Background(), ch, &ownsandv1.SignalingOffer{SessionId: newID(t), SandboxId: w.sandbox, ExpiresIn: durationpb.New(time.Hour), Sdp: "not an sdp"})
	if a.GetError().GetCode() != ownsandv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT {
		t.Fatalf("answer %v", a)
	}
	requireNoSession(t, dp)
}
