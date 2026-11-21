// SPDX-License-Identifier: FSL-1.1-ALv2

// Package testenv runs accounts-api in process for integration tests: a
// fresh Postgres database (skipped without ACCOUNTS_TEST_DATABASE_URL), the
// production handler from internal/app, a test Logto issuer, the fake Logto
// directory, the email fake and a controllable clock.
package testenv

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/protos/delino/accounts/v1/accountsv1connect"
	"github.com/delinoio/mono/servers/accounts-api/internal/app"
	"github.com/delinoio/mono/servers/accounts-api/internal/auth"
	"github.com/delinoio/mono/servers/accounts-api/internal/auth/authtest"
	"github.com/delinoio/mono/servers/accounts-api/internal/db"
	"github.com/delinoio/mono/servers/accounts-api/internal/db/dbtest"
	"github.com/delinoio/mono/servers/accounts-api/internal/directory"
	"github.com/delinoio/mono/servers/accounts-api/internal/email"
)

// WebOrigin is the web app origin of invitation links in tests.
const WebOrigin = "https://accounts.example.test"

// Clock is a settable clock. Each read advances it by one millisecond, so
// consecutive events, such as two joins, have distinct times as they would on
// a real clock.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

// Now returns the current test time and advances the clock by a millisecond.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now
	c.now = c.now.Add(time.Millisecond)
	return now
}

// Advance moves the clock forward.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// SafeBuffer is a goroutine-safe log buffer.
type SafeBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *SafeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *SafeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Env is a running accounts-api.
type Env struct {
	t         testing.TB
	DB        *db.DB
	URL       string
	Issuer    *authtest.Issuer
	Directory *directory.Fake
	Email     *email.Fake
	Clock     *Clock
	Logs      *SafeBuffer
}

// New starts accounts-api on a fresh database.
func New(t testing.TB) *Env {
	t.Helper()
	database := dbtest.New(t)
	logs := &SafeBuffer{}
	logger := slog.New(slog.NewJSONHandler(io.MultiWriter(logs), &slog.HandlerOptions{Level: slog.LevelDebug}))
	env := &Env{
		t:         t,
		DB:        database,
		Issuer:    authtest.NewIssuer(t),
		Directory: directory.NewFake(),
		Email:     email.NewFake(),
		Clock:     &Clock{now: time.Now().UTC().Truncate(time.Microsecond)},
		Logs:      logs,
	}
	handler := app.Handler(app.Options{
		DB: database,
		Validator: auth.NewLogtoValidator(auth.LogtoConfig{
			Issuer: env.Issuer.URL, Audience: authtest.Audience, Logger: logger,
		}),
		Directory:   env.Directory,
		Email:       env.Email,
		Logger:      logger,
		Now:         env.Clock.Now,
		Audience:    authtest.Audience,
		CORSOrigins: []string{WebOrigin},
		WebOrigin:   WebOrigin,
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	env.URL = srv.URL
	return env
}

// bearer adds an Authorization header to every request.
type bearer string

func (b bearer) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if b != "" {
			req.Header().Set("Authorization", "Bearer "+string(b))
		}
		return next(ctx, req)
	}
}

func (b bearer) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (b bearer) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

// Client returns a BillingAccountService client that sends token; an empty
// token sends no Authorization header.
func (e *Env) Client(token string) accountsv1connect.BillingAccountServiceClient {
	return accountsv1connect.NewBillingAccountServiceClient(http.DefaultClient, e.URL,
		connect.WithInterceptors(bearer(token)))
}

// User registers a Logto user with a primary email and a display name and
// returns a client with a valid user access token.
func (e *Env) User(userID, primaryEmail, name string) accountsv1connect.BillingAccountServiceClient {
	e.t.Helper()
	e.Directory.Set(userID, directory.User{PrimaryEmail: primaryEmail, Name: name})
	return e.Client(e.Issuer.UserToken(e.t, userID))
}

// InvitationToken returns the token of the invitation email sent to addr,
// read from the link in the latest such email.
func (e *Env) InvitationToken(addr string) string {
	e.t.Helper()
	msgs := e.Email.Messages()
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].To != addr {
			continue
		}
		_, token, ok := strings.Cut(msgs[i].Text, "#token=")
		if !ok {
			break
		}
		token, _, _ = strings.Cut(token, "\n")
		return token
	}
	e.t.Fatalf("no invitation email to %s", addr)
	return ""
}
