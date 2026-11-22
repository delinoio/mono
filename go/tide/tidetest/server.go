// SPDX-License-Identifier: Apache-2.0

// Package tidetest is an in-memory fake of tide-api for tests of programs
// that use the tide Go SDK. It serves every public service of
// delino.tide.v1 over an httptest server; services and RPCs that tests do
// not need return unimplemented.
//
// The fake guest of a dev session is the local machine: an exec runs the
// command as a local process, and file operations read and write local
// paths. A dev session profile therefore uses a local absolute directory
// as its workspace path, and sync and download tests run the SDK's guest
// commands with the local sh and tar. It is a test fake, not a sandbox:
// never point it at directories that a test does not own.
package tidetest

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

	"connectrpc.com/connect"

	"github.com/delinoio/mono/go/uuidv7"
	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/protos/delino/tide/v1/tidev1connect"
)

// Default credentials of a new Server.
const (
	DefaultAccessToken = "test-access-token"
	DefaultAPIKey      = "tide_test-personal-api-key"
	DefaultUserID      = "user-1"
)

// CredentialKind is the kind of the credential of a request.
type CredentialKind int

const (
	CredentialNone CredentialKind = iota
	CredentialAccessToken
	CredentialAPIKey
)

// Server is a fake tide-api. Configure the exported fields before the
// first request; they are not synchronized.
type Server struct {
	// URL is the origin of the server, for example http://127.0.0.1:1234.
	URL string
	// AccessToken is the accepted Logto access token, APIKey the accepted
	// personal API key. An API key is accepted only on DevSessionService.
	AccessToken string
	APIKey      string
	// UserID is the user of both credentials.
	UserID string
	// ExecStreamEventLimit ends every exec and attach stream with an OK
	// status after this many events, like the 14-minute limit of tide-api.
	// Zero means no limit.
	ExecStreamEventLimit int
	// OutputKeepBytes keeps only the last bytes of each exec output stream,
	// like the 8 MiB of the OwnSand agent. Zero keeps everything.
	OutputKeepBytes int
	// NewSessionState is the state of a new dev session. Zero means
	// RUNNING.
	NewSessionState tidev1.RequestState
	// PromoteAfterGets moves a QUEUED or STARTING dev session to RUNNING
	// after this many GetDevSession calls for it. Zero never promotes.
	PromoteAfterGets int
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

	orgs            []*tidev1.Organization
	members         map[string][]*tidev1.Member
	profiles        []*tidev1.RunnerProfile
	sessions        []*tidev1.DevSession
	sessionGets     map[string]int
	apiKeys         []*tidev1.ApiKey
	requests        []*tidev1.Request
	runners         []*tidev1.Runner
	devices         []*tidev1.DeviceCapacity
	links           map[string]*tidev1.OwnSandLink
	linkKeys        map[string]string
	integrationReqs []*tidev1.CreateAgentIntegrationRequest
	integration     []*tidev1.AgentIntegration
	queue           chan *tidev1.WatchQueueResponse

	guest *guest
}

// New starts a fake server. Call Close when the test ends.
func New() *Server {
	s := &Server{
		AccessToken: DefaultAccessToken,
		APIKey:      DefaultAPIKey,
		UserID:      DefaultUserID,
		faults:      map[string][]error{},
		calls:       map[string]int{},
		members:     map[string][]*tidev1.Member{},
		sessionGets: map[string]int{},
		links:       map[string]*tidev1.OwnSandLink{},
		queue:       make(chan *tidev1.WatchQueueResponse, 64),
	}
	s.guest = newGuest(s)
	opts := connect.WithInterceptors(&interceptor{s: s})
	mux := http.NewServeMux()
	mux.Handle(tidev1connect.NewOrganizationServiceHandler(&orgService{s: s}, opts))
	mux.Handle(tidev1connect.NewMemberServiceHandler(&memberService{s: s}, opts))
	mux.Handle(tidev1connect.NewOwnSandLinkServiceHandler(&linkService{s: s}, opts))
	mux.Handle(tidev1connect.NewGitHubServiceHandler(&gitHubService{s: s}, opts))
	mux.Handle(tidev1connect.NewAgentIntegrationServiceHandler(&integrationService{s: s}, opts))
	mux.Handle(tidev1connect.NewRunnerProfileServiceHandler(&profileService{s: s}, opts))
	mux.Handle(tidev1connect.NewWorkServiceHandler(&workService{s: s}, opts))
	mux.Handle(tidev1connect.NewApiKeyServiceHandler(&apiKeyService{s: s}, opts))
	mux.Handle(tidev1connect.NewAuditServiceHandler(&tidev1connect.UnimplementedAuditServiceHandler{}, opts))
	mux.Handle(tidev1connect.NewDevSessionServiceHandler(&devSessionService{s: s}, opts))
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
// "/delino.tide.v1.DevSessionService/GetDevSession") fail with errs, one
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

// interceptor checks credentials and injects faults.
type interceptor struct{ s *Server }

func (i *interceptor) check(procedure string, h http.Header) error {
	s := i.s
	s.mu.Lock()
	s.calls[procedure]++
	var fault error
	if f := s.faults[procedure]; len(f) > 0 {
		fault, s.faults[procedure] = f[0], f[1:]
	}
	s.mu.Unlock()
	tok, ok := strings.CutPrefix(h.Get("Authorization"), "Bearer ")
	switch {
	case !ok || tok == "":
		s.lastCred.Store(int32(CredentialNone))
		return connect.NewError(connect.CodeUnauthenticated, errors.New("missing credential"))
	case tok == s.AccessToken:
		s.lastCred.Store(int32(CredentialAccessToken))
	case tok == s.APIKey && strings.HasPrefix(tok, "tide_"):
		s.lastCred.Store(int32(CredentialAPIKey))
		if !strings.HasPrefix(procedure, "/"+tidev1connect.DevSessionServiceName+"/") {
			return connect.NewError(connect.CodePermissionDenied, errors.New("a personal API key is accepted only on DevSessionService"))
		}
	default:
		s.lastCred.Store(int32(CredentialNone))
		return connect.NewError(connect.CodeUnauthenticated, errors.New("invalid credential"))
	}
	return fault
}

func (i *interceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if err := i.check(req.Spec().Procedure, req.Header()); err != nil {
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
		if err := i.check(conn.Spec().Procedure, conn.RequestHeader()); err != nil {
			return err
		}
		return next(ctx, conn)
	}
}

// page returns one page of items with the protos contract rules: page
// size 50 by default and at most 200, and an opaque token.
func page[T any](items []T, size int32, token string) ([]T, string, error) {
	if size <= 0 {
		size = 50
	}
	if size > 200 {
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
func (s *Server) requireOrg(id string) (*tidev1.Organization, error) {
	for _, o := range s.orgs {
		if o.GetOrganizationId() == id {
			return o, nil
		}
	}
	return nil, notFound("organization")
}
