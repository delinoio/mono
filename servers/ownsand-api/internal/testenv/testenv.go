// SPDX-License-Identifier: FSL-1.1-ALv2

// Package testenv runs the wired control plane in process for integration
// tests: a fresh Postgres database, an HTTP/2 test server and Connect
// clients that authenticate as users, API keys or devices.
package testenv

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/delinoio/mono/packages/ownsand-sdk-go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
	"github.com/delinoio/mono/servers/ownsand-api/internal/api"
	"github.com/delinoio/mono/servers/ownsand-api/internal/app"
	"github.com/delinoio/mono/servers/ownsand-api/internal/auth"
	"github.com/delinoio/mono/servers/ownsand-api/internal/db"
	"github.com/delinoio/mono/servers/ownsand-api/internal/db/dbtest"
	"github.com/delinoio/mono/servers/ownsand-api/internal/objectstore"
	"github.com/delinoio/mono/servers/ownsand-api/internal/ratelimit"
)

// userTokenPrefix marks a fake Logto access token: the rest is the user ID.
const userTokenPrefix = "test-user-token."

// fakeLogto accepts tokens of the form userTokenPrefix + sub.
type fakeLogto struct{}

func (fakeLogto) Validate(_ context.Context, raw string) (string, error) {
	if sub, ok := strings.CutPrefix(raw, userTokenPrefix); ok && sub != "" {
		return sub, nil
	}
	return "", auth.ErrInvalidToken
}

// Clock is a settable test clock.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

// Now returns the current test time.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock forward.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// SafeBuffer is a concurrency-safe log sink.
type SafeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *SafeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// String returns everything written so far.
func (b *SafeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Env is a running control plane.
type Env struct {
	T      testing.TB
	DB     *db.DB
	App    *app.App
	Server *httptest.Server
	Clock  *Clock
	Logs   *SafeBuffer
	Logger *slog.Logger
	// Objects is the in-memory object store of snapshot exports.
	Objects *objectstore.Memory
}

// Option changes the app options before New wires the app.
type Option func(*app.Options)

// New starts a control plane on a fresh database. It skips the test when
// no test database is configured.
func New(t testing.TB, opts ...Option) *Env {
	t.Helper()
	database := dbtest.New(t)
	clock := &Clock{now: time.Now().UTC().Truncate(time.Millisecond)}
	logs := &SafeBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	// Generous limits so tests are not rate-limited unless they test it.
	limits := api.Limits{
		PerDevice: ratelimit.New(1000, 1000, clock.Now),
		PerPrefix: ratelimit.New(1000, 1000, clock.Now),
		PerClient: ratelimit.New(1000, 1000, clock.Now),
	}
	timing := api.Timing{
		HeartbeatTimeout: 45 * time.Second,
		CommandWait:      5 * time.Second,
		EventPoll:        50 * time.Millisecond,
		OfflineSweep:     100 * time.Millisecond,
		CleanupPoll:      time.Hour,
		ImportWait:       10 * time.Second,
	}
	objects := objectstore.NewMemory(clock.Now)
	o := app.Options{
		DB:       database,
		Logger:   logger,
		Now:      clock.Now,
		Logto:    fakeLogto{},
		Limits:   &limits,
		Timing:   &timing,
		Objects:  objects,
		STUNURLs: []string{"stun:127.0.0.1:3478"},
		RelayTiming: &api.RelayTiming{Rotation: 3 * time.Second, Keepalive: 200 * time.Millisecond, Acceptance: time.Second,
			RequestDeadline: 2 * time.Second, GrantAck: time.Second, Signaling: 2 * time.Second},
	}
	for _, opt := range opts {
		opt(&o)
	}
	a, err := app.New(o)
	if err != nil {
		t.Fatalf("wire app: %v", err)
	}
	srv := httptest.NewUnstartedServer(a.Handler())
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return &Env{T: t, DB: database, App: a, Server: srv, Clock: clock, Logs: logs, Logger: logger, Objects: objects}
}

// UserToken returns a fake Logto access token for userID.
func UserToken(userID string) string { return userTokenPrefix + userID }

// bearer adds an Authorization header to every request.
type bearer struct {
	token string
	extra http.Header
}

func (b bearer) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if b.token != "" {
			req.Header().Set("Authorization", "Bearer "+b.token)
		}
		for k, v := range b.extra {
			req.Header()[k] = v
		}
		return next(ctx, req)
	}
}

func (b bearer) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		if b.token != "" {
			conn.RequestHeader().Set("Authorization", "Bearer "+b.token)
		}
		for k, v := range b.extra {
			conn.RequestHeader()[k] = v
		}
		return conn
	}
}

func (b bearer) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

// Clients holds a Connect client of every service, authenticated with one
// credential.
type Clients struct {
	Organization ownsandv1connect.OrganizationServiceClient
	Member       ownsandv1connect.MemberServiceClient
	APIKey       ownsandv1connect.ApiKeyServiceClient
	Device       ownsandv1connect.DeviceServiceClient
	Agent        ownsandv1connect.AgentServiceClient
	Sandbox      ownsandv1connect.SandboxServiceClient
	Snapshot     ownsandv1connect.SnapshotServiceClient
	DataPath     ownsandv1connect.DataPathServiceClient
	Relay        ownsandv1connect.RelayServiceClient
	RelayAgent   ownsandv1connect.RelayAgentServiceClient
}

// ClientsWith returns clients that send token as the bearer credential (no
// header when token is empty) and the extra headers.
func (e *Env) ClientsWith(token string, extra http.Header) *Clients {
	httpClient := e.Server.Client()
	opts := []connect.ClientOption{connect.WithInterceptors(bearer{token: token, extra: extra})}
	url := e.Server.URL
	return &Clients{
		Organization: ownsandv1connect.NewOrganizationServiceClient(httpClient, url, opts...),
		Member:       ownsandv1connect.NewMemberServiceClient(httpClient, url, opts...),
		APIKey:       ownsandv1connect.NewApiKeyServiceClient(httpClient, url, opts...),
		Device:       ownsandv1connect.NewDeviceServiceClient(httpClient, url, opts...),
		Agent:        ownsandv1connect.NewAgentServiceClient(httpClient, url, opts...),
		Sandbox:      ownsandv1connect.NewSandboxServiceClient(httpClient, url, opts...),
		Snapshot:     ownsandv1connect.NewSnapshotServiceClient(httpClient, url, opts...),
		DataPath:     ownsandv1connect.NewDataPathServiceClient(httpClient, url, opts...),
		Relay:        ownsandv1connect.NewRelayServiceClient(httpClient, url, opts...),
		RelayAgent:   ownsandv1connect.NewRelayAgentServiceClient(httpClient, url, opts...),
	}
}

// Clients returns clients with token as the bearer credential.
func (e *Env) Clients(token string) *Clients { return e.ClientsWith(token, nil) }

// User returns clients for a Logto user.
func (e *Env) User(userID string) *Clients { return e.Clients(UserToken(userID)) }

// Org is an organization with an owner, an admin, a member and an API key.
type Org struct {
	ID          string
	Owner       string
	Admin       string
	Member      string
	APIKey      string
	APIKeyID    string
	OwnerClient *Clients
}

// NewOrg creates an organization owned by owner, invites an admin and a
// member and creates an API key.
func (e *Env) NewOrg(owner string) *Org {
	e.T.Helper()
	ctx := context.Background()
	oc := e.User(owner)
	created, err := oc.Organization.CreateOrganization(ctx, connect.NewRequest(&ownsandv1.CreateOrganizationRequest{Name: "org of " + owner}))
	if err != nil {
		e.T.Fatalf("create organization: %v", err)
	}
	org := &Org{ID: created.Msg.GetOrganization().GetOrganizationId(), Owner: owner, Admin: owner + "-admin", Member: owner + "-member", OwnerClient: oc}
	e.Join(org.ID, oc, org.Admin, ownsandv1.Role_ROLE_ADMIN)
	e.Join(org.ID, oc, org.Member, ownsandv1.Role_ROLE_MEMBER)
	key, err := oc.APIKey.CreateApiKey(ctx, connect.NewRequest(&ownsandv1.CreateApiKeyRequest{OrganizationId: org.ID, Name: "ci"}))
	if err != nil {
		e.T.Fatalf("create api key: %v", err)
	}
	org.APIKey = key.Msg.GetApiKeySecret()
	org.APIKeyID = key.Msg.GetApiKey().GetApiKeyId()
	return org
}

// Join invites userID with role through inviter and accepts the invitation.
func (e *Env) Join(orgID string, inviter *Clients, userID string, role ownsandv1.Role) {
	e.T.Helper()
	ctx := context.Background()
	inv, err := inviter.Member.CreateInvitation(ctx, connect.NewRequest(&ownsandv1.CreateInvitationRequest{OrganizationId: orgID, Role: role}))
	if err != nil {
		e.T.Fatalf("create invitation: %v", err)
	}
	if _, err := e.User(userID).Member.AcceptInvitation(ctx, connect.NewRequest(&ownsandv1.AcceptInvitationRequest{InvitationSecret: inv.Msg.GetInvitationSecret()})); err != nil {
		e.T.Fatalf("accept invitation: %v", err)
	}
}

// Device is an enrolled test device with its key.
type Device struct {
	ID         string
	OrgID      string
	PrivateKey ed25519.PrivateKey
	Epoch      string
}

// EnrollDevice creates an enrollment token through admin and enrolls a new
// device.
func (e *Env) EnrollDevice(orgID string, admin *Clients, hostOS ownsandv1.HostOs, arch ownsandv1.Arch) *Device {
	e.T.Helper()
	ctx := context.Background()
	tok, err := admin.Device.CreateEnrollmentToken(ctx, connect.NewRequest(&ownsandv1.CreateEnrollmentTokenRequest{OrganizationId: orgID, Ttl: durationpb.New(time.Hour)}))
	if err != nil {
		e.T.Fatalf("create enrollment token: %v", err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		e.T.Fatal(err)
	}
	resp, err := e.Clients("").Agent.Enroll(ctx, connect.NewRequest(&ownsandv1.EnrollRequest{
		EnrollmentToken: tok.Msg.GetEnrollmentToken(), PublicKey: pub, Name: "host", HostOs: hostOS, Arch: arch, AgentVersion: "1.0.0",
	}))
	if err != nil {
		e.T.Fatalf("enroll: %v", err)
	}
	epoch, _ := uuidv7.NewString()
	return &Device{ID: resp.Msg.GetDeviceId(), OrgID: orgID, PrivateKey: priv, Epoch: epoch}
}

// Sign returns the device signature of purpose followed by nonce.
func (d *Device) Sign(purpose string, nonce []byte) []byte {
	return ed25519.Sign(d.PrivateKey, append([]byte(purpose), nonce...))
}

// Authenticate runs CreateChallenge and Authenticate for the device.
func (e *Env) Authenticate(d *Device) (*ownsandv1.AuthenticateResponse, error) {
	ctx := context.Background()
	c := e.Clients("")
	ch, err := c.Agent.CreateChallenge(ctx, connect.NewRequest(&ownsandv1.CreateChallengeRequest{DeviceId: d.ID}))
	if err != nil {
		return nil, err
	}
	nonce := ch.Msg.GetNonce()
	resp, err := c.Agent.Authenticate(ctx, connect.NewRequest(&ownsandv1.AuthenticateRequest{
		DeviceId: d.ID, Nonce: nonce, Signature: d.Sign(auth.PurposeAuthenticate, nonce), StateEpoch: d.Epoch,
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

// SessionCredential authenticates the device and returns its `osd_`
// credential.
func (e *Env) SessionCredential(d *Device) string {
	e.T.Helper()
	resp, err := e.Authenticate(d)
	if err != nil {
		e.T.Fatalf("authenticate device: %v", err)
	}
	if resp.GetSession() == nil {
		e.T.Fatalf("authenticate device: got a removal notice")
	}
	return resp.GetSession().GetDeviceSessionCredential()
}

// Code returns the Connect code of err, or 0 for nil.
func Code(err error) connect.Code {
	if err == nil {
		return 0
	}
	var ce *connect.Error
	if errors.As(err, &ce) {
		return ce.Code()
	}
	return connect.CodeUnknown
}

// Discard is a logger that drops everything.
func Discard() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }
