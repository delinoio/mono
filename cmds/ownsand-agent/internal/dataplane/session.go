// SPDX-License-Identifier: Apache-2.0

package dataplane

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/agent"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/ops"
	"github.com/delinoio/mono/packages/ownsand-sdk-go/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// DataChannelLabel is the label of the one data channel of a session.
const DataChannelLabel = "ownsand-data"

// Send buffering of a data channel: a sender waits while more than
// maxBuffered bytes are queued and continues below lowWatermark.
const (
	maxBuffered  = 4 << 20
	lowWatermark = 1 << 20
	// gatherTimeout bounds the non-trickle ICE gathering of an answer.
	gatherTimeout = 10 * time.Second
)

type session struct {
	d         *DataPlane
	id        string
	sandboxID string
	gen       uint64
	pc        *webrtc.PeerConnection
	ctx       context.Context
	cancel    context.CancelFunc

	mu       sync.Mutex
	dc       *webrtc.DataChannel
	streams  map[uint64]context.CancelFunc
	activity time.Time
	ended    bool
	decoder  *datapath.Decoder

	sendMu sync.Mutex
	low    chan struct{}
}

func answerError(sessionID string, code ownsandv1.ErrorCode, msg string) *ownsandv1.SignalingAnswer {
	return &ownsandv1.SignalingAnswer{SessionId: sessionID, Error: &ownsandv1.ErrorDetail{Code: code, Message: msg}}
}

// Offer answers a signaling offer. The answer carries every local ICE
// candidate (non-trickle). pion checks during the DTLS handshake that the
// peer's certificate matches the fingerprint of this offer, so the data
// channel accepts only the caller that the control plane authorized.
func (d *DataPlane) Offer(ctx context.Context, ch agent.Channel, offer *ownsandv1.SignalingOffer) *ownsandv1.SignalingAnswer {
	logger := d.logger.With("session_id", offer.GetSessionId(), "sandbox_id", offer.GetSandboxId())
	d.mu.Lock()
	closed, stun := d.closed, append([]string(nil), d.stunURLs...)
	_, dup := d.sessions[offer.GetSessionId()]
	d.mu.Unlock()
	if closed {
		return answerError(offer.GetSessionId(), ownsandv1.ErrorCode_ERROR_CODE_UNAVAILABLE, "agent is shutting down")
	}
	if dup || offer.GetSessionId() == "" || offer.GetSdp() == "" {
		return answerError(offer.GetSessionId(), ownsandv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "offer needs a new session ID and an SDP body")
	}
	if state, ok := d.opts.Sandboxes.State(offer.GetSandboxId()); !ok || state != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		return answerError(offer.GetSessionId(), ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION, "sandbox is not running on this device")
	}
	expiresIn := offer.GetExpiresIn().AsDuration()
	if offer.GetExpiresIn() == nil && offer.GetExpireTime() != nil {
		expiresIn = time.Until(offer.GetExpireTime().AsTime())
	}
	if expiresIn <= 0 {
		return answerError(offer.GetSessionId(), ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION, "session already expired")
	}

	var se webrtc.SettingEngine
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4, webrtc.NetworkTypeUDP6})
	if d.opts.IncludeLoopback {
		se.SetIncludeLoopbackCandidate(true)
	}
	if d.opts.InterfaceFilter != nil {
		se.SetInterfaceFilter(d.opts.InterfaceFilter)
	}
	api := webrtc.NewAPI(webrtc.WithSettingEngine(se))
	var servers []webrtc.ICEServer
	if len(stun) > 0 {
		servers = []webrtc.ICEServer{{URLs: stun}}
	}
	pc, err := api.NewPeerConnection(webrtc.Configuration{ICEServers: servers})
	if err != nil {
		logger.Error("create peer connection failed", "error", err)
		return answerError(offer.GetSessionId(), ownsandv1.ErrorCode_ERROR_CODE_INTERNAL, "peer connection could not be created")
	}
	sctx, cancel := context.WithCancel(context.Background())
	s := &session{d: d, id: offer.GetSessionId(), sandboxID: offer.GetSandboxId(), gen: ch.Generation(), pc: pc,
		ctx: sctx, cancel: cancel, streams: map[uint64]context.CancelFunc{}, activity: time.Now(),
		decoder: datapath.NewDecoder(), low: make(chan struct{}, 1)}
	pc.OnDataChannel(s.onDataChannel)
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		if st == webrtc.PeerConnectionStateFailed || st == webrtc.PeerConnectionStateClosed {
			s.end(ownsandv1.SessionEndReason_SESSION_END_REASON_UNSPECIFIED)
		}
	})
	fail := func(msg string, err error) *ownsandv1.SignalingAnswer {
		logger.Warn("answer failed", "step", msg, "error", err)
		cancel()
		pc.Close()
		return answerError(offer.GetSessionId(), ownsandv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "offer could not be answered: "+msg)
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer.GetSdp()}); err != nil {
		return fail("remote description", err)
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return fail("create answer", err)
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		return fail("local description", err)
	}
	select {
	case <-gathered:
	case <-time.After(gatherTimeout):
		return fail("candidate gathering", errors.New("timed out"))
	case <-ctx.Done():
		return fail("candidate gathering", ctx.Err())
	}
	d.mu.Lock()
	d.sessions[s.id] = s
	d.mu.Unlock()
	go s.watch(expiresIn)
	// A channel that closed while the offer was answered ends the session.
	go func() {
		select {
		case <-ch.Done():
			s.end(ownsandv1.SessionEndReason_SESSION_END_REASON_AGENT_CHANNEL_LOST)
		case <-sctx.Done():
		}
	}()
	logger.Info("session answered", "transport", ownsandv1.DataPathTransport_DATA_PATH_TRANSPORT_PEER_TO_PEER.String(), "expires_in_s", int64(expiresIn.Seconds()))
	return &ownsandv1.SignalingAnswer{SessionId: s.id, Sdp: pc.LocalDescription().SDP}
}

func (s *session) onDataChannel(dc *webrtc.DataChannel) {
	reliable := dc.Ordered() && dc.MaxRetransmits() == nil && dc.MaxPacketLifeTime() == nil
	s.mu.Lock()
	accept := dc.Label() == DataChannelLabel && reliable && s.dc == nil && !s.ended
	if accept {
		s.dc = dc
	}
	s.mu.Unlock()
	if !accept {
		s.d.logger.Warn("rejecting a data channel", "session_id", s.id, "label_ok", dc.Label() == DataChannelLabel, "reliable", reliable)
		dc.Close()
		return
	}
	dc.SetBufferedAmountLowThreshold(lowWatermark)
	dc.OnBufferedAmountLow(func() {
		select {
		case s.low <- struct{}{}:
		default:
		}
	})
	dc.OnOpen(func() {
		s.d.logger.Info("data channel open", "session_id", s.id)
	})
	dc.OnMessage(func(msg webrtc.DataChannelMessage) { s.onMessage(msg.Data) })
	dc.OnClose(func() { s.end(ownsandv1.SessionEndReason_SESSION_END_REASON_UNSPECIFIED) })
}

func (s *session) touch() {
	s.mu.Lock()
	s.activity = time.Now()
	s.mu.Unlock()
}

// onMessage reassembles frames; OnMessage callbacks of one channel run in
// order.
func (s *session) onMessage(data []byte) {
	s.touch()
	frames, err := s.decoder.Push(data)
	for _, f := range frames {
		s.onFrame(f)
	}
	if err != nil {
		s.d.logger.Warn("malformed data channel stream; closing the session", "session_id", s.id, "error", err)
		s.end(ownsandv1.SessionEndReason_SESSION_END_REASON_UNSPECIFIED)
	}
}

func (s *session) errorFrame(streamID uint64, code ownsandv1.ErrorCode, msg string) {
	if streamID == datapath.ConnectionStreamID {
		return
	}
	_ = s.send(s.ctx, datapath.MustNewFrame(streamID, s.sandboxID, &ownsandv1.ErrorDetail{Code: code, Message: msg}))
}

func (s *session) onFrame(f *ownsandv1.DataFrame) {
	if err := datapath.ValidateFrame(f); err != nil {
		var unknown *datapath.UnknownKindError
		if errors.As(err, &unknown) {
			s.errorFrame(f.GetStreamId(), ownsandv1.ErrorCode_ERROR_CODE_UNIMPLEMENTED, "unknown frame kind")
		} else {
			s.errorFrame(f.GetStreamId(), ownsandv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "malformed frame")
		}
		return
	}
	if !datapath.IsRequestKind(f.GetKind()) {
		s.errorFrame(f.GetStreamId(), ownsandv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "frame kind is not a request on a peer-to-peer session")
		return
	}
	id := f.GetStreamId()
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	if _, busy := s.streams[id]; busy {
		s.mu.Unlock()
		s.errorFrame(id, ownsandv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "stream already carries a request")
		return
	}
	ctx, cancel := context.WithCancel(s.ctx)
	s.streams[id] = cancel
	s.mu.Unlock()
	go func() {
		defer func() {
			cancel()
			s.mu.Lock()
			delete(s.streams, id)
			s.activity = time.Now()
			s.mu.Unlock()
		}()
		s.d.opts.Dispatcher.Handle(ctx, ops.Request{Frame: f, SandboxID: s.sandboxID}, sessionSink{s})
	}()
}

type sessionSink struct{ s *session }

func (k sessionSink) Send(ctx context.Context, f *ownsandv1.DataFrame) error {
	return k.s.send(ctx, f)
}

// send writes one frame as data channel messages of at most 16 KiB. It
// waits while the channel buffers more than maxBuffered bytes.
func (s *session) send(ctx context.Context, f *ownsandv1.DataFrame) error {
	msgs, err := datapath.EncodeMessages(f)
	if err != nil {
		return err
	}
	s.mu.Lock()
	dc := s.dc
	s.mu.Unlock()
	if dc == nil {
		return errors.New("dataplane: data channel not open")
	}
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	for _, m := range msgs {
		for dc.BufferedAmount() > maxBuffered {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-s.low:
			case <-time.After(time.Second):
			}
		}
		if err := dc.Send(m); err != nil {
			return err
		}
	}
	s.touch()
	return nil
}

// watch ends the session at its absolute expiry and after the idle time
// without frames and without an open request.
func (s *session) watch(expiresIn time.Duration) {
	expiry := time.NewTimer(expiresIn)
	defer expiry.Stop()
	tick := time.NewTicker(min(s.d.timing.SessionIdle/4, 30*time.Second))
	defer tick.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-expiry.C:
			s.end(ownsandv1.SessionEndReason_SESSION_END_REASON_EXPIRED)
			return
		case <-tick.C:
			s.mu.Lock()
			idle := len(s.streams) == 0 && time.Since(s.activity) >= s.d.timing.SessionIdle
			s.mu.Unlock()
			if idle {
				s.end(ownsandv1.SessionEndReason_SESSION_END_REASON_IDLE)
				return
			}
		}
	}
}

// end closes the session. With a reason, it first sends a session-end
// frame on every open stream.
func (s *session) end(reason ownsandv1.SessionEndReason) {
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	var open []uint64
	for id := range s.streams {
		open = append(open, id)
	}
	dc := s.dc
	s.mu.Unlock()
	if reason != ownsandv1.SessionEndReason_SESSION_END_REASON_UNSPECIFIED && dc != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		for _, id := range open {
			_ = s.send(ctx, datapath.MustNewFrame(id, s.sandboxID, &ownsandv1.SessionEnd{Reason: reason}))
		}
		// Closing the data channel resets its stream and drops queued
		// messages, so wait until the session-end frames left.
		for dc.BufferedAmount() > 0 && ctx.Err() == nil {
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}
	s.cancel()
	if dc != nil {
		dc.Close()
	}
	s.pc.Close()
	s.d.mu.Lock()
	if s.d.sessions[s.id] == s {
		delete(s.d.sessions, s.id)
	}
	s.d.mu.Unlock()
	s.d.logger.Info("session ended", "session_id", s.id, "reason", reason.String(), "open_streams", len(open))
}
