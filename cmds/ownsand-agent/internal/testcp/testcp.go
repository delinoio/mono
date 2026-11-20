// SPDX-License-Identifier: Apache-2.0

// Package testcp is an in-process fake of the control plane's
// device-facing services (AgentService and RelayAgentService) for agent
// tests. It serves Connect over HTTP/2 without TLS, like the control plane
// behind its h2c route.
package testcp

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/identity"
	"github.com/delinoio/mono/packages/ownsand-sdk-go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
)

// Token is the enrollment token that the fake accepts.
const Token = "ose_test-token"

// OrganizationID is the organization of every enrolled device.
const OrganizationID = "0192f4b6-7c3a-7def-8abc-0123456789ab"

type device struct {
	pub     ed25519.PublicKey
	removed bool
}

// Channel is one open agent channel on the fake.
type Channel struct {
	DeviceID string
	out      chan *ownsandv1.AgentEnvelope
	done     chan struct{}
	once     sync.Once
}

// Close ends the channel from the control plane side.
func (c *Channel) Close() { c.once.Do(func() { close(c.done) }) }

// Server is the fake control plane.
type Server struct {
	URL string
	t   testing.TB
	srv *httptest.Server

	mu         sync.Mutex
	devices    map[string]*device
	nonces     map[string]bool
	sessions   map[string]string
	channel    *Channel
	channels   int
	received   []*ownsandv1.AgentEnvelope
	notify     chan struct{}
	AckHB      bool
	AckState   bool
	confirms   int
	authCount  int
	relayHook  func(ctx context.Context, deviceID string, stream *connect.BidiStream[ownsandv1.ConnectRequest, ownsandv1.ConnectResponse]) error
	relayCreds []string
	urlBase    string
}

// SetSignedURLBase makes the fake answer signed URL requests with URLs
// under base (a fake bucket).
func (s *Server) SetSignedURLBase(base string) {
	s.mu.Lock()
	s.urlBase = base
	s.mu.Unlock()
}

// New starts a fake control plane.
func New(t testing.TB) *Server {
	s := &Server{t: t, devices: map[string]*device{}, nonces: map[string]bool{}, sessions: map[string]string{}, notify: make(chan struct{}), AckHB: true, AckState: true}
	mux := http.NewServeMux()
	mux.Handle(ownsandv1connect.NewAgentServiceHandler(s))
	mux.Handle(ownsandv1connect.NewRelayAgentServiceHandler(relayHandler{s}))
	s.srv = httptest.NewUnstartedServer(mux)
	var p http.Protocols
	p.SetHTTP1(true)
	p.SetUnencryptedHTTP2(true)
	s.srv.Config.Protocols = &p
	s.srv.Start()
	s.URL = s.srv.URL
	t.Cleanup(s.Close)
	return s
}

// Close stops the server.
func (s *Server) Close() {
	s.mu.Lock()
	if s.channel != nil {
		s.channel.Close()
	}
	s.mu.Unlock()
	s.srv.CloseClientConnections()
	s.srv.Close()
}

func randomToken(prefix string) string {
	b := make([]byte, 32)
	rand.Read(b)
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}

// Enroll implements AgentService.
func (s *Server) Enroll(_ context.Context, req *connect.Request[ownsandv1.EnrollRequest]) (*connect.Response[ownsandv1.EnrollResponse], error) {
	if req.Msg.GetEnrollmentToken() != Token || len(req.Msg.GetPublicKey()) != ed25519.PublicKeySize {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("invalid enrollment token"))
	}
	id, _ := uuidv7.NewString()
	s.mu.Lock()
	s.devices[id] = &device{pub: req.Msg.GetPublicKey()}
	s.mu.Unlock()
	return connect.NewResponse(&ownsandv1.EnrollResponse{DeviceId: id, OrganizationId: OrganizationID}), nil
}

// CreateChallenge implements AgentService.
func (s *Server) CreateChallenge(_ context.Context, req *connect.Request[ownsandv1.CreateChallengeRequest]) (*connect.Response[ownsandv1.CreateChallengeResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.devices[req.Msg.GetDeviceId()]; !ok {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("unknown device"))
	}
	nonce := make([]byte, 32)
	rand.Read(nonce)
	s.nonces[string(nonce)] = true
	return connect.NewResponse(&ownsandv1.CreateChallengeResponse{Nonce: nonce, ExpireTime: timestamppb.New(time.Now().Add(time.Minute))}), nil
}

func (s *Server) checkSignature(deviceID string, nonce, sig []byte, purpose string) (*device, error) {
	d, ok := s.devices[deviceID]
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("unknown device"))
	}
	if !s.nonces[string(nonce)] {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("unknown nonce"))
	}
	delete(s.nonces, string(nonce))
	if !ed25519.Verify(d.pub, identity.Message(purpose, nonce), sig) {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("bad signature"))
	}
	return d, nil
}

// Authenticate implements AgentService.
func (s *Server) Authenticate(_ context.Context, req *connect.Request[ownsandv1.AuthenticateRequest]) (*connect.Response[ownsandv1.AuthenticateResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.checkSignature(req.Msg.GetDeviceId(), req.Msg.GetNonce(), req.Msg.GetSignature(), identity.PurposeAuthenticate)
	if err != nil {
		return nil, err
	}
	s.authCount++
	if d.removed {
		return connect.NewResponse(&ownsandv1.AuthenticateResponse{Result: &ownsandv1.AuthenticateResponse_RemovalNotice{RemovalNotice: &ownsandv1.RemovalNotice{RemoveTime: timestamppb.Now()}}}), nil
	}
	cred := randomToken("osd_")
	s.sessions[cred] = req.Msg.GetDeviceId()
	return connect.NewResponse(&ownsandv1.AuthenticateResponse{Result: &ownsandv1.AuthenticateResponse_Session{Session: &ownsandv1.DeviceSession{
		DeviceSessionCredential: cred, ExpireTime: timestamppb.New(time.Now().Add(15 * time.Minute)), ExpiresIn: durationpb.New(15 * time.Minute), OrganizationId: OrganizationID,
	}}}), nil
}

// ConfirmRemoval implements AgentService.
func (s *Server) ConfirmRemoval(_ context.Context, req *connect.Request[ownsandv1.ConfirmRemovalRequest]) (*connect.Response[ownsandv1.ConfirmRemovalResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.checkSignature(req.Msg.GetDeviceId(), req.Msg.GetNonce(), req.Msg.GetSignature(), identity.PurposeConfirmRemoval)
	if err != nil {
		return nil, err
	}
	if !d.removed {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("device is not removed"))
	}
	delete(s.devices, req.Msg.GetDeviceId())
	s.confirms++
	return connect.NewResponse(&ownsandv1.ConfirmRemovalResponse{}), nil
}

// credential checks the bearer device session credential; each one opens
// one connection.
func (s *Server) credential(h http.Header) (string, error) {
	cred, ok := strings.CutPrefix(h.Get("Authorization"), "Bearer ")
	s.mu.Lock()
	defer s.mu.Unlock()
	id, known := s.sessions[cred]
	if !ok || !known {
		return "", connect.NewError(connect.CodeUnauthenticated, errors.New("invalid device session credential"))
	}
	delete(s.sessions, cred)
	return id, nil
}

// Channel implements AgentService.
func (s *Server) Channel(ctx context.Context, stream *connect.BidiStream[ownsandv1.ChannelRequest, ownsandv1.ChannelResponse]) error {
	deviceID, err := s.credential(stream.RequestHeader())
	if err != nil {
		return err
	}
	ch := &Channel{DeviceID: deviceID, out: make(chan *ownsandv1.AgentEnvelope, 256), done: make(chan struct{})}
	s.mu.Lock()
	if s.channel != nil {
		s.channel.Close()
	}
	s.channel = ch
	s.channels++
	s.mu.Unlock()
	var senders sync.WaitGroup
	defer senders.Wait()
	defer ch.Close()
	senders.Add(1)
	go func() {
		defer senders.Done()
		for {
			select {
			case <-ch.done:
				return
			case <-ctx.Done():
				return
			case env := <-ch.out:
				if stream.Send(&ownsandv1.ChannelResponse{Envelope: env}) != nil {
					ch.Close()
					return
				}
			}
		}
	}()
	recvErr := make(chan error, 1)
	go func() {
		for {
			req, err := stream.Receive()
			if err != nil {
				recvErr <- err
				return
			}
			s.record(ch, req.GetEnvelope())
		}
	}()
	select {
	case <-ch.done:
		return connect.NewError(connect.CodeUnavailable, errors.New("channel closed by the control plane"))
	case <-recvErr:
		return nil
	case <-ctx.Done():
		return nil
	}
}

func (s *Server) record(ch *Channel, env *ownsandv1.AgentEnvelope) {
	s.mu.Lock()
	s.received = append(s.received, env)
	close(s.notify)
	s.notify = make(chan struct{})
	ackHB, ackState, urlBase := s.AckHB, s.AckState, s.urlBase
	s.mu.Unlock()
	switch m := env.GetMessage().(type) {
	case *ownsandv1.AgentEnvelope_SignedUrlRequest:
		if urlBase != "" {
			req := m.SignedUrlRequest
			grant := &ownsandv1.SignedUrlGrant{RequestId: req.GetRequestId(), SnapshotId: req.GetSnapshotId(), Operation: req.GetOperation()}
			for _, p := range req.GetPartNumbers() {
				grant.PartUrls = append(grant.PartUrls, &ownsandv1.SignedPartUrl{PartNumber: p, Url: fmt.Sprintf("%s/%s/%d", urlBase, req.GetSnapshotId(), p), ExpireTime: timestamppb.New(time.Now().Add(15 * time.Minute))})
			}
			s.sendOn(ch, &ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_SignedUrlGrant{SignedUrlGrant: grant}})
		}
	case *ownsandv1.AgentEnvelope_Heartbeat:
		if ackHB {
			s.sendOn(ch, &ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_HeartbeatAck{HeartbeatAck: &ownsandv1.HeartbeatAck{Sequence: m.Heartbeat.GetSequence()}}})
		}
	case *ownsandv1.AgentEnvelope_StateReport:
		if ackState {
			ack := &ownsandv1.StateReportAck{ReportSequence: m.StateReport.GetReportSequence()}
			for _, e := range m.StateReport.GetSandboxes() {
				ack.Sandboxes = append(ack.Sandboxes, &ownsandv1.SandboxUpdateAck{SandboxId: e.GetSandboxId(), Revision: e.GetRevision()})
			}
			s.sendOn(ch, &ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_StateAck{StateAck: &ownsandv1.StateAck{Ack: &ownsandv1.StateAck_StateReport{StateReport: ack}}}})
		}
	case *ownsandv1.AgentEnvelope_SandboxUpdate:
		if ackState {
			e := m.SandboxUpdate.GetSandbox()
			s.sendOn(ch, &ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_StateAck{StateAck: &ownsandv1.StateAck{Ack: &ownsandv1.StateAck_SandboxUpdate{SandboxUpdate: &ownsandv1.SandboxUpdateAck{SandboxId: e.GetSandboxId(), Revision: e.GetRevision()}}}}})
		}
	}
}

func (s *Server) sendOn(ch *Channel, env *ownsandv1.AgentEnvelope) {
	select {
	case ch.out <- env:
	case <-ch.done:
	}
}

// Send sends an envelope on the current channel.
func (s *Server) Send(env *ownsandv1.AgentEnvelope) bool {
	s.mu.Lock()
	ch := s.channel
	s.mu.Unlock()
	if ch == nil {
		return false
	}
	select {
	case ch.out <- env:
		return true
	case <-ch.done:
		return false
	}
}

// Current returns the current channel.
func (s *Server) Current() *Channel {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.channel
}

// Channels returns how many channels opened.
func (s *Server) Channels() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.channels
}

// Confirms returns how many removals were confirmed.
func (s *Server) Confirms() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.confirms
}

// SetAck sets whether the fake acknowledges heartbeats and state.
func (s *Server) SetAck(heartbeats, state bool) {
	s.mu.Lock()
	s.AckHB, s.AckState = heartbeats, state
	s.mu.Unlock()
}

// MarkRemoved marks a device removed: Authenticate returns the removal
// notice.
func (s *Server) MarkRemoved(deviceID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d, ok := s.devices[deviceID]; ok {
		d.removed = true
	}
}

// Forget deletes a device record without a tombstone.
func (s *Server) Forget(deviceID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.devices, deviceID)
}

// WaitFor waits until an envelope received after index from matches and
// returns it with its index.
func (s *Server) WaitFor(timeout time.Duration, from int, match func(*ownsandv1.AgentEnvelope) bool) (*ownsandv1.AgentEnvelope, int) {
	deadline := time.After(timeout)
	for {
		s.mu.Lock()
		for i := from; i < len(s.received); i++ {
			if match(s.received[i]) {
				env := proto.Clone(s.received[i]).(*ownsandv1.AgentEnvelope)
				s.mu.Unlock()
				return env, i
			}
		}
		notify := s.notify
		s.mu.Unlock()
		select {
		case <-notify:
		case <-deadline:
			return nil, -1
		}
	}
}

// Received returns the number of received envelopes.
func (s *Server) Received() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.received)
}

// CommandResult waits for the result of a command.
func (s *Server) CommandResult(timeout time.Duration, commandID string) *ownsandv1.AgentCommandResult {
	env, _ := s.WaitFor(timeout, 0, func(e *ownsandv1.AgentEnvelope) bool {
		return e.GetCommandResult().GetCommandId() == commandID
	})
	return env.GetCommandResult()
}

// SetRelayHandler sets the handler of RelayAgentService.Connect.
func (s *Server) SetRelayHandler(h func(ctx context.Context, deviceID string, stream *connect.BidiStream[ownsandv1.ConnectRequest, ownsandv1.ConnectResponse]) error) {
	s.mu.Lock()
	s.relayHook = h
	s.mu.Unlock()
}

// RelayCredentials returns the credentials of the relay connections.
func (s *Server) RelayCredentials() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.relayCreds...)
}

type relayHandler struct{ s *Server }

func (r relayHandler) Connect(ctx context.Context, stream *connect.BidiStream[ownsandv1.ConnectRequest, ownsandv1.ConnectResponse]) error {
	cred, _ := strings.CutPrefix(stream.RequestHeader().Get("Authorization"), "Bearer ")
	deviceID, err := r.s.credential(stream.RequestHeader())
	if err != nil {
		return err
	}
	r.s.mu.Lock()
	r.s.relayCreds = append(r.s.relayCreds, cred)
	hook := r.s.relayHook
	r.s.mu.Unlock()
	if hook == nil {
		return connect.NewError(connect.CodeUnimplemented, errors.New("no relay handler"))
	}
	return hook(ctx, deviceID, stream)
}
