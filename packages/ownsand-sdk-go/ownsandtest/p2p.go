package ownsandtest

import (
	"context"
	"errors"
	"io"
	"sync"

	"connectrpc.com/connect"
	"github.com/delinoio/mono/packages/ownsand-sdk-go/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/pion/webrtc/v4"
)

// PeerOptions configure the peer-to-peer side of the fake agent.
type PeerOptions struct {
	// Setup adjusts the pion setting engine (for example, to allow
	// loopback candidates).
	Setup func(*webrtc.SettingEngine)
}

// EnablePeerToPeer makes OpenSession answer with a pion peer connection of
// the fake agent.
func (s *Server) EnablePeerToPeer(o PeerOptions) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.p2p = &peerAgent{s: s, opts: o}
}

// SetPeerUnreachable makes later OpenSession answers unusable: the fake
// agent closes its peer connection right after the answer, so the caller's
// data channel never opens.
func (s *Server) SetPeerUnreachable(v bool) {
	s.p2p.mu.Lock()
	s.p2p.unreachable = v
	s.p2p.mu.Unlock()
}

// EndPeerSessions sends a session-end frame with reason on every open data
// channel and closes it.
func (s *Server) EndPeerSessions(reason ownsandv1.SessionEndReason) {
	for _, ps := range s.p2p.sessions() {
		ps.endWith(reason)
	}
}

// FailPeerSessions closes every open data channel without a session-end frame.
func (s *Server) FailPeerSessions() {
	for _, ps := range s.p2p.sessions() {
		ps.cancel()
		_ = ps.pc.Close()
	}
}

// PeerRequests returns the kinds of the request frames that arrived on data
// channels.
func (s *Server) PeerRequests() []ownsandv1.DataFrameKind {
	s.p2p.mu.Lock()
	defer s.p2p.mu.Unlock()
	return append([]ownsandv1.DataFrameKind(nil), s.p2p.requests...)
}

type peerAgent struct {
	s           *Server
	opts        PeerOptions
	mu          sync.Mutex
	unreachable bool
	open        []*peerSession
	requests    []ownsandv1.DataFrameKind
}

func (p *peerAgent) sessions() []*peerSession {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*peerSession(nil), p.open...)
}

func (p *peerAgent) closeAll() {
	for _, ps := range p.sessions() {
		ps.cancel()
		_ = ps.pc.Close()
	}
}

func (p *peerAgent) answer(ctx context.Context, offer, sandbox string) (string, error) {
	se := webrtc.SettingEngine{}
	if p.opts.Setup != nil {
		p.opts.Setup(&se)
	}
	api := webrtc.NewAPI(webrtc.WithSettingEngine(se))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return "", err
	}
	sctx, cancel := context.WithCancel(context.Background())
	ps := &peerSession{p: p, pc: pc, sandbox: sandbox, ctx: sctx, cancel: cancel}
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		if dc.Label() != "ownsand" {
			return
		}
		ps.attach(dc)
	})
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer}); err != nil {
		_ = pc.Close()
		return "", err
	}
	ans, err := pc.CreateAnswer(nil)
	if err != nil {
		_ = pc.Close()
		return "", err
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(ans); err != nil {
		_ = pc.Close()
		return "", err
	}
	select {
	case <-gathered:
	case <-ctx.Done():
		_ = pc.Close()
		return "", ctx.Err()
	}
	sdp := pc.LocalDescription().SDP
	p.mu.Lock()
	unreachable := p.unreachable
	if !unreachable {
		p.open = append(p.open, ps)
	}
	p.mu.Unlock()
	if unreachable {
		cancel()
		_ = pc.Close()
	}
	return sdp, nil
}

type peerSession struct {
	p       *peerAgent
	pc      *webrtc.PeerConnection
	sandbox string
	ctx     context.Context
	cancel  context.CancelFunc
	dc      *webrtc.DataChannel
	writeMu sync.Mutex
}

func (ps *peerSession) attach(dc *webrtc.DataChannel) {
	ps.dc = dc
	pr, pw := io.Pipe()
	dc.OnMessage(func(m webrtc.DataChannelMessage) { _, _ = pw.Write(m.Data) })
	dc.OnClose(func() {
		ps.cancel()
		_ = pw.Close()
	})
	go func() {
		fr := datapath.NewFrameReader(pr)
		for {
			f, err := fr.ReadDataFrame()
			if err != nil {
				return
			}
			ps.p.mu.Lock()
			ps.p.requests = append(ps.p.requests, f.GetKind())
			ps.p.mu.Unlock()
			go ps.serve(f)
		}
	}()
}

func (ps *peerSession) send(f *ownsandv1.DataFrame) error {
	b, err := datapath.EncodeFrame(f)
	if err != nil {
		return err
	}
	ps.writeMu.Lock()
	defer ps.writeMu.Unlock()
	if ps.ctx.Err() != nil {
		return ps.ctx.Err()
	}
	for _, m := range datapath.SplitMessages(b) {
		if err := ps.dc.Send(m); err != nil {
			return err
		}
	}
	return nil
}

func (ps *peerSession) endWith(reason ownsandv1.SessionEndReason) {
	_ = ps.send(datapath.NewFrame(0, "", func(f *ownsandv1.DataFrame) {
		f.Payload = &ownsandv1.DataFrame_SessionEnd{SessionEnd: &ownsandv1.SessionEnd{Reason: reason}}
	}))
	ps.cancel()
	_ = ps.dc.Close()
	_ = ps.pc.Close()
}

func (ps *peerSession) serve(f *ownsandv1.DataFrame) {
	id := f.GetStreamId()
	frame := func(set func(*ownsandv1.DataFrame)) error {
		return ps.send(datapath.NewFrame(id, ps.sandbox, set))
	}
	execSend := func(ev *ownsandv1.ExecEvent) error {
		return frame(func(f *ownsandv1.DataFrame) { f.Payload = &ownsandv1.DataFrame_ExecEvent{ExecEvent: ev} })
	}
	noAccept := func() error { return nil }
	a := ps.p.s.Agent
	var err error
	if f.GetSandboxId() != ps.sandbox {
		err = connect.NewError(connect.CodePermissionDenied, errors.New("frame for another sandbox"))
	} else {
		switch p := f.GetPayload().(type) {
		case *ownsandv1.DataFrame_ExecRequest:
			err = a.Exec(ps.ctx, p.ExecRequest, noAccept, execSend)
		case *ownsandv1.DataFrame_ExecAttachRequest:
			err = a.Attach(ps.ctx, p.ExecAttachRequest, ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_ATTACH_EXEC, nil, noAccept, execSend)
		case *ownsandv1.DataFrame_FileReadRequest:
			err = a.Read(ps.ctx, p.FileReadRequest, noAccept, func(r *ownsandv1.ReadFileResponse) error {
				switch m := r.GetMessage().(type) {
				case *ownsandv1.ReadFileResponse_Chunk:
					return frame(func(f *ownsandv1.DataFrame) { f.Payload = &ownsandv1.DataFrame_FileChunk{FileChunk: m.Chunk} })
				case *ownsandv1.ReadFileResponse_Finished_:
					return frame(func(f *ownsandv1.DataFrame) {
						f.Payload = &ownsandv1.DataFrame_FileReadFinished{FileReadFinished: m.Finished}
					})
				}
				return nil
			})
		case *ownsandv1.DataFrame_FileWriteChunk:
			var resp *ownsandv1.WriteFileResponse
			resp, err = a.Write(p.FileWriteChunk, nil, false)
			if err == nil {
				err = frame(func(f *ownsandv1.DataFrame) { f.Payload = &ownsandv1.DataFrame_FileWriteResult{FileWriteResult: resp} })
			}
		default:
			err = connect.NewError(connect.CodeInvalidArgument, errors.New("unexpected frame kind"))
		}
	}
	if err != nil && ps.ctx.Err() == nil {
		_ = frame(func(f *ownsandv1.DataFrame) {
			f.Payload = &ownsandv1.DataFrame_Error{Error: &ownsandv1.FrameError{Code: connect.CodeOf(err).String(), Message: err.Error()}}
		})
	}
}
