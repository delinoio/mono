// SPDX-License-Identifier: FSL-1.1-ALv2

// Package app wires the accounts-api HTTP handler: the Connect services with
// their interceptors, the Paddle webhook, CORS and the operational
// endpoints. cmd/accounts-api
// and the integration tests both build it, so the tests exercise the
// production wiring.
package app

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/delinoio/mono/servers/accounts-api/internal/api"
	"github.com/delinoio/mono/servers/accounts-api/internal/auth"
	"github.com/delinoio/mono/servers/accounts-api/internal/billing"
	"github.com/delinoio/mono/servers/accounts-api/internal/db"
	"github.com/delinoio/mono/servers/accounts-api/internal/directory"
	"github.com/delinoio/mono/servers/accounts-api/internal/email"
	"github.com/delinoio/mono/servers/accounts-api/internal/server"
)

// Options are the dependencies of the handler.
type Options struct {
	DB *db.DB
	// Billing is the credit logic; it also serves the Paddle webhook.
	Billing   *billing.Service
	Validator auth.Validator
	Directory directory.Directory
	Email     email.Sender
	Logger    *slog.Logger
	// Now is the business clock; nil selects time.Now.
	Now func() time.Time
	// Audience is the API resource indicator, for the authentication logs.
	Audience    string
	CORSOrigins []string
	WebOrigin   string
}

// Handler returns the root HTTP handler. Interceptor order: the RPC log
// record wraps everything, so rejected calls are logged too; authentication
// runs next and the caller observer reports the authenticated caller to the
// log record.
func Handler(opts Options) http.Handler {
	svc := api.New(api.Deps{
		DB:        opts.DB,
		Billing:   opts.Billing,
		Directory: opts.Directory,
		Email:     opts.Email,
		Logger:    opts.Logger,
		Now:       opts.Now,
		WebOrigin: opts.WebOrigin,
	})
	mounts := svc.Mounts(
		api.NewLogInterceptor(opts.Logger),
		auth.NewInterceptor(opts.Validator, auth.DefaultPolicy, opts.Audience, opts.Logger),
		api.CallerObserver{},
	)
	// The Paddle webhook is the only plain HTTP business endpoint. Its
	// Paddle signature is its only credential, so it is outside the Connect
	// interceptors.
	mounts = append(mounts, server.Mount{Path: billing.WebhookPath, Handler: opts.Billing.WebhookHandler()})
	return server.NewHandler(opts.Logger,
		server.WithCORSOrigins(opts.CORSOrigins),
		server.WithMounts(mounts...),
	)
}
