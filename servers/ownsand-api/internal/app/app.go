// SPDX-License-Identifier: FSL-1.1-ALv2

// Package app assembles the control plane: the authenticator, the Connect
// handlers of every service and the background components.
package app

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
	"github.com/delinoio/mono/servers/ownsand-api/internal/accounts"
	"github.com/delinoio/mono/servers/ownsand-api/internal/api"
	"github.com/delinoio/mono/servers/ownsand-api/internal/auth"
	"github.com/delinoio/mono/servers/ownsand-api/internal/config"
	"github.com/delinoio/mono/servers/ownsand-api/internal/jobs"
	"github.com/delinoio/mono/servers/ownsand-api/internal/lease"
	"github.com/delinoio/mono/servers/ownsand-api/internal/ratelimit"
	"github.com/delinoio/mono/servers/ownsand-api/internal/rpc"
	"github.com/delinoio/mono/servers/ownsand-api/internal/server"
	"github.com/delinoio/mono/servers/ownsand-api/internal/store"
)

// Credential failure limit per display prefix: 10 failures, then one per 6
// seconds.
const (
	credentialFailureRate  = 1.0 / 6
	credentialFailureBurst = 10
)

// App is the assembled control plane.
type App struct {
	Config  config.Config
	Store   *store.Store
	Logger  *slog.Logger
	Authn   *auth.Authenticator
	API     *api.Server
	Handler http.Handler
	Lease   *lease.Manager
}

// Options replace dependencies in tests.
type Options struct {
	Tokens       auth.TokenValidator
	DisplayNames accounts.DisplayNameSource
}

// New assembles the app.
func New(cfg config.Config, st *store.Store, logger *slog.Logger, opts Options) *App {
	tokens := opts.Tokens
	if tokens == nil && cfg.LogtoEndpoint != "" {
		tokens = auth.NewJWTValidator(cfg.LogtoIssuer(), cfg.LogtoJWKSURI(), cfg.APIResource, &http.Client{Timeout: 10 * time.Second}, logger)
	}
	if tokens == nil {
		logger.Warn("logto_validation_disabled", slog.String("reason", config.EnvLogtoEndpoint+" is not set; every Logto token is rejected"))
	}
	names := opts.DisplayNames
	if names == nil {
		names = accounts.Unavailable{}
	}
	a := &App{Config: cfg, Store: st, Logger: logger}
	a.Authn = &auth.Authenticator{Store: st, Tokens: tokens, Failures: ratelimit.New(credentialFailureRate, credentialFailureBurst, nil)}
	a.API = &api.Server{Store: st, Logger: logger, DisplayNames: names}
	interceptor := &rpc.Interceptor{Authn: a.Authn, Logger: logger}
	opt := connect.WithInterceptors(interceptor)
	routes := []server.Route{}
	add := func(path string, h http.Handler) { routes = append(routes, server.Route{Path: path, Handler: h}) }
	add(ownsandv1connect.NewOrganizationServiceHandler(api.OrganizationService{Server: a.API}, opt))
	add(ownsandv1connect.NewMemberServiceHandler(api.MemberService{Server: a.API}, opt))
	add(ownsandv1connect.NewApiKeyServiceHandler(api.ApiKeyService{Server: a.API}, opt))
	add(ownsandv1connect.NewDeviceServiceHandler(api.DeviceService{Server: a.API}, opt))
	// Services of later layers answer unimplemented until they are added.
	add(ownsandv1connect.NewSandboxServiceHandler(ownsandv1connect.UnimplementedSandboxServiceHandler{}, opt))
	add(ownsandv1connect.NewSnapshotServiceHandler(ownsandv1connect.UnimplementedSnapshotServiceHandler{}, opt))
	add(ownsandv1connect.NewAgentServiceHandler(ownsandv1connect.UnimplementedAgentServiceHandler{}, opt))
	add(ownsandv1connect.NewDataPathServiceHandler(ownsandv1connect.UnimplementedDataPathServiceHandler{}, opt))
	add(ownsandv1connect.NewRelayServiceHandler(ownsandv1connect.UnimplementedRelayServiceHandler{}, opt))
	add(ownsandv1connect.NewRelayAgentServiceHandler(ownsandv1connect.UnimplementedRelayAgentServiceHandler{}, opt))
	a.Handler = server.NewHandler(routes, cfg.CORSAllowedOrigins)
	return a
}

// UseLease connects the single-active-instance lease.
func (a *App) UseLease(m *lease.Manager) { a.Lease = m }

// ObjectStore returns the R2 client, or nil when R2 is not configured.
func (a *App) ObjectStore() jobs.ObjectStore { return nil }

// Start runs the background components until ctx ends.
func (a *App) Start(ctx context.Context) {}

// Drain runs when the server starts to shut down.
func (a *App) Drain(ctx context.Context) {}
