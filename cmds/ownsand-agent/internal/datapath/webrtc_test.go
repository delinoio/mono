package datapath

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	sdkdp "github.com/delinoio/mono/packages/ownsand-sdk-go/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/pion/webrtc/v4"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type caller struct {
	pc     *webrtc.PeerConnection
	dc     *webrtc.DataChannel
	open   chan struct{}
	closed chan struct{}
	frames chan *ownsandv1.DataFrame
}

func newCaller(t *testing.T) (*caller, string) {
	t.Helper()
	var se webrtc.SettingEngine
	se.SetIncludeLoopbackCandidate(true)
	se.SetInterfaceFilter(func(name string) bool { return name == "lo" })
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	pc, err := webrtc.NewAPI(webrtc.WithSettingEngine(se)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	c := &caller{pc: pc, open: make(chan struct{}), closed: make(chan struct{}), frames: make(chan *ownsandv1.DataFrame, 1024)}
	dc, err := pc.CreateDataChannel("ownsand", nil)
	if err != nil {
		t.Fatal(err)
	}
	c.dc = dc
	pr, pw := io.Pipe()
	dc.OnOpen(func() { close(c.open) })
	dc.OnClose(func() { close(c.closed); _ = pw.Close() })
	dc.OnMessage(func(m webrtc.DataChannelMessage) {
		if len(m.Data) > sdkdp.DataChannelMessageSize {
			t.Errorf("message of %d bytes exceeds 16 KiB", len(m.Data))
		}
		_, _ = pw.Write(m.Data)
	})
	go func() {
		fr := sdkdp.NewFrameReader(pr)
		for {
			f, err := fr.ReadDataFrame()
			if err != nil {
				return
			}
			c.frames <- f
		}
	}()
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

func (c *caller) send(t *testing.T, f *ownsandv1.DataFrame) {
	b, err := sdkdp.EncodeFrame(f)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range sdkdp.SplitMessages(b) {
		if err := c.dc.Send(m); err != nil {
			t.Fatal(err)
		}
	}
}

func (e *envLog) waitAnswer(t *testing.T, sessionID string) *ownsandv1.SignalingAnswer {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		e.mu.Lock()
		for _, env := range e.envs {
			if a := env.GetSignalingAnswer(); a != nil && a.GetSessionId() == sessionID {
				e.mu.Unlock()
				return a
			}
		}
		e.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no signaling answer")
	return nil
}

func newP2P(t *testing.T) (*testSandbox, *DataPath, *envLog) {
	sb := newSandbox(t)
	dp, err := New(Config{Sandboxes: sb.mgr, Logger: quiet, Origin: "http://127.0.0.1:1", Authenticate: func(context.Context) (string, error) { return "", context.Canceled }, IncludeLoopback: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dp.Close)
	envs := &envLog{}
	dp.Connected(1, envs.send)
	return sb, dp, envs
}

func TestPeerToPeerExecAndRevocation(t *testing.T) {
	sb, dp, envs := newP2P(t)
	c, sdp := newCaller(t)
	sessionID := sdkdp.NewID()
	dp.HandleOffer(1, &ownsandv1.SignalingOffer{SessionId: sessionID, SandboxId: sb.id, ExpireTime: timestamppb.New(time.Now().Add(time.Hour)), SdpOffer: sdp})
	ans := envs.waitAnswer(t, sessionID)
	if ans.GetErrorCode() != "" {
		t.Fatalf("answer error %s %s", ans.GetErrorCode(), ans.GetErrorMessage())
	}
	if err := c.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: ans.GetSdpAnswer()}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-c.open:
	case <-time.After(20 * time.Second):
		t.Fatal("data channel did not open")
	}
	// An exec whose output spans several data channel messages.
	req := newExec("/bin/sh", "-c", "head -c 100000 /dev/zero | tr '\\0' 'x'")
	f := execFrame(sb.id, req)
	f.StreamId = 7
	c.send(t, f)
	var out bytes.Buffer
	timeout := time.After(20 * time.Second)
	for done := false; !done; {
		select {
		case fr := <-c.frames:
			if fr.GetStreamId() != 7 {
				t.Fatalf("frame on stream %d", fr.GetStreamId())
			}
			ev := fr.GetExecEvent()
			if o := ev.GetOutput(); o != nil {
				out.Write(o.GetData())
			}
			done = ev.GetExited() != nil
		case <-timeout:
			t.Fatal("exec did not finish over the data channel")
		}
	}
	if out.Len() != 100000 {
		t.Fatalf("output %d bytes", out.Len())
	}
	// A revocation ends the session with a session-end frame.
	dp.HandleRevocation(1, &ownsandv1.SessionRevocation{RevocationId: "r", Target: &ownsandv1.SessionRevocation_SessionId{SessionId: sessionID}})
	select {
	case fr := <-c.frames:
		if fr.GetSessionEnd().GetReason() != ownsandv1.SessionEndReason_SESSION_END_REASON_REVOKED || fr.GetStreamId() != 0 {
			t.Fatalf("session end frame %v", fr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no session-end frame")
	}
	envs.mu.Lock()
	acked := false
	for _, env := range envs.envs {
		if env.GetRevocationAck().GetRevocationId() == "r" {
			acked = true
		}
	}
	envs.mu.Unlock()
	if !acked {
		t.Fatal("revocation not acknowledged")
	}
}

func TestOfferRejectedForStoppedSandbox(t *testing.T) {
	sb, dp, envs := newP2P(t)
	sb.command(t, &ownsandv1.AgentCommand{CommandId: "p", Command: &ownsandv1.AgentCommand_PauseSandbox{PauseSandbox: &ownsandv1.PauseSandboxCommand{SandboxId: sb.id}}})
	_, sdp := newCaller(t)
	dp.HandleOffer(1, &ownsandv1.SignalingOffer{SessionId: "s", SandboxId: sb.id, ExpireTime: timestamppb.New(time.Now().Add(time.Hour)), SdpOffer: sdp})
	if a := envs.waitAnswer(t, "s"); a.GetErrorCode() != "failed_precondition" || a.GetSdpAnswer() != "" {
		t.Fatalf("answer %v", a)
	}
}

func TestChannelLossEndsSession(t *testing.T) {
	sb, dp, envs := newP2P(t)
	c, sdp := newCaller(t)
	dp.HandleOffer(1, &ownsandv1.SignalingOffer{SessionId: "s", SandboxId: sb.id, ExpireTime: timestamppb.New(time.Now().Add(time.Hour)), SdpOffer: sdp})
	ans := envs.waitAnswer(t, "s")
	_ = c.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: ans.GetSdpAnswer()})
	select {
	case <-c.open:
	case <-time.After(20 * time.Second):
		t.Fatal("data channel did not open")
	}
	dp.Disconnected(1)
	select {
	case fr := <-c.frames:
		if fr.GetSessionEnd().GetReason() != ownsandv1.SessionEndReason_SESSION_END_REASON_AGENT_CHANNEL_LOST {
			t.Fatalf("frame %v", fr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no session-end frame after channel loss")
	}
}
