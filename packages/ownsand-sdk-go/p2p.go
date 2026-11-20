package ownsand

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/delinoio/mono/packages/ownsand-sdk-go/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/pion/webrtc/v4"
)

// DataChannelLabel is the label of the OwnSand data channel. The caller
// creates one ordered, reliable data channel per session.
const DataChannelLabel = "ownsand"

// peerOpenTimeout bounds ICE gathering and, separately, the data channel
// open after the answer arrived.
const peerOpenTimeout = 10 * time.Second

type peerSetupFunc func(*webrtc.SettingEngine)

var errPeerOpenTimeout = errors.New("data channel did not open within 10 seconds")

// errChannelFailed reports a data channel that closed or failed without a
// session-end frame.
var errChannelFailed = errors.New("data channel failed without a session end")

// sessionEndedError reports a session that the agent ended with a
// session-end frame.
type sessionEndedError struct{ reason ownsandv1.SessionEndReason }

func (e *sessionEndedError) Error() string { return "data channel session ended: " + e.reason.String() }

func frameError(fe *ownsandv1.FrameError) error {
	var code connect.Code
	if err := code.UnmarshalText([]byte(fe.GetCode())); err != nil {
		code = connect.CodeUnknown
	}
	return &remoteError{code: code, msg: fe.GetMessage()}
}

// dataPath selects the transport of one sandbox handle and holds its
// peer-to-peer session.
type dataPath struct {
	c         *Client
	org       string
	sandboxID string

	mu        sync.Mutex
	relayOnly bool
	sess      *p2pSession
	cur       ownsandv1.DataPathTransport
}

func newDataPath(c *Client, org, sandboxID string) *dataPath {
	return &dataPath{c: c, org: org, sandboxID: sandboxID}
}

func (d *dataPath) current() ownsandv1.DataPathTransport {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.cur
}

func (d *dataPath) close() {
	d.mu.Lock()
	s := d.sess
	d.sess = nil
	d.mu.Unlock()
	if s != nil {
		s.close()
	}
}

// session returns the live peer-to-peer session, or nil when the handle
// uses the relay. It opens a session only when an operation needs one, so
// an idle handle never keeps the sandbox active.
func (d *dataPath) session(ctx context.Context) (*p2pSession, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.c.transport == TransportRelay || d.relayOnly {
		d.setCur(ownsandv1.DataPathTransport_DATA_PATH_TRANSPORT_RELAY)
		return nil, nil
	}
	if d.sess != nil && !d.sess.isDone() {
		return d.sess, nil
	}
	if d.sess != nil {
		d.sess.close()
		d.sess = nil
	}
	sess, err := d.openSession(ctx)
	if err != nil {
		f := classify(ctx, err)
		if f.canceled || d.c.transport == TransportPeerToPeer {
			return nil, err
		}
		if f.remote && (f.code == connect.CodeUnauthenticated || f.code == connect.CodePermissionDenied || f.code == connect.CodeNotFound) {
			return nil, err
		}
		d.c.log.Info("peer-to-peer data channel unavailable; using the relay",
			slog.String("sandbox_id", d.sandboxID), slog.String("code", f.code.String()))
		d.relayOnly = true
		d.setCur(ownsandv1.DataPathTransport_DATA_PATH_TRANSPORT_RELAY)
		return nil, nil
	}
	d.sess = sess
	d.setCur(ownsandv1.DataPathTransport_DATA_PATH_TRANSPORT_PEER_TO_PEER)
	return sess, nil
}

func (d *dataPath) setCur(t ownsandv1.DataPathTransport) {
	if d.cur != t {
		d.c.log.Info("data path transport selected", slog.String("sandbox_id", d.sandboxID), slog.String("transport", t.String()))
	}
	d.cur = t
}

// handleSessionError handles the end of a session. A session end opens a
// new session for the next request; a failed channel moves the handle to
// the relay. It reports whether err was such an end.
func (d *dataPath) handleSessionError(s *p2pSession, err error) bool {
	var se *sessionEndedError
	switch {
	case errors.As(err, &se):
		d.c.log.Info("data channel session ended", slog.String("sandbox_id", d.sandboxID), slog.String("reason", se.reason.String()))
		d.mu.Lock()
		if d.sess == s {
			d.sess = nil
		}
		d.mu.Unlock()
		s.close()
		return true
	case errors.Is(err, errChannelFailed):
		d.mu.Lock()
		if d.sess == s {
			d.sess = nil
		}
		if d.c.transport != TransportPeerToPeer {
			d.c.log.Info("data channel failed; using the relay", slog.String("sandbox_id", d.sandboxID))
			d.relayOnly = true
			d.setCur(ownsandv1.DataPathTransport_DATA_PATH_TRANSPORT_RELAY)
		}
		d.mu.Unlock()
		s.close()
		return true
	}
	return false
}

func (d *dataPath) openSession(ctx context.Context) (*p2pSession, error) {
	c := d.c
	iceResp, err := c.dataPath.GetIceServers(ctx, connect.NewRequest(&ownsandv1.GetIceServersRequest{OrganizationId: d.org}))
	if err != nil {
		return nil, err
	}
	var servers []webrtc.ICEServer
	for _, s := range iceResp.Msg.GetIceServers() {
		servers = append(servers, webrtc.ICEServer{URLs: s.GetUrls()})
	}
	se := webrtc.SettingEngine{}
	if c.peerSetup != nil {
		c.peerSetup(&se)
	}
	api := webrtc.NewAPI(webrtc.WithSettingEngine(se))
	pc, err := api.NewPeerConnection(webrtc.Configuration{ICEServers: servers})
	if err != nil {
		return nil, fmt.Errorf("create peer connection: %w", err)
	}
	ordered := true
	dc, err := pc.CreateDataChannel(DataChannelLabel, &webrtc.DataChannelInit{Ordered: &ordered})
	if err != nil {
		_ = pc.Close()
		return nil, fmt.Errorf("create data channel: %w", err)
	}
	s := newP2PSession(c, d.sandboxID, pc, dc)
	opened := make(chan struct{})
	dc.OnOpen(func() { close(opened) })

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		s.close()
		return nil, fmt.Errorf("create offer: %w", err)
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		s.close()
		return nil, fmt.Errorf("set local description: %w", err)
	}
	// Signaling is non-trickle: the offer carries every candidate.
	if err := d.waitOn(ctx, gathered); err != nil {
		s.close()
		return nil, err
	}
	resp, err := c.dataPath.OpenSession(ctx, connect.NewRequest(&ownsandv1.OpenSessionRequest{
		OrganizationId: d.org, SandboxId: d.sandboxID, SdpOffer: pc.LocalDescription().SDP,
	}))
	if err != nil {
		s.close()
		return nil, err
	}
	// pion accepts only the DTLS fingerprint of this answer, which arrived
	// over the authenticated control plane connection.
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: resp.Msg.GetSdpAnswer()}); err != nil {
		s.close()
		return nil, fmt.Errorf("set remote description: %w", err)
	}
	if err := d.waitOn(ctx, opened); err != nil {
		s.close()
		return nil, err
	}
	s.sessionID = resp.Msg.GetSessionId()
	c.log.Info("data channel opened", slog.String("sandbox_id", d.sandboxID), slog.String("session_id", s.sessionID))
	return s, nil
}

func (d *dataPath) waitOn(ctx context.Context, ch <-chan struct{}) error {
	t := d.c.clk.NewTimer(peerOpenTimeout)
	defer t.Stop()
	select {
	case <-ch:
		return nil
	case <-t.C():
		return errPeerOpenTimeout
	case <-ctx.Done():
		return ctx.Err()
	}
}

// p2pSession is one OpenSession session: one data channel that carries
// length-prefixed DataFrame messages for several concurrent requests.
type p2pSession struct {
	c         *Client
	sandboxID string
	sessionID string
	pc        *webrtc.PeerConnection
	dc        *webrtc.DataChannel

	pw *io.PipeWriter

	writeMu sync.Mutex

	mu      sync.Mutex
	nextID  uint64
	streams map[uint64]*p2pStream
	done    chan struct{}
	endErr  error
}

func newP2PSession(c *Client, sandboxID string, pc *webrtc.PeerConnection, dc *webrtc.DataChannel) *p2pSession {
	pr, pw := io.Pipe()
	s := &p2pSession{c: c, sandboxID: sandboxID, pc: pc, dc: dc, pw: pw, streams: map[uint64]*p2pStream{}, done: make(chan struct{})}
	dc.OnMessage(func(m webrtc.DataChannelMessage) {
		if _, err := pw.Write(m.Data); err != nil {
			return
		}
	})
	dc.OnClose(func() { s.end(errChannelFailed) })
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		if st == webrtc.PeerConnectionStateFailed || st == webrtc.PeerConnectionStateClosed {
			s.end(errChannelFailed)
		}
	})
	go s.readLoop(pr)
	return s
}

func (s *p2pSession) readLoop(r io.Reader) {
	fr := datapath.NewFrameReader(r)
	for {
		f, err := fr.ReadDataFrame()
		if err != nil {
			// A frame above the size limit or a broken stream ends the
			// channel.
			s.end(errChannelFailed)
			return
		}
		if se, ok := f.GetPayload().(*ownsandv1.DataFrame_SessionEnd); ok {
			s.end(&sessionEndedError{reason: se.SessionEnd.GetReason()})
			return
		}
		if f.GetStreamId() == 0 {
			continue
		}
		s.mu.Lock()
		st := s.streams[f.GetStreamId()]
		s.mu.Unlock()
		if st == nil {
			continue
		}
		select {
		case st.ch <- f:
		case <-st.closed:
		case <-s.done:
			return
		}
	}
}

func (s *p2pSession) end(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.done:
		return
	default:
	}
	s.endErr = err
	close(s.done)
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
	s.end(&sessionEndedError{reason: ownsandv1.SessionEndReason_SESSION_END_REASON_UNSPECIFIED})
	_ = s.pw.Close()
	_ = s.dc.Close()
	_ = s.pc.Close()
}

// open sends the first request frame of a new stream.
func (s *p2pSession) open(set func(*ownsandv1.DataFrame)) (*p2pStream, error) {
	s.mu.Lock()
	if s.isDone() {
		err := s.endErr
		s.mu.Unlock()
		return nil, err
	}
	s.nextID++
	st := &p2pStream{s: s, id: s.nextID, ch: make(chan *ownsandv1.DataFrame, 64), closed: make(chan struct{})}
	s.streams[st.id] = st
	s.mu.Unlock()
	f := datapath.NewFrame(st.id, s.sandboxID, set)
	if err := s.send(f); err != nil {
		st.close()
		return nil, err
	}
	return st, nil
}

func (s *p2pSession) send(f *ownsandv1.DataFrame) error {
	b, err := datapath.EncodeFrame(f)
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	for _, m := range datapath.SplitMessages(b) {
		if err := s.dc.Send(m); err != nil {
			s.end(errChannelFailed)
			return errChannelFailed
		}
	}
	return nil
}

type p2pStream struct {
	s      *p2pSession
	id     uint64
	ch     chan *ownsandv1.DataFrame
	closed chan struct{}
	once   sync.Once
}

// recv returns the next frame of the stream, or the session end.
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
		st.s.mu.Lock()
		err := st.s.endErr
		st.s.mu.Unlock()
		return nil, err
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
