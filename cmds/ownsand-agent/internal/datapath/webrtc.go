package datapath

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"

	"connectrpc.com/connect"
	sdkdp "github.com/delinoio/mono/packages/ownsand-sdk-go/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/pion/webrtc/v4"
)

const (
	// SessionIdle ends a session without frames and without an open request.
	SessionIdle = 15 * time.Minute
	// SessionMaxLifetime caps the absolute expiry of a session.
	SessionMaxLifetime = time.Hour
	// MaxSessions caps the open peer-to-peer sessions of the agent.
	MaxSessions = 64
	// sendBufferHigh pauses senders while the data channel buffers more.
	sendBufferHigh = 4 << 20
	sendBufferLow  = 1 << 20
)

// sessionTable holds the peer-to-peer sessions.
type sessionTable struct {
	log  *slog.Logger
	disp *Dispatcher
	sb   SandboxAccess
	// includeLoopback restricts ICE to loopback candidates (tests only).
	includeLoopback bool

	mu       sync.Mutex
	ice      []webrtc.ICEServer
	sessions map[string]*session
}

func newSessionTable(log *slog.Logger, disp *Dispatcher, sb SandboxAccess, includeLoopback bool) *sessionTable {
	return &sessionTable{log: log, disp: disp, sb: sb, includeLoopback: includeLoopback, sessions: map[string]*session{}}
}

func (t *sessionTable) setIceServers(servers []*ownsandv1.IceServer) {
	ice := make([]webrtc.ICEServer, 0, len(servers))
	for _, s := range servers {
		ice = append(ice, webrtc.ICEServer{URLs: s.GetUrls()})
	}
	t.mu.Lock()
	t.ice = ice
	t.mu.Unlock()
}

// answer handles an OpenSession offer from the agent channel and returns
// the signaling answer. Signaling is non-trickle: the answer carries every
// candidate.
func (t *sessionTable) answer(gen uint64, offer *ownsandv1.SignalingOffer) *ownsandv1.SignalingAnswer {
	ans := &ownsandv1.SignalingAnswer{SessionId: offer.GetSessionId()}
	failWith := func(code connect.Code, msg string) *ownsandv1.SignalingAnswer {
		ans.ErrorCode, ans.ErrorMessage = code.String(), msg
		t.log.Info("session offer rejected", "session_id", offer.GetSessionId(), "code", code.String())
		return ans
	}
	if t.sb.SandboxState(offer.GetSandboxId()) != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		return failWith(connect.CodeFailedPrecondition, "the sandbox is not running")
	}
	remaining := min(time.Until(offer.GetExpireTime().AsTime()), SessionMaxLifetime)
	if offer.GetExpireTime() == nil || remaining <= 0 {
		return failWith(connect.CodeInvalidArgument, "the offer has no future expiry")
	}
	t.mu.Lock()
	if len(t.sessions) >= MaxSessions {
		t.mu.Unlock()
		return failWith(connect.CodeResourceExhausted, "the agent is at its session limit")
	}
	ice := t.ice
	t.mu.Unlock()

	var se webrtc.SettingEngine
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4, webrtc.NetworkTypeUDP6})
	se.SetIncludeLoopbackCandidate(t.includeLoopback)
	if t.includeLoopback {
		// Tests: gather only loopback candidates, so ICE does not probe every
		// host interface.
		se.SetInterfaceFilter(func(name string) bool { return name == "lo" })
	}
	// pion checks the remote DTLS certificate against the fingerprint in the
	// offer SDP, so the data channel accepts only the fingerprint that the
	// control plane forwarded. Never disable that check.
	api := webrtc.NewAPI(webrtc.WithSettingEngine(se))
	pc, err := api.NewPeerConnection(webrtc.Configuration{ICEServers: ice})
	if err != nil {
		return failWith(connect.CodeInternal, "create peer connection failed")
	}
	s := &session{
		t: t, id: offer.GetSessionId(), sandboxID: offer.GetSandboxId(), gen: gen, pc: pc,
		expires: time.Now().Add(remaining), last: time.Now(), streams: map[uint64]*p2pStream{},
		lowBuf: make(chan struct{}, 1),
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	pc.OnDataChannel(s.onDataChannel)
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		if st == webrtc.PeerConnectionStateFailed || st == webrtc.PeerConnectionStateClosed {
			s.close()
		}
	})
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer.GetSdpOffer()}); err != nil {
		_ = pc.Close()
		return failWith(connect.CodeInvalidArgument, "the offer is not a valid SDP offer")
	}
	desc, err := pc.CreateAnswer(nil)
	if err != nil {
		_ = pc.Close()
		return failWith(connect.CodeInvalidArgument, "create answer failed")
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(desc); err != nil {
		_ = pc.Close()
		return failWith(connect.CodeInternal, "set local description failed")
	}
	select {
	case <-gathered:
	case <-time.After(20 * time.Second):
		_ = pc.Close()
		return failWith(connect.CodeUnavailable, "ICE candidate gathering timed out")
	}
	t.mu.Lock()
	t.sessions[s.id] = s
	t.mu.Unlock()
	go s.watch()
	ans.SdpAnswer = pc.LocalDescription().SDP
	t.log.Info("session answered", "session_id", s.id, "sandbox_id", s.sandboxID, "transport", ownsandv1.DataPathTransport_DATA_PATH_TRANSPORT_PEER_TO_PEER.String())
	return ans
}

// end ends one session, or every session of a channel.
func (t *sessionTable) end(sessionID string, reason ownsandv1.SessionEndReason) {
	t.mu.Lock()
	s := t.sessions[sessionID]
	t.mu.Unlock()
	if s != nil {
		s.end(reason)
	}
}

func (t *sessionTable) channelLost(gen uint64) {
	t.mu.Lock()
	var ss []*session
	for _, s := range t.sessions {
		if s.gen == gen {
			ss = append(ss, s)
		}
	}
	t.mu.Unlock()
	for _, s := range ss {
		s.end(ownsandv1.SessionEndReason_SESSION_END_REASON_AGENT_CHANNEL_LOST)
	}
}

func (t *sessionTable) closeAll() {
	t.mu.Lock()
	var ss []*session
	for _, s := range t.sessions {
		ss = append(ss, s)
	}
	t.mu.Unlock()
	for _, s := range ss {
		s.close()
	}
}

// session is one peer-to-peer data channel session.
type session struct {
	t         *sessionTable
	id        string
	sandboxID string
	gen       uint64
	pc        *webrtc.PeerConnection
	ctx       context.Context
	cancel    context.CancelFunc
	expires   time.Time

	wmu    sync.Mutex
	lowBuf chan struct{}

	mu      sync.Mutex
	dc      *webrtc.DataChannel
	last    time.Time
	streams map[uint64]*p2pStream
	closed  bool
}

func (s *session) activity() {
	s.mu.Lock()
	s.last = time.Now()
	s.mu.Unlock()
}

func (s *session) onDataChannel(dc *webrtc.DataChannel) {
	s.mu.Lock()
	if s.dc != nil || s.closed {
		s.mu.Unlock()
		_ = dc.Close() // one data channel per session
		return
	}
	s.dc = dc
	s.mu.Unlock()
	pr, pw := io.Pipe()
	dc.SetBufferedAmountLowThreshold(sendBufferLow)
	dc.OnBufferedAmountLow(func() {
		select {
		case s.lowBuf <- struct{}{}:
		default:
		}
	})
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		s.activity()
		_, _ = pw.Write(msg.Data)
	})
	dc.OnClose(func() {
		_ = pw.Close()
		s.close()
	})
	go func() {
		fr := sdkdp.NewFrameReader(pr)
		for {
			f, err := fr.ReadDataFrame()
			if err != nil {
				if errors.Is(err, sdkdp.ErrFrameTooLarge) {
					s.t.log.Warn("data channel frame exceeds the size limit; closing the session", "session_id", s.id)
				}
				_ = pr.CloseWithError(err)
				s.close()
				return
			}
			s.handle(f)
		}
	}()
}

func (s *session) handle(f *ownsandv1.DataFrame) {
	id := f.GetStreamId()
	if f.GetRelayStreamClose() != nil {
		// Accepted on the data channel too: it releases the stream's
		// output subscription without stopping the operation.
		s.mu.Lock()
		st := s.streams[id]
		s.mu.Unlock()
		if st != nil {
			st.cancel()
		}
		return
	}
	if !IsRequest(f) {
		return
	}
	s.mu.Lock()
	if _, dup := s.streams[id]; dup || s.closed {
		s.mu.Unlock()
		_ = s.write(errorFrame(id, s.sandboxID, connect.CodeInvalidArgument, "stream_id is already open"))
		return
	}
	st := &p2pStream{s: s, id: id}
	st.ctx, st.cancel = context.WithCancel(s.ctx)
	s.streams[id] = st
	s.mu.Unlock()
	go func() {
		s.t.disp.Serve(s.sandboxID, f, st)
		st.cancel()
		s.mu.Lock()
		delete(s.streams, id)
		s.last = time.Now()
		s.mu.Unlock()
	}()
}

// write encodes one frame and sends it as data channel messages of at most
// 16 KiB. The messages of one frame stay contiguous in the byte stream.
func (s *session) write(f *ownsandv1.DataFrame) error {
	b, err := sdkdp.EncodeFrame(f)
	if err != nil {
		return err
	}
	s.mu.Lock()
	dc := s.dc
	s.mu.Unlock()
	if dc == nil {
		return errors.New("no data channel")
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	for dc.BufferedAmount() > sendBufferHigh {
		select {
		case <-s.lowBuf:
		case <-time.After(100 * time.Millisecond):
		case <-s.ctx.Done():
			return s.ctx.Err()
		}
	}
	for _, m := range sdkdp.SplitMessages(b) {
		if err := dc.Send(m); err != nil {
			return err
		}
	}
	s.activity()
	return nil
}

// watch ends the session at its idle time or absolute expiry.
func (s *session) watch() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
		}
		s.mu.Lock()
		idle := len(s.streams) == 0 && time.Since(s.last) >= SessionIdle
		s.mu.Unlock()
		switch {
		case !time.Now().Before(s.expires):
			s.end(ownsandv1.SessionEndReason_SESSION_END_REASON_EXPIRED)
		case idle:
			s.end(ownsandv1.SessionEndReason_SESSION_END_REASON_IDLE)
		}
	}
}

// end sends a session-end frame on stream 0 and closes the session.
func (s *session) end(reason ownsandv1.SessionEndReason) {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return
	}
	_ = s.write(sessionEndFrame(0, s.sandboxID, reason))
	// Let SCTP deliver the session-end frame before the connection closes.
	s.mu.Lock()
	dc := s.dc
	s.mu.Unlock()
	for deadline := time.Now().Add(2 * time.Second); dc != nil && dc.BufferedAmount() > 0 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	s.t.log.Info("session ended", "session_id", s.id, "reason", reason.String())
	s.close()
}

func (s *session) close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()
	s.cancel()
	s.t.mu.Lock()
	if s.t.sessions[s.id] == s {
		delete(s.t.sessions, s.id)
	}
	s.t.mu.Unlock()
	go func() { _ = s.pc.Close() }()
}

// p2pStream is one request stream of a session; it is the Sink of the
// request. The data channel has its own flow control, so payload frames use
// no relay credit.
type p2pStream struct {
	s      *session
	id     uint64
	ctx    context.Context
	cancel context.CancelFunc
}

func (p *p2pStream) Context() context.Context { return p.ctx }

func (p *p2pStream) Payload(set func(*ownsandv1.DataFrame)) error {
	if p.ctx.Err() != nil {
		return p.ctx.Err()
	}
	return p.s.write(sdkdp.NewFrame(p.id, p.s.sandboxID, set))
}

func (p *p2pStream) Error(code connect.Code, msg string) error {
	return p.s.write(errorFrame(p.id, p.s.sandboxID, code, msg))
}
