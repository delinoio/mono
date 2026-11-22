// SPDX-License-Identifier: FSL-1.1-ALv2

// Package testenv runs the wired control plane in process for integration
// tests: a fresh Postgres database (TIDE_TEST_DATABASE_URL, see dbtest), an
// HTTP/2 test server, a settable clock, a fake Logto validator, a random
// secrets keyring and Connect clients that authenticate as users or with
// personal API keys. Later layers add their services with WithExtension and
// their fakes (OwnSand, GitHub, vendors) next to this package.
package testenv

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/protos/delino/tide/v1/tidev1connect"
	"github.com/delinoio/mono/servers/tide-api/internal/api"
	"github.com/delinoio/mono/servers/tide-api/internal/app"
	"github.com/delinoio/mono/servers/tide-api/internal/auth"
	"github.com/delinoio/mono/servers/tide-api/internal/config"
	"github.com/delinoio/mono/servers/tide-api/internal/db"
	"github.com/delinoio/mono/servers/tide-api/internal/db/dbtest"
	"github.com/delinoio/mono/servers/tide-api/internal/layers"
	"github.com/delinoio/mono/servers/tide-api/internal/ownsandclient/ownsandfake"
	"github.com/delinoio/mono/servers/tide-api/internal/ratelimit"
	"github.com/delinoio/mono/servers/tide-api/internal/secrets"
	"github.com/delinoio/mono/servers/tide-api/internal/work"
)

// UserTokenPrefix marks a fake Logto access token: the rest is the user ID.
const UserTokenPrefix = "test-user-token."

// fakeLogto accepts tokens of the form UserTokenPrefix + sub.
type fakeLogto struct{}

func (fakeLogto) Validate(_ context.Context, raw string) (string, error) {
	if sub, ok := strings.CutPrefix(raw, UserTokenPrefix); ok && sub != "" {
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
	// SecretKey is the key of the test keyring.
	SecretKey secrets.Key
}

// Option changes the app options before New wires the app.
type Option func(*app.Options)

// WithExtension adds a later layer (or a test fake of one).
func WithExtension(ext app.Extension) Option {
	return func(o *app.Options) { o.Extensions = append(o.Extensions, ext) }
}

// WithLayers wires every layer of tide-api with the fake OwnSand API and
// fills out with the shared parts. The engine uses short intervals.
func WithLayers(fake *ownsandfake.Fake, out *layers.Layers) Option {
	return WithLayerOptions(layers.Options{OwnSand: fake}, out)
}

// FastWorkTiming returns engine intervals for tests.
func FastWorkTiming() work.Timing {
	return work.Timing{PassInterval: 200 * time.Millisecond, ReconcileInterval: 100 * time.Millisecond, SandboxPoll: 50 * time.Millisecond,
		StartTimeout: time.Minute, GaugeInterval: 100 * time.Millisecond, CreateTimeout: time.Second}
}

// WithLayerOptions is WithLayers with explicit layer options; empty fields
// get test defaults.
func WithLayerOptions(opts layers.Options, out *layers.Layers) Option {
	if opts.PublicOrigin == "" {
		opts.PublicOrigin = "https://tide.test"
	}
	if opts.WorkTiming == nil {
		t := FastWorkTiming()
		opts.WorkTiming = &t
	}
	return func(o *app.Options) {
		o.Extensions = append(o.Extensions, layers.Extensions(opts, out)...)
	}
}

// WithLimits replaces the generous test rate limits.
func WithLimits(l api.Limits) Option {
	return func(o *app.Options) { o.Limits = &l }
}

// GenerousLimits returns limits that tests do not hit unless they test them.
func GenerousLimits(now func() time.Time) api.Limits {
	return api.Limits{
		PerPrefix: ratelimit.New(1000, 1000, now),
		PerUser:   ratelimit.New(1000, 1000, now),
		PerClient: ratelimit.New(1000, 1000, now),
	}
}

// New starts a control plane on a fresh database. It skips the test when no
// test database is configured.
func New(t testing.TB, opts ...Option) *Env {
	t.Helper()
	database := dbtest.New(t)
	clock := &Clock{now: time.Now().UTC().Truncate(time.Millisecond)}
	logs := &SafeBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	key, err := secrets.GenerateKey("test")
	if err != nil {
		t.Fatal(err)
	}
	keyring, err := secrets.NewKeyring(key)
	if err != nil {
		t.Fatal(err)
	}
	limits := GenerousLimits(clock.Now)
	o := app.Options{
		DB:      database,
		Logger:  logger,
		Now:     clock.Now,
		Logto:   fakeLogto{},
		Secrets: keyring,
		Limits:  &limits,
		// Tests connect to the server directly, so the TCP peer is the client.
		ClientAddressMode: config.ClientAddressDirect,
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
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		srv.Close()
	})
	return &Env{T: t, DB: database, App: a, Server: srv, Clock: clock, Logs: logs, SecretKey: key}
}

// Clients are the Connect clients of one credential.
type Clients struct {
	Organization tidev1connect.OrganizationServiceClient
	Member       tidev1connect.MemberServiceClient
	APIKey       tidev1connect.ApiKeyServiceClient
	Audit        tidev1connect.AuditServiceClient
	DevSession   tidev1connect.DevSessionServiceClient
	OwnSandLink  tidev1connect.OwnSandLinkServiceClient
	Integration  tidev1connect.AgentIntegrationServiceClient
	Profile      tidev1connect.RunnerProfileServiceClient
	Work         tidev1connect.WorkServiceClient
	GitHub       tidev1connect.GitHubServiceClient
	// HTTP sends plain requests with the same credential.
	HTTP *http.Client
}

type bearerTransport struct {
	token string
	next  http.RoundTripper
}

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if b.token != "" {
		r.Header.Set("Authorization", "Bearer "+b.token)
	}
	return b.next.RoundTrip(r)
}

// Bearer returns clients that send token as the bearer credential (empty:
// no credential).
func (e *Env) Bearer(token string) *Clients {
	hc := &http.Client{Transport: bearerTransport{token: token, next: e.Server.Client().Transport}}
	url := e.Server.URL
	return &Clients{
		Organization: tidev1connect.NewOrganizationServiceClient(hc, url),
		Member:       tidev1connect.NewMemberServiceClient(hc, url),
		APIKey:       tidev1connect.NewApiKeyServiceClient(hc, url),
		Audit:        tidev1connect.NewAuditServiceClient(hc, url),
		DevSession:   tidev1connect.NewDevSessionServiceClient(hc, url),
		OwnSandLink:  tidev1connect.NewOwnSandLinkServiceClient(hc, url),
		Integration:  tidev1connect.NewAgentIntegrationServiceClient(hc, url),
		Profile:      tidev1connect.NewRunnerProfileServiceClient(hc, url),
		Work:         tidev1connect.NewWorkServiceClient(hc, url),
		GitHub:       tidev1connect.NewGitHubServiceClient(hc, url),
		HTTP:         hc,
	}
}

// User returns clients that authenticate as the Logto user sub.
func (e *Env) User(sub string) *Clients { return e.Bearer(UserTokenPrefix + sub) }

// Org is an organization with one owner, one admin and one member.
type Org struct {
	ID                   string
	Owner, Admin, Member string
}

// NewOrg creates an organization owned by owner and adds the users
// owner+"-admin" and owner+"-member" through invitations.
func (e *Env) NewOrg(owner string) Org {
	e.T.Helper()
	ctx := context.Background()
	resp, err := e.User(owner).Organization.CreateOrganization(ctx, connect.NewRequest(&tidev1.CreateOrganizationRequest{Name: owner + " org"}))
	if err != nil {
		e.T.Fatalf("create organization: %v", err)
	}
	org := Org{ID: resp.Msg.GetOrganization().GetOrganizationId(), Owner: owner, Admin: owner + "-admin", Member: owner + "-member"}
	e.Join(org.ID, owner, org.Admin, tidev1.Role_ROLE_ADMIN)
	e.Join(org.ID, owner, org.Member, tidev1.Role_ROLE_MEMBER)
	return org
}

// Join makes user a member with role through an invitation of inviter.
func (e *Env) Join(orgID, inviter, user string, role tidev1.Role) {
	e.T.Helper()
	ctx := context.Background()
	inv, err := e.User(inviter).Member.CreateInvitation(ctx, connect.NewRequest(&tidev1.CreateInvitationRequest{OrganizationId: orgID, Role: role}))
	if err != nil {
		e.T.Fatalf("create invitation: %v", err)
	}
	if _, err := e.User(user).Member.AcceptInvitation(ctx, connect.NewRequest(&tidev1.AcceptInvitationRequest{InvitationSecret: inv.Msg.GetInvitationSecret()})); err != nil {
		e.T.Fatalf("accept invitation: %v", err)
	}
}

// NewAPIKey creates a personal API key of user and returns its ID and secret.
func (e *Env) NewAPIKey(orgID, user string) (string, string) {
	e.T.Helper()
	resp, err := e.User(user).APIKey.CreateApiKey(context.Background(), connect.NewRequest(&tidev1.CreateApiKeyRequest{OrganizationId: orgID, Name: "agent"}))
	if err != nil {
		e.T.Fatalf("create api key: %v", err)
	}
	return resp.Msg.GetApiKey().GetApiKeyId(), resp.Msg.GetSecret()
}

// Code returns the Connect code of err, or 0 for nil.
func Code(err error) connect.Code {
	if err == nil {
		return 0
	}
	return connect.CodeOf(err)
}
