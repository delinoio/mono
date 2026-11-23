// SPDX-License-Identifier: FSL-1.1-ALv2

package dataplane

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/agent"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/ops"
	"github.com/delinoio/mono/go/ownsand/datapath"
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
	// endReason is the reason that the first end call carried.
	endReason ownsandv1.SessionEndReason
	decoder   *datapath.Decoder
	// remainingAtAnswer is the lifetime that was left when the offer was
	// answered; the session-end log event reports it.
	remainingAtAnswer time.Duration

	// stop closes when the session starts to end. It interrupts operation
	// sends that wait for the buffer to drain, so the bounded session-end
	// flush can take sendMu.
	stop chan struct{}

	sendMu sync.Mutex
	low    chan struct{}
	// torn reports that a send stopped within a frame; the byte stream
	// then carries a partial frame and nothing else can follow it.
	torn bool
}

var errSessionEnding = errors.New("dataplane: session is ending")

func answerError(sessionID string, code ownsandv1.ErrorCode, msg string) *ownsandv1.SignalingAnswer {
	return &ownsandv1.SignalingAnswer{SessionId: sessionID, Error: &ownsandv1.ErrorDetail{Code: code, Message: msg}}
}

// Offer answers a signaling offer. The answer carries every local ICE
// candidate (non-trickle). pion checks during the DTLS handshake that the
// peer's certificate matches the fingerprint of this offer, so the data
// channel accepts only the caller that the control plane authorized.
//
// The session lifetime starts when the offer arrives: Offer fixes one
// monotonic deadline from expires_in (or from the expire_time fallback) before
// it sets up the peer connection, so the time spent to gather candidates
// counts against the lifetime. An offer whose deadline passes during setup
// gets an error answer and leaves no session.
func (d *DataPlane) Offer(ctx context.Context, ch agent.Channel, offer *ownsandv1.SignalingOffer) *ownsandv1.SignalingAnswer {
	logger := d.logger.With("session_id", offer.GetSessionId(), "sandbox_id", offer.GetSandboxId())
	// Both forms of the lifetime become one deadline at this point. The
	// time.Time value carries a monotonic reading, so a wall clock step does
	// not move the deadline.
	received := time.Now()
	deadline := offerDeadline(offer, received)
	d.mu.Lock()
	closed, stun := d.closed, append([]string(nil), d.stunURLs...)
	_, dup := d.sessions[offer.GetSessionId()]
	// Cheap early check; the authoritative one shares the critical section
	// that inserts the session, below. A full data plane leaves a pending
	// tombstone for the caller's next attempt.
	full := !closed && !dup && len(d.sessions) >= d.limits.MaxSessions
	// A revocation that arrived before this offer registered its session
	// already ended it: the tombstone is consumed by the offer it names.
	preRevoked := !closed && !dup && !full && offer.GetSessionId() != "" && d.revoked.take(offer.GetSessionId(), time.Now())
	d.mu.Unlock()
	if closed {
		return answerError(offer.GetSessionId(), ownsandv1.ErrorCode_ERROR_CODE_UNAVAILABLE, "agent is shutting down")
	}
	if dup || offer.GetSessionId() == "" || offer.GetSdp() == "" {
		return answerError(offer.GetSessionId(), ownsandv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "offer needs a new session ID and an SDP body")
	}
	if full {
		return d.rejectSessionCapacity(offer)
	}
	if preRevoked {
		logger.Warn("session revoked before its offer was handled", "cancellation_reason", ownsandv1.SessionEndReason_SESSION_END_REASON_REVOKED.String())
		return answerError(offer.GetSessionId(), ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION, "session ended before it was answered")
	}
	if state, ok := d.opts.Sandboxes.State(offer.GetSandboxId()); !ok || state != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		return answerError(offer.GetSessionId(), ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION, "sandbox is not running on this device")
	}
	if !deadline.After(received) {
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
		decoder: datapath.NewDecoder(), low: make(chan struct{}, 1), stop: make(chan struct{})}
	// The session is visible to Revoke, ChannelLost and Close from here on,
	// so each of them can end it while its candidates are gathered. The
	// closed check and the insertion share one critical section: Close
	// either finds the session in the map or the offer sees closed.
	d.mu.Lock()
	_, dup = d.sessions[s.id]
	closed = d.closed
	// A revocation that arrived while the peer connection was created found
	// no session; the tombstone check shares this critical section with the
	// insertion, so a revocation either left a tombstone here or finds the
	// session in the map.
	full = !closed && !dup && len(d.sessions) >= d.limits.MaxSessions
	preRevoked = !closed && !dup && !full && d.revoked.take(s.id, time.Now())
	if !closed && !dup && !full && !preRevoked {
		d.sessions[s.id] = s
	}
	d.mu.Unlock()
	if closed || dup || full || preRevoked {
		cancel()
		pc.Close()
		if full {
			return d.rejectSessionCapacity(offer)
		}
		if preRevoked {
			logger.Warn("session revoked before it was registered", "cancellation_reason", ownsandv1.SessionEndReason_SESSION_END_REASON_REVOKED.String())
			return answerError(offer.GetSessionId(), ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION, "session ended before it was answered")
		}
		if closed {
			return answerError(offer.GetSessionId(), ownsandv1.ErrorCode_ERROR_CODE_UNAVAILABLE, "agent is shutting down")
		}
		return answerError(offer.GetSessionId(), ownsandv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "offer needs a new session ID and an SDP body")
	}
	pc.OnDataChannel(s.onDataChannel)
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		if st == webrtc.PeerConnectionStateFailed || st == webrtc.PeerConnectionStateClosed {
			s.end(ownsandv1.SessionEndReason_SESSION_END_REASON_UNSPECIFIED)
		}
	})
	// fail ends the session, which removes it from the session map and
	// closes its peer connection.
	fail := func(msg string, err error) *ownsandv1.SignalingAnswer {
		logger.Warn("answer failed", "step", msg, "error", err)
		s.end(ownsandv1.SessionEndReason_SESSION_END_REASON_UNSPECIFIED)
		return answerError(offer.GetSessionId(), ownsandv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "offer could not be answered: "+msg)
	}
	// ended answers an offer whose session ended before the answer was
	// ready (revoked, closed data plane or lost channel). A closed data plane
	// answers unavailable like an offer that arrives after Close; every other
	// end answers failed_precondition.
	ended := func() *ownsandv1.SignalingAnswer {
		s.mu.Lock()
		reason := s.endReason
		s.mu.Unlock()
		d.mu.Lock()
		closed := d.closed
		d.mu.Unlock()
		logger.Warn("session ended before it was answered", "cancellation_reason", reason.String(), "data_plane_closed", closed)
		if closed {
			return answerError(offer.GetSessionId(), ownsandv1.ErrorCode_ERROR_CODE_UNAVAILABLE, "agent is shutting down")
		}
		return answerError(offer.GetSessionId(), ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION, "session ended before it was answered")
	}
	// expired answers an offer whose lifetime ran out during setup. It ends
	// the session, which closes the peer connection and removes the session
	// from the map. An end that another cause started first keeps its reason.
	expired := func() *ownsandv1.SignalingAnswer {
		s.end(ownsandv1.SessionEndReason_SESSION_END_REASON_EXPIRED)
		if s.endedBy() != ownsandv1.SessionEndReason_SESSION_END_REASON_EXPIRED {
			return ended()
		}
		logger.Warn("session expired before it was answered", "setup_overrun_ms", time.Since(deadline).Milliseconds())
		return answerError(offer.GetSessionId(), ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION, "session already expired")
	}
	// A channel that closed before or during the answer ends the session.
	// ChannelLost covers the sessions in the map by channel generation;
	// this also covers a channel whose loss was reported before the
	// insertion above.
	select {
	case <-ch.Done():
		s.end(ownsandv1.SessionEndReason_SESSION_END_REASON_AGENT_CHANNEL_LOST)
		return ended()
	default:
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer.GetSdp()}); err != nil {
		if s.isEnded() {
			return ended()
		}
		return fail("remote description", err)
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		if s.isEnded() {
			return ended()
		}
		return fail("create answer", err)
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		if s.isEnded() {
			return ended()
		}
		return fail("local description", err)
	}
	timeout := time.NewTimer(gatherTimeout)
	defer timeout.Stop()
	// The session lifetime also bounds the setup: an offer that expires
	// while its candidates are gathered ends at its deadline.
	expiry := time.NewTimer(time.Until(deadline))
	defer expiry.Stop()
	select {
	case <-gathered:
	case <-expiry.C:
		return expired()
	case <-timeout.C:
		return fail("candidate gathering", errors.New("timed out"))
	case <-ctx.Done():
		return fail("candidate gathering", ctx.Err())
	case <-ch.Done():
		s.end(ownsandv1.SessionEndReason_SESSION_END_REASON_AGENT_CHANNEL_LOST)
		return ended()
	case <-s.ctx.Done():
		return ended()
	}
	if s.isEnded() {
		return ended()
	}
	// The gathered select above may pick the candidates although the
	// deadline passed in the same instant.
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return expired()
	}
	s.mu.Lock()
	s.remainingAtAnswer = remaining
	s.mu.Unlock()
	go s.watch(deadline)
	// A channel that closed while the offer was answered ends the session.
	go func() {
		select {
		case <-ch.Done():
			s.end(ownsandv1.SessionEndReason_SESSION_END_REASON_AGENT_CHANNEL_LOST)
		case <-sctx.Done():
		}
	}()
	logger.Info("session answered", "transport", ownsandv1.DataPathTransport_DATA_PATH_TRANSPORT_PEER_TO_PEER.String(), "remaining_lifetime_ms", remaining.Milliseconds())
	return &ownsandv1.SignalingAnswer{SessionId: s.id, Sdp: pc.LocalDescription().SDP}
}

// rejectSessionCapacity answers an offer that arrives while the data plane
// holds the maximum number of sessions. It creates no peer connection.
func (d *DataPlane) rejectSessionCapacity(offer *ownsandv1.SignalingOffer) *ownsandv1.SignalingAnswer {
	d.sessionRejected.note(d.logger, time.Now(), "session", offer.GetSessionId(), offer.GetSandboxId(), d.limits.MaxSessions)
	return answerError(offer.GetSessionId(), ownsandv1.ErrorCode_ERROR_CODE_RESOURCE_EXHAUSTED, sessionCapacityMessage)
}

// offerDeadline returns the deadline of the session lifetime that the offer
// carries, measured from received. It uses expires_in and falls back to the
// absolute expire_time. The result is not after received when the lifetime
// already ran out.
func offerDeadline(offer *ownsandv1.SignalingOffer, received time.Time) time.Time {
	expiresIn := offer.GetExpiresIn().AsDuration()
	if offer.GetExpiresIn() == nil && offer.GetExpireTime() != nil {
		expiresIn = offer.GetExpireTime().AsTime().Sub(received)
	}
	return received.Add(expiresIn)
}

func (s *session) endedBy() ownsandv1.SessionEndReason {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.endReason
}

func (s *session) isEnded() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ended
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
	// The limit counts the streams that carry a request. A rejected request
	// registers nothing and starts no goroutine, so it neither keeps the
	// session out of the idle rule nor holds a slot; the session stays open.
	if len(s.streams) >= s.d.limits.MaxStreamsPerSession {
		s.mu.Unlock()
		s.d.streamRejected.note(s.d.logger, time.Now(), "stream", s.id, s.sandboxID, s.d.limits.MaxStreamsPerSession)
		s.errorFrame(id, ownsandv1.ErrorCode_ERROR_CODE_RESOURCE_EXHAUSTED, streamCapacityMessage)
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

// send writes one frame of an operation. It fails once the session has
// started to end.
func (s *session) send(ctx context.Context, f *ownsandv1.DataFrame) error {
	return s.write(ctx, f, s.stop)
}

// write writes one frame as data channel messages of at most 16 KiB. It
// waits while the channel buffers more than maxBuffered bytes, until ctx
// ends or stop closes; a nil stop (the session-end flush) never stops it.
func (s *session) write(ctx context.Context, f *ownsandv1.DataFrame, stop <-chan struct{}) error {
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
	select {
	case <-stop:
		return errSessionEnding
	default:
	}
	if s.torn {
		return errors.New("dataplane: data channel stream carries a partial frame")
	}
	for i, m := range msgs {
		for dc.BufferedAmount() > maxBuffered {
			var err error
			select {
			case <-ctx.Done():
				err = ctx.Err()
			case <-stop:
				err = errSessionEnding
			case <-s.low:
			case <-time.After(time.Second):
			}
			if err != nil {
				s.torn = s.torn || i > 0
				return err
			}
		}
		if err := dc.Send(m); err != nil {
			s.torn = s.torn || i > 0
			return err
		}
	}
	s.touch()
	return nil
}

// watch ends the session at its absolute expiry deadline and after the idle
// time without frames and without an open request.
func (s *session) watch(deadline time.Time) {
	expiry := time.NewTimer(time.Until(deadline))
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
			if s.endIdle() {
				return
			}
		}
	}
}

// idleLocked reports whether the session has no open request and no frame
// activity for the idle time. The caller holds s.mu.
func (s *session) idleLocked() bool {
	return len(s.streams) == 0 && time.Since(s.activity) >= s.d.timing.SessionIdle
}

// endIdle ends the session with the idle reason when it is idle. It
// evaluates the idle predicate and starts the end in one critical section,
// so a request admitted or a frame written before that section keeps the
// session open, and a request that arrives after it finds the session
// ended and never registers or reaches the dispatcher. It reports whether
// it ended the session.
func (s *session) endIdle() bool {
	s.mu.Lock()
	if s.ended || !s.idleLocked() {
		s.mu.Unlock()
		return false
	}
	t := s.beginEndLocked(ownsandv1.SessionEndReason_SESSION_END_REASON_IDLE)
	s.mu.Unlock()
	s.finishEnd(t)
	return true
}

// end closes the session. With a reason, it first sends a session-end
// frame on every open stream.
func (s *session) end(reason ownsandv1.SessionEndReason) {
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	t := s.beginEndLocked(reason)
	s.mu.Unlock()
	s.finishEnd(t)
}

// sessionEnd is the state that the transition into the ended state captures
// for the teardown.
type sessionEnd struct {
	reason            ownsandv1.SessionEndReason
	open              []uint64
	dc                *webrtc.DataChannel
	remainingAtAnswer time.Duration
}

// beginEndLocked moves the session into the ended state. The caller holds
// s.mu and has checked that s.ended is false.
func (s *session) beginEndLocked(reason ownsandv1.SessionEndReason) sessionEnd {
	s.ended = true
	s.endReason = reason
	close(s.stop)
	var open []uint64
	for id := range s.streams {
		open = append(open, id)
	}
	return sessionEnd{reason: reason, open: open, dc: s.dc, remainingAtAnswer: s.remainingAtAnswer}
}

// finishEnd tears the ended session down. It runs without s.mu.
func (s *session) finishEnd(t sessionEnd) {
	reason, open, dc, remainingAtAnswer := t.reason, t.open, t.dc, t.remainingAtAnswer
	if reason != ownsandv1.SessionEndReason_SESSION_END_REASON_UNSPECIFIED && dc != nil {
		// Closing stop above made every waiting operation send return, so
		// write gets sendMu without waiting for a stalled channel.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		for _, id := range open {
			if err := s.write(ctx, datapath.MustNewFrame(id, s.sandboxID, &ownsandv1.SessionEnd{Reason: reason}), nil); err != nil {
				s.d.logger.Warn("session-end frame not sent", "session_id", s.id, "stream_id", id, "error", err)
				break
			}
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
	s.d.logger.Info("session ended", "session_id", s.id, "reason", reason.String(), "open_streams", len(open), "remaining_lifetime_at_answer_ms", remainingAtAnswer.Milliseconds())
}
