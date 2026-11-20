// SPDX-License-Identifier: Apache-2.0

package ownsand

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/pion/webrtc/v4"
	"google.golang.org/protobuf/proto"

	"github.com/delinoio/mono/packages/ownsand-sdk-go/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// DataChannelLabel is the label of the one data channel of a session.
const DataChannelLabel = "ownsand-data"

// p2pConfig holds peer-to-peer settings. Tests restrict ICE to loopback.
type p2pConfig struct {
	settingEngine func() *webrtc.SettingEngine
}

// errSessionEnded reports a session end frame: the agent ended the session
// (idle, expiry, revocation or agent channel loss). It is not a failure;
// the SDK opens a new session for the open operations.
type errSessionEnded struct {
	reason ownsandv1.SessionEndReason
}

func (e *errSessionEnded) Error() string {
	return fmt.Sprintf("ownsand: data path session ended (%s)", e.reason)
}

// errP2PFailed reports a data channel that closed without a session-end
// frame, an ICE failure or an invalid byte stream. The handle moves to the
// relay.
type errP2PFailed struct {
	cause error
}

func (e *errP2PFailed) Error() string {
	return "ownsand: peer-to-peer data channel failed: " + e.cause.Error()
}

func (e *errP2PFailed) Unwrap() error { return e.cause }

// errP2POpen reports that a new session could not open (ICE, DTLS or the
// 10-second open timeout).
type errP2POpen struct {
	cause error
}

func (e *errP2POpen) Error() string {
	return "ownsand: peer-to-peer data channel did not open: " + e.cause.Error()
}

func (e *errP2POpen) Unwrap() error { return e.cause }

// p2pSession is one OpenSession session: one ordered, reliable data channel
// that carries the concurrent streams of a sandbox handle.
type p2pSession struct {
	sandboxID string
	sessionID string
	pc        *webrtc.PeerConnection
	dc        *webrtc.DataChannel

	sendMu   sync.Mutex
	lowWater chan struct{}

	mu      sync.Mutex
	streams map[uint64]*p2pStream
	nextID  uint64
	dec     *datapath.Decoder

	done    chan struct{}
	endOnce sync.Once
	endErr  error
}

// maxBufferedAmount bounds the bytes queued on the data channel before the
// sender waits, so a large write applies back-pressure to the caller.
const maxBufferedAmount = 4 << 20

func openP2PSession(ctx context.Context, c *Client, org, sandboxID string) (*p2pSession, error) {
	iceRes, err := c.dataPath.GetIceServers(ctx, connect.NewRequest(&ownsandv1.GetIceServersRequest{OrganizationId: org}))
	if err != nil {
		return nil, err
	}
	var servers []webrtc.ICEServer
	for _, s := range iceRes.Msg.GetIceServers() {
		servers = append(servers, webrtc.ICEServer{URLs: s.GetUrls()})
	}
	se := &webrtc.SettingEngine{}
	if c.p2p.settingEngine != nil {
		se = c.p2p.settingEngine()
	}
	api := webrtc.NewAPI(webrtc.WithSettingEngine(*se))
	pc, err := api.NewPeerConnection(webrtc.Configuration{ICEServers: servers})
	if err != nil {
		return nil, &errP2POpen{cause: err}
	}
	ordered := true
	dc, err := pc.CreateDataChannel(DataChannelLabel, &webrtc.DataChannelInit{Ordered: &ordered})
	if err != nil {
		_ = pc.Close()
		return nil, &errP2POpen{cause: err}
	}
	s := &p2pSession{
		sandboxID: sandboxID,
		pc:        pc,
		dc:        dc,
		streams:   map[uint64]*p2pStream{},
		dec:       datapath.NewDecoder(),
		done:      make(chan struct{}),
		lowWater:  make(chan struct{}, 1),
	}
	opened := make(chan struct{})
	var openOnce sync.Once
	dc.OnOpen(func() { openOnce.Do(func() { close(opened) }) })
	dc.OnMessage(s.onMessage)
	dc.OnClose(func() { s.end(&errP2PFailed{cause: errors.New("data channel closed without a session-end frame")}) })
	dc.SetBufferedAmountLowThreshold(maxBufferedAmount / 4)
	dc.OnBufferedAmountLow(func() {
		select {
		case s.lowWater <- struct{}{}:
		default:
		}
	})
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		if st == webrtc.PeerConnectionStateFailed || st == webrtc.PeerConnectionStateClosed {
			s.end(&errP2PFailed{cause: fmt.Errorf("peer connection %s", st)})
		}
	})

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		_ = pc.Close()
		return nil, &errP2POpen{cause: err}
	}
	// Non-trickle signaling: gather every candidate before the offer leaves.
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		_ = pc.Close()
		return nil, &errP2POpen{cause: err}
	}
	select {
	case <-gathered:
	case <-ctx.Done():
		_ = pc.Close()
		return nil, ctx.Err()
	}
	res, err := c.dataPath.OpenSession(ctx, connect.NewRequest(&ownsandv1.OpenSessionRequest{
		OrganizationId: org,
		SandboxId:      sandboxID,
		SdpOffer:       pc.LocalDescription().SDP,
	}))
	if err != nil {
		_ = pc.Close()
		return nil, err
	}
	s.sessionID = res.Msg.GetSessionId()
	// pion verifies the DTLS fingerprint of the answer, which arrived over
	// the authenticated control plane connection.
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: res.Msg.GetSdpAnswer()}); err != nil {
		_ = pc.Close()
		return nil, &errP2POpen{cause: err}
	}
	timer := time.NewTimer(c.timing.p2pOpenTimeout)
	defer timer.Stop()
	select {
	case <-opened:
		return s, nil
	case <-s.done:
		_ = pc.Close()
		return nil, &errP2POpen{cause: s.endErr}
	case <-timer.C:
		_ = pc.Close()
		return nil, &errP2POpen{cause: errors.New("data channel did not open within the open timeout")}
	case <-ctx.Done():
		_ = pc.Close()
		return nil, ctx.Err()
	}
}

// end ends the session once with err and closes the peer connection.
func (s *p2pSession) end(err error) {
	s.endOnce.Do(func() {
		s.endErr = err
		close(s.done)
		go func() { _ = s.pc.Close() }()
	})
}

func (s *p2pSession) isDone() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

func (s *p2pSession) close() {
	s.end(&errP2PFailed{cause: errors.New("session closed by the caller")})
}

func (s *p2pSession) onMessage(msg webrtc.DataChannelMessage) {
	s.mu.Lock()
	frames, err := s.dec.Push(msg.Data)
	s.mu.Unlock()
	for _, f := range frames {
		s.dispatch(f)
	}
	if err != nil {
		// The decoder stays failed; the receiver closes the data channel.
		s.end(&errP2PFailed{cause: err})
	}
}

func (s *p2pSession) dispatch(f *ownsandv1.DataFrame) {
	if f.GetKind() == ownsandv1.DataFrameKind_DATA_FRAME_KIND_SESSION_END {
		s.end(&errSessionEnded{reason: f.GetSessionEnd().GetReason()})
		return
	}
	if err := datapath.ValidateFrame(f); err != nil {
		// Unknown kinds of a newer schema on stream 0 are ignored; other
		// malformed frames fail only their stream.
		var uk *datapath.UnknownKindError
		if errors.As(err, &uk) && f.GetStreamId() == datapath.ConnectionStreamID {
			return
		}
	}
	s.mu.Lock()
	st := s.streams[f.GetStreamId()]
	s.mu.Unlock()
	if st == nil {
		return
	}
	select {
	case st.ch <- f:
	case <-st.closed:
	case <-s.done:
	}
}

// send writes one frame as data channel messages of at most 16 KiB.
func (s *p2pSession) send(ctx context.Context, f *ownsandv1.DataFrame) error {
	msgs, err := datapath.EncodeMessages(f)
	if err != nil {
		return err
	}
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	for _, m := range msgs {
		for s.dc.BufferedAmount() > maxBufferedAmount {
			select {
			case <-s.lowWater:
			case <-time.After(50 * time.Millisecond):
			case <-s.done:
				return s.endErr
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if s.isDone() {
			return s.endErr
		}
		if err := s.dc.Send(m); err != nil {
			s.end(&errP2PFailed{cause: err})
			return s.endErr
		}
	}
	return nil
}

// p2pStream is one request stream on a session.
type p2pStream struct {
	s      *p2pSession
	id     uint64
	ch     chan *ownsandv1.DataFrame
	closed chan struct{}
	once   sync.Once
}

// openStream allocates a new stream_id and sends the request payload.
func (s *p2pSession) openStream(ctx context.Context, payload proto.Message) (*p2pStream, error) {
	s.mu.Lock()
	s.nextID++
	st := &p2pStream{s: s, id: s.nextID, ch: make(chan *ownsandv1.DataFrame, 64), closed: make(chan struct{})}
	s.streams[st.id] = st
	s.mu.Unlock()
	f, err := datapath.NewFrame(st.id, s.sandboxID, payload)
	if err != nil {
		st.close()
		return nil, err
	}
	if err := s.send(ctx, f); err != nil {
		st.close()
		return nil, err
	}
	return st, nil
}

// recv returns the next frame of the stream, or the session's end error.
func (st *p2pStream) recv(ctx context.Context) (*ownsandv1.DataFrame, error) {
	select {
	case f := <-st.ch:
		return f, nil
	default:
	}
	select {
	case f := <-st.ch:
		return f, nil
	case <-st.s.done:
		select {
		case f := <-st.ch:
			return f, nil
		default:
		}
		return nil, st.s.endErr
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (st *p2pStream) close() {
	st.once.Do(func() {
		close(st.closed)
		st.s.mu.Lock()
		delete(st.s.streams, st.id)
		st.s.mu.Unlock()
	})
}
