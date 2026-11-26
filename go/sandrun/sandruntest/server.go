// SPDX-License-Identifier: Apache-2.0

// Package sandruntest is an in-memory fake of sandrun-api for tests of programs
// that use the SandRun Go SDK. It serves every public service of
// delino.sandrun.v1 over an httptest server; services and RPCs that tests do
// not need return unimplemented.
//
// The fake guest of a dev session is the local machine: an exec runs the
// command as a local process, and file operations read and write local
// paths. A dev session profile therefore uses a local absolute directory
// as its workspace path, and sync and download tests run the SDK's guest
// commands with the local sh and tar. It is a test fake, not a sandbox:
// never point it at directories that a test does not own.
package sandruntest

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/go/uuidv7"
	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/protos/delino/sandrun/v1/sandrunv1connect"
)

// Default credentials of a new Server.
const (
	DefaultAccessToken = "test-access-token"
	DefaultAPIKey      = "sandrun_test-personal-api-key"
	DefaultUserID      = "user-1"
)

// CredentialKind is the kind of the credential of a request.
type CredentialKind int

const (
	CredentialNone CredentialKind = iota
	CredentialAccessToken
	CredentialAPIKey
)

// Server is a fake sandrun-api. Configure the exported fields before the
// first request; they are not synchronized.
type Server struct {
	// URL is the origin of the server, for example http://127.0.0.1:1234.
	URL string
	// AccessToken is the accepted Logto access token. APIKey is the
	// built-in fixture key. Active, unexpired keys issued by ApiKeyService
	// also work on DevSessionService in the organization that issued them.
	AccessToken string
	APIKey      string
	// UserID is the user associated with all accepted credentials.
	UserID string
	// ExecStreamEventLimit ends every exec and attach stream with an OK
	// status after this many events, like the 14-minute limit of sandrun-api.
	// Zero means no limit.
	ExecStreamEventLimit int
	// OutputKeepBytes keeps only the last bytes of each exec output stream,
	// like the 8 MiB of the OwnSand agent. Zero keeps everything.
	OutputKeepBytes int
	// NewSessionState is the state of a new dev session. Zero means
	// RUNNING.
	NewSessionState sandrunv1.RequestState
	// PromoteAfterGets moves a QUEUED or STARTING dev session to RUNNING
	// after this many GetDevSession calls for it. Zero never promotes.
	PromoteAfterGets int
	// BeforeExec, when it is set, is called with a copy of each
	// ExecDevSession request before the fake guest starts the command.
	// Tests use it to change the guest between the commands of a sync, for
	// example to act as another client.
	BeforeExec func(*sandrunv1.ExecDevSessionRequest)
	// ReadStreamChunkLimit ends every ReadDevSessionFile stream with an OK
	// status and no end message after this many chunks, like the stream
	// limit of sandrun-api. Zero means no limit.
	ReadStreamChunkLimit int
	// WatchEventLimit ends every WatchQueue stream with an OK status after
	// this many events. Zero means no limit (the stream ends with the
	// request).
	WatchEventLimit int

	srv      *httptest.Server
	mu       sync.Mutex
	faults   map[string][]error
	calls    map[string]int
	lastCred atomic.Int32
	nextID   atomic.Int64

	orgs            []*sandrunv1.Organization
	members         map[string][]*sandrunv1.Member
	profiles        []*sandrunv1.RunnerProfile
	sessions        []*sandrunv1.DevSession
	sessionGets     map[string]int
	apiKeys         []*sandrunv1.ApiKey
	requests        []*sandrunv1.Request
	runners         []*sandrunv1.Runner
	devices         []*sandrunv1.DeviceCapacity
	links           map[string]*sandrunv1.OwnSandLink
	linkKeys        map[string]string
	integrationReqs []*sandrunv1.CreateAgentIntegrationRequest
	integration     []*sandrunv1.AgentIntegration
	queueByOrg      map[string]chan *sandrunv1.WatchQueueResponse
	unscopedQueue   chan *sandrunv1.WatchQueueResponse

	guest *guest
}

// New starts a fake server. Call Close when the test ends.
func New() *Server {
	s := &Server{
		AccessToken:   DefaultAccessToken,
		APIKey:        DefaultAPIKey,
		UserID:        DefaultUserID,
		faults:        map[string][]error{},
		calls:         map[string]int{},
		members:       map[string][]*sandrunv1.Member{},
		sessionGets:   map[string]int{},
		links:         map[string]*sandrunv1.OwnSandLink{},
		queueByOrg:    map[string]chan *sandrunv1.WatchQueueResponse{},
		unscopedQueue: make(chan *sandrunv1.WatchQueueResponse, queueBufferSize),
	}
	s.guest = newGuest(s)
	opts := connect.WithInterceptors(&interceptor{s: s})
	mux := http.NewServeMux()
	mux.Handle(sandrunv1connect.NewOrganizationServiceHandler(&orgService{s: s}, opts))
	mux.Handle(sandrunv1connect.NewMemberServiceHandler(&memberService{s: s}, opts))
	mux.Handle(sandrunv1connect.NewOwnSandLinkServiceHandler(&linkService{s: s}, opts))
	mux.Handle(sandrunv1connect.NewGitHubServiceHandler(&gitHubService{s: s}, opts))
	mux.Handle(sandrunv1connect.NewAgentIntegrationServiceHandler(&integrationService{s: s}, opts))
	mux.Handle(sandrunv1connect.NewRunnerProfileServiceHandler(&profileService{s: s}, opts))
	mux.Handle(sandrunv1connect.NewWorkServiceHandler(&workService{s: s}, opts))
	mux.Handle(sandrunv1connect.NewApiKeyServiceHandler(&apiKeyService{s: s}, opts))
	mux.Handle(sandrunv1connect.NewAuditServiceHandler(&sandrunv1connect.UnimplementedAuditServiceHandler{}, opts))
	mux.Handle(sandrunv1connect.NewDevSessionServiceHandler(&devSessionService{s: s}, opts))
	srv := httptest.NewUnstartedServer(mux)
	srv.Config.Protocols = new(http.Protocols)
	srv.Config.Protocols.SetHTTP1(true)
	srv.Config.Protocols.SetUnencryptedHTTP2(true)
	srv.Start()
	s.srv = srv
	s.URL = srv.URL
	return s
}

// Close stops the server and kills the running exec processes.
func (s *Server) Close() {
	s.guest.close()
	s.srv.CloseClientConnections()
	s.srv.Close()
}

// Fail makes the next calls of a procedure (for example
// "/delino.sandrun.v1.DevSessionService/GetDevSession") fail with errs, one
// error per call.
func (s *Server) Fail(procedure string, errs ...error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults[procedure] = append(s.faults[procedure], errs...)
}

// Calls returns how often a procedure was called.
func (s *Server) Calls(procedure string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[procedure]
}

// LastCredential returns the credential kind of the last request.
func (s *Server) LastCredential() CredentialKind { return CredentialKind(s.lastCred.Load()) }

// newID returns a new UUID v7.
func (s *Server) newID() string {
	id, err := uuidv7.NewString()
	if err != nil {
		panic(err)
	}
	return id
}

const issuedAPIKeyPrefix = "sandrun_secret-"

// apiKeyOrganization returns the scope of a valid personal API key. The
// configured fixture key has no organization scope.
func (s *Server) apiKeyOrganization(token string) (string, bool) {
	if token == s.APIKey && strings.HasPrefix(token, "sandrun_") {
		return "", true
	}
	id, ok := strings.CutPrefix(token, issuedAPIKeyPrefix)
	if !ok {
		return "", false
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, key := range s.apiKeys {
		if key.GetApiKeyId() != id || key.GetState() != sandrunv1.ApiKeyState_API_KEY_STATE_ACTIVE {
			continue
		}
		if expiresAt := key.GetExpiresAt(); expiresAt != nil && !time.Now().Before(expiresAt.AsTime()) {
			return "", false
		}
		return key.GetOrganizationId(), true
	}
	return "", false
}

// interceptor checks credentials and injects faults.
type interceptor struct{ s *Server }

func (i *interceptor) check(procedure string, h http.Header) (string, error) {
	s := i.s
	s.mu.Lock()
	s.calls[procedure]++
	var fault error
	if f := s.faults[procedure]; len(f) > 0 {
		fault, s.faults[procedure] = f[0], f[1:]
	}
	s.mu.Unlock()
	tok, ok := strings.CutPrefix(h.Get("Authorization"), "Bearer ")
	if !ok || tok == "" {
		s.lastCred.Store(int32(CredentialNone))
		return "", connect.NewError(connect.CodeUnauthenticated, errors.New("missing credential"))
	}
	if tok == s.AccessToken {
		s.lastCred.Store(int32(CredentialAccessToken))
		return "", fault
	}
	org, ok := s.apiKeyOrganization(tok)
	if !ok {
		s.lastCred.Store(int32(CredentialNone))
		return "", connect.NewError(connect.CodeUnauthenticated, errors.New("invalid credential"))
	}
	s.lastCred.Store(int32(CredentialAPIKey))
	if !strings.HasPrefix(procedure, "/"+sandrunv1connect.DevSessionServiceName+"/") {
		return "", connect.NewError(connect.CodePermissionDenied, errors.New("a personal API key is accepted only on DevSessionService"))
	}
	return org, fault
}

func checkAPIKeyOrganization(org string, msg any) error {
	if org == "" {
		return nil
	}
	req, ok := msg.(interface{ GetOrganizationId() string })
	if !ok || req.GetOrganizationId() != org {
		return notFound("organization")
	}
	return nil
}

func (i *interceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		org, err := i.check(req.Spec().Procedure, req.Header())
		if err != nil {
			return nil, err
		}
		if err := checkAPIKeyOrganization(org, req.Any()); err != nil {
			return nil, err
		}
		return next(ctx, req)
	}
}

func (i *interceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i *interceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		org, err := i.check(conn.Spec().Procedure, conn.RequestHeader())
		if err != nil {
			return err
		}
		if org != "" {
			conn = &apiKeyOrganizationConn{StreamingHandlerConn: conn, organization: org}
		}
		return next(ctx, conn)
	}
}

type apiKeyOrganizationConn struct {
	connect.StreamingHandlerConn
	organization string
}

func (c *apiKeyOrganizationConn) Receive(msg any) error {
	if err := c.StreamingHandlerConn.Receive(msg); err != nil {
		return err
	}
	return checkAPIKeyOrganization(c.organization, msg)
}

// page rejects negative sizes and returns one page of items with an opaque token.
// A size of zero defaults to 50. The maximum size is 200.
func page[T any](items []T, size int32, token string) ([]T, string, error) {
	switch {
	case size < 0:
		return nil, "", connect.NewError(connect.CodeInvalidArgument, errors.New("invalid page_size: must not be negative"))
	case size == 0:
		size = 50
	case size > 200:
		size = 200
	}
	start := 0
	if token != "" {
		n, err := strconv.Atoi(token)
		if err != nil || n < 0 || n > len(items) {
			return nil, "", connect.NewError(connect.CodeInvalidArgument, errors.New("invalid page token"))
		}
		start = n
	}
	end := min(start+int(size), len(items))
	next := ""
	if end < len(items) {
		next = strconv.Itoa(end)
	}
	return items[start:end], next, nil
}

func notFound(what string) error {
	return connect.NewError(connect.CodeNotFound, fmt.Errorf("%s not found", what))
}

// requireOrg returns the organization or not_found.
func (s *Server) requireOrg(id string) (*sandrunv1.Organization, error) {
	for _, o := range s.orgs {
		if o.GetOrganizationId() == id {
			return o, nil
		}
	}
	return nil, notFound("organization")
}
