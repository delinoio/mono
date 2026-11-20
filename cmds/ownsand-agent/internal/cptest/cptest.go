// Package cptest is an in-process fake of the control plane for agent tests:
// AgentService (enrollment, challenges, authentication, removal and the
// agent channel) served with Connect handlers on an httptest server that
// speaks HTTP/2 without TLS (h2c). Only _test.go files import it.
package cptest

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/controlplane"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// DeviceState is the fake device state.
type DeviceState int

const (
	DeviceEnrolled DeviceState = iota + 1
	DeviceRemoved
	DeviceUnknown
)

// Channel is one open agent channel on the fake.
type Channel struct {
	Generation uint64
	Credential string
	Received   chan *ownsandv1.AgentEnvelope
	send       chan *ownsandv1.AgentEnvelope
	cancel     context.CancelFunc
	done       chan struct{}
}

// Send sends an envelope to the agent.
func (c *Channel) Send(env *ownsandv1.AgentEnvelope) { c.send <- env }

// Close closes the channel from the server side.
func (c *Channel) Close() { c.cancel() }

// Done is closed when the channel ended.
func (c *Channel) Done() <-chan struct{} { return c.done }

// Server is the fake control plane.
type Server struct {
	t   testing.TB
	srv *httptest.Server
	URL string

	mu             sync.Mutex
	tokens         map[string]bool
	enrolled       map[string]string // token -> device ID
	DeviceID       string
	PublicKey      ed25519.PublicKey
	State          DeviceState
	credentials    map[string]bool
	AuthCount      int
	ConfirmCount   int
	Epochs         []string
	gen            uint64
	channels       chan *Channel
	current        *Channel
	AckHeartbeats  bool
	ExtraHandlers  map[string]http.Handler
	ChannelHeaders []http.Header
}

// New starts a fake control plane. extra registers more Connect handlers
// (for example, RelayAgentService) by path prefix.
func New(t testing.TB, extra func(s *Server, mux *http.ServeMux)) *Server {
	s := &Server{
		t:             t,
		tokens:        map[string]bool{},
		enrolled:      map[string]string{},
		credentials:   map[string]bool{},
		channels:      make(chan *Channel, 16),
		AckHeartbeats: true,
		State:         DeviceUnknown,
	}
	mux := http.NewServeMux()
	path, h := ownsandv1connect.NewAgentServiceHandler(s)
	mux.Handle(path, h)
	if extra != nil {
		extra(s, mux)
	}
	srv := httptest.NewUnstartedServer(mux)
	var p http.Protocols
	p.SetHTTP1(true)
	p.SetUnencryptedHTTP2(true)
	srv.Config.Protocols = &p
	srv.Start()
	t.Cleanup(srv.Close)
	s.srv = srv
	s.URL = srv.URL
	return s
}

// AddToken adds a valid enrollment token.
func (s *Server) AddToken(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens[token] = true
}

// AddDevice registers an enrolled device.
func (s *Server) AddDevice(deviceID string, pub ed25519.PublicKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.DeviceID, s.PublicKey, s.State = deviceID, pub, DeviceEnrolled
}

// SetState changes the device state.
func (s *Server) SetState(st DeviceState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.State = st
}

// SetAckHeartbeats turns heartbeat acknowledgements on or off.
func (s *Server) SetAckHeartbeats(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.AckHeartbeats = on
}

// Counts returns the authentication and confirmation counts.
func (s *Server) Counts() (auth, confirm int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.AuthCount, s.ConfirmCount
}

// NextChannel waits for the next agent channel.
func (s *Server) NextChannel(timeout time.Duration) *Channel {
	select {
	case c := <-s.channels:
		return c
	case <-time.After(timeout):
		s.t.Fatalf("no agent channel within %v", timeout)
		return nil
	}
}

// ValidCredential reports whether a credential was issued and the device is
// not removed.
func (s *Server) ValidCredential(cred string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.credentials[cred] && s.State == DeviceEnrolled
}

// CurrentGeneration returns the generation of the current channel.
func (s *Server) CurrentGeneration() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == nil {
		return 0
	}
	return s.current.Generation
}

func (s *Server) Enroll(_ context.Context, req *connect.Request[ownsandv1.EnrollRequest]) (*connect.Response[ownsandv1.EnrollResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tok := req.Msg.GetEnrollmentToken()
	if id, ok := s.enrolled[tok]; ok {
		if ed25519.PublicKey(req.Msg.GetPublicKey()).Equal(s.PublicKey) {
			return connect.NewResponse(&ownsandv1.EnrollResponse{DeviceId: id, OrganizationId: "org"}), nil
		}
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("invalid token"))
	}
	if !s.tokens[tok] {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("invalid token"))
	}
	delete(s.tokens, tok)
	s.DeviceID = "0190a000-0000-7000-8000-000000000001"
	s.PublicKey = append(ed25519.PublicKey(nil), req.Msg.GetPublicKey()...)
	s.State = DeviceEnrolled
	s.enrolled[tok] = s.DeviceID
	return connect.NewResponse(&ownsandv1.EnrollResponse{DeviceId: s.DeviceID, OrganizationId: "org"}), nil
}

func (s *Server) CreateChallenge(_ context.Context, req *connect.Request[ownsandv1.CreateChallengeRequest]) (*connect.Response[ownsandv1.CreateChallengeResponse], error) {
	nonce := make([]byte, 32)
	_, _ = rand.Read(nonce)
	return connect.NewResponse(&ownsandv1.CreateChallengeResponse{Nonce: nonce, ExpireTime: timestamppb.New(time.Now().Add(time.Minute))}), nil
}

func (s *Server) check(deviceID string, nonce, sig []byte, label string) error {
	if s.State == DeviceUnknown || deviceID != s.DeviceID {
		return connect.NewError(connect.CodeNotFound, errors.New("unknown device"))
	}
	if !ed25519.Verify(s.PublicKey, append([]byte(label), nonce...), sig) {
		return connect.NewError(connect.CodeUnauthenticated, errors.New("bad signature"))
	}
	return nil
}

func (s *Server) Authenticate(_ context.Context, req *connect.Request[ownsandv1.AuthenticateRequest]) (*connect.Response[ownsandv1.AuthenticateResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(req.Msg.GetDeviceId(), req.Msg.GetNonce(), req.Msg.GetSignature(), controlplane.AuthenticateLabel); err != nil {
		return nil, err
	}
	s.AuthCount++
	s.Epochs = append(s.Epochs, req.Msg.GetStateEpoch())
	if s.State == DeviceRemoved {
		return connect.NewResponse(&ownsandv1.AuthenticateResponse{Result: &ownsandv1.AuthenticateResponse_RemovalNotice{RemovalNotice: &ownsandv1.RemovalNotice{}}}), nil
	}
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	cred := "osd_" + base64.RawURLEncoding.EncodeToString(b)
	s.credentials[cred] = true
	return connect.NewResponse(&ownsandv1.AuthenticateResponse{
		Result:     &ownsandv1.AuthenticateResponse_DeviceSessionCredential{DeviceSessionCredential: cred},
		ExpireTime: timestamppb.New(time.Now().Add(15 * time.Minute)),
	}), nil
}

func (s *Server) ConfirmRemoval(_ context.Context, req *connect.Request[ownsandv1.ConfirmRemovalRequest]) (*connect.Response[ownsandv1.ConfirmRemovalResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(req.Msg.GetDeviceId(), req.Msg.GetNonce(), req.Msg.GetSignature(), controlplane.ConfirmRemovalLabel); err != nil {
		return nil, err
	}
	if s.State != DeviceRemoved {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("device is not removed"))
	}
	s.ConfirmCount++
	s.State = DeviceUnknown
	return connect.NewResponse(&ownsandv1.ConfirmRemovalResponse{}), nil
}

// BearerCredential extracts the credential of an Authorization header.
func BearerCredential(h http.Header) string {
	const p = "Bearer "
	v := h.Get("Authorization")
	if len(v) < len(p) || v[:len(p)] != p {
		return ""
	}
	return v[len(p):]
}

func (s *Server) Channel(ctx context.Context, stream *connect.BidiStream[ownsandv1.AgentEnvelope, ownsandv1.AgentEnvelope]) error {
	cred := BearerCredential(stream.RequestHeader())
	if !s.ValidCredential(cred) {
		return connect.NewError(connect.CodeUnauthenticated, errors.New("invalid credential"))
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.mu.Lock()
	s.gen++
	ch := &Channel{Generation: s.gen, Credential: cred, Received: make(chan *ownsandv1.AgentEnvelope, 1024), send: make(chan *ownsandv1.AgentEnvelope, 1024), cancel: cancel, done: make(chan struct{})}
	if s.current != nil {
		s.current.cancel()
	}
	s.current = ch
	s.ChannelHeaders = append(s.ChannelHeaders, stream.RequestHeader().Clone())
	s.mu.Unlock()
	defer close(ch.done)
	s.channels <- ch
	errc := make(chan error, 1)
	go func() {
		for {
			env, err := stream.Receive()
			if err != nil {
				errc <- err
				return
			}
			if hb := env.GetHeartbeat(); hb != nil {
				s.mu.Lock()
				ack := s.AckHeartbeats
				s.mu.Unlock()
				if ack {
					ch.send <- &ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_HeartbeatAck{HeartbeatAck: &ownsandv1.HeartbeatAck{Sequence: hb.GetSequence()}}}
				}
				continue
			}
			select {
			case ch.Received <- env:
			default:
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-errc:
			return nil
		case env := <-ch.send:
			if err := stream.Send(env); err != nil {
				return nil
			}
		}
	}
}
