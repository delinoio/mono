// SPDX-License-Identifier: FSL-1.1-ALv2

// Package app wires the control plane: the Connect handlers, the
// interceptors and the background workers. cmd/ownsand-api and the
// in-process integration tests both build it.
package app

import (
	"context"
	"crypto/rand"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
	"github.com/delinoio/mono/servers/ownsand-api/internal/api"
	"github.com/delinoio/mono/servers/ownsand-api/internal/auth"
	"github.com/delinoio/mono/servers/ownsand-api/internal/db"
	"github.com/delinoio/mono/servers/ownsand-api/internal/janitor"
	"github.com/delinoio/mono/servers/ownsand-api/internal/server"
)

// Options configures the control plane.
type Options struct {
	DB     *db.DB
	Logger *slog.Logger
	Now    func() time.Time

	Logto api.TokenValidator

	// NonceKey and NonceKeyPrevious authenticate device nonces. A nil
	// NonceKey selects a random key for this process.
	NonceKey         []byte
	NonceKeyPrevious []byte

	CORSOrigins             []string
	TrustProxyClientAddress bool

	DisplayNames api.DisplayNameSource
	// Limits overrides the default rate limits (tests).
	Limits *api.Limits
	// JanitorInterval is the retention job interval; zero selects the
	// default.
	JanitorInterval time.Duration
}

// App is a wired control plane.
type App struct {
	opts    Options
	Service *api.Service
	handler http.Handler
}

// New wires the control plane.
func New(opts Options) (*App, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.JanitorInterval == 0 {
		opts.JanitorInterval = janitor.DefaultInterval
	}
	key := opts.NonceKey
	if key == nil {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		opts.Logger.Warn("nonce_key_ephemeral", slog.String("detail", "OWNSAND_NONCE_KEY is unset; nonces are valid only in this process"))
	}
	limits := api.DefaultLimits(opts.Now)
	if opts.Limits != nil {
		limits = *opts.Limits
	}
	svc := &api.Service{
		DB:           opts.DB,
		Logger:       opts.Logger,
		Now:          opts.Now,
		Nonces:       auth.NewNonceIssuer(key, opts.NonceKeyPrevious, opts.Now),
		Limits:       limits,
		DisplayNames: opts.DisplayNames,
	}
	authn := &api.Authenticator{Service: svc, Logto: opts.Logto}
	handlerOpts := []connect.HandlerOption{connect.WithInterceptors(svc.Interceptors(authn, opts.TrustProxyClientAddress)...)}

	var mounts []server.Mount
	add := func(path string, h http.Handler) { mounts = append(mounts, server.Mount{Path: path, Handler: h}) }
	add(ownsandv1connect.NewOrganizationServiceHandler(api.OrganizationHandler{Service: svc}, handlerOpts...))
	add(ownsandv1connect.NewMemberServiceHandler(api.MemberHandler{Service: svc}, handlerOpts...))
	add(ownsandv1connect.NewApiKeyServiceHandler(api.APIKeyHandler{Service: svc}, handlerOpts...))
	add(ownsandv1connect.NewDeviceServiceHandler(api.DeviceHandler{Service: svc}, handlerOpts...))
	add(ownsandv1connect.NewAgentServiceHandler(api.AgentHandler{Service: svc}, handlerOpts...))

	return &App{
		opts:    opts,
		Service: svc,
		handler: server.NewHandler(server.Options{Logger: opts.Logger, CORSOrigins: opts.CORSOrigins, Mounts: mounts}),
	}, nil
}

// Handler returns the root HTTP handler.
func (a *App) Handler() http.Handler { return a.handler }

// Run runs the background workers until ctx ends and waits for them.
func (a *App) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		janitor.Run(ctx, a.opts.DB, a.opts.Logger, a.opts.Now, a.opts.JanitorInterval)
	}()
	wg.Wait()
}
