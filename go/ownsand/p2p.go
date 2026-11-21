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

	"github.com/delinoio/mono/go/ownsand/datapath"
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

// errSessionRetired reports a session that the SDK itself ended because a
// send was interrupted (for example by a cancelled context) after some
// fragments of a frame were already written. The peer would read the next
// frame's bytes as the missing tail of the torn frame, so the session cannot
// carry another frame. Like errSessionEnded it is not a transport failure:
// the handle stays on peer-to-peer and opens a new session, and concurrent
// streams of the retired session continue on it.
type errSessionRetired struct{}

func (e *errSessionRetired) Error() string {
	return "ownsand: data path session retired after an interrupted frame"
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
	// bufferedAmount reports the bytes queued on the data channel. It is a
	// field only so that tests can hold a send at a chosen fragment, which
	// a real data channel on loopback cannot do deterministically.
	bufferedAmount func() uint64

	queueTimeout time.Duration

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

		bufferedAmount: dc.BufferedAmount,
		queueTimeout:   c.timing.streamQueueTimeout,
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
	s.mu.Lock()
	st := s.streams[f.GetStreamId()]
	s.mu.Unlock()
	if err := datapath.ValidateFrame(f); err != nil {
		// Unknown kinds of a newer schema on stream 0 are ignored, and so is
		// a malformed frame on stream 0 or on a stream that is not open
		// (stream 0 has no p2pStream). Any other invalid frame fails only
		// its stream, so the operation ends with the error instead of
		// waiting for a frame that it can use.
		if st != nil {
			st.fail(&errInvalidFrame{cause: err})
		}
		return
	}
	if st == nil {
		return
	}
	select {
	case <-st.failed:
		// A failed stream receives no more frames.
		return
	default:
	}
	select {
	case st.ch <- f:
		return
	default:
	}
	// The queue is full. The data channel is ordered and this callback
	// serves every stream, so it waits only a bounded time for the
	// stream's consumer: a consumer that stays blocked (for example an exec
	// OnEvent callback that waits for another operation on the same
	// handle) would otherwise stop the responses of every other stream. The
	// failed stream's operation continues from its delivered offsets.
	timer := time.NewTimer(s.queueTimeout)
	defer timer.Stop()
	select {
	case st.ch <- f:
	case <-st.closed:
	case <-s.done:
	case <-timer.C:
		st.fail(errStreamOverrun)
	}
}

// errStreamOverrun reports a stream whose consumer did not take its queued
// frames within the stream queue timeout. It is a transport interruption:
// the operation continues on a new stream from its delivered offsets.
var errStreamOverrun = errors.New("ownsand: peer-to-peer stream consumer fell behind; the stream was failed")

// send writes one frame as data channel messages of at most 16 KiB.
//
// The frame is a varint length prefix and a body split over the messages, so
// an interruption after the first message leaves a torn frame on the ordered
// channel and the peer would take the next frame's bytes as its tail. Such an
// interruption retires the session (errSessionRetired); an interruption before
// the first message leaves the session reusable.
func (s *p2pSession) send(ctx context.Context, f *ownsandv1.DataFrame) error {
	msgs, err := datapath.EncodeMessages(f)
	if err != nil {
		return err
	}
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	for i, m := range msgs {
		for s.bufferedAmount() > maxBufferedAmount {
			select {
			case <-s.lowWater:
			case <-time.After(50 * time.Millisecond):
			case <-s.done:
				return s.endErr
			case <-ctx.Done():
				if i > 0 {
					s.end(&errSessionRetired{})
				}
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

// errInvalidFrame reports a frame that the agent sent on a stream and that
// does not follow the framing rules (for example a kind of a newer schema).
// It ends the operation: a new attempt would receive the same frame.
type errInvalidFrame struct {
	cause error
}

func (e *errInvalidFrame) Error() string {
	return "ownsand: invalid frame on the peer-to-peer data channel: " + e.cause.Error()
}

func (e *errInvalidFrame) Unwrap() error { return e.cause }

// p2pStream is one request stream on a session.
type p2pStream struct {
	s      *p2pSession
	id     uint64
	ch     chan *ownsandv1.DataFrame
	closed chan struct{}
	once   sync.Once

	// failed is closed when the receiver fails the stream with failErr.
	// Frames queued before the failure are still delivered first.
	failed   chan struct{}
	failOnce sync.Once
	failErr  error
}

// fail ends the stream with err for its receiver. Only the receive
// callback calls it, so no frame is queued after the failure.
func (st *p2pStream) fail(err error) {
	st.failOnce.Do(func() {
		st.failErr = err
		close(st.failed)
	})
}

// openStream allocates a new stream_id and sends the request payload.
func (s *p2pSession) openStream(ctx context.Context, payload proto.Message) (*p2pStream, error) {
	s.mu.Lock()
	s.nextID++
	st := &p2pStream{s: s, id: s.nextID, ch: make(chan *ownsandv1.DataFrame, 64), closed: make(chan struct{}), failed: make(chan struct{})}
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

// recv returns the next frame of the stream, or the stream's failure or the
// session's end error after the queued frames.
func (st *p2pStream) recv(ctx context.Context) (*ownsandv1.DataFrame, error) {
	select {
	case f := <-st.ch:
		return f, nil
	default:
	}
	select {
	case f := <-st.ch:
		return f, nil
	case <-st.failed:
		select {
		case f := <-st.ch:
			return f, nil
		default:
		}
		return nil, st.failErr
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
