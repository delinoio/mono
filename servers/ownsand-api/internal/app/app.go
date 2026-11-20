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
	"github.com/delinoio/mono/servers/ownsand-api/internal/agent"
	"github.com/delinoio/mono/servers/ownsand-api/internal/api"
	"github.com/delinoio/mono/servers/ownsand-api/internal/auth"
	"github.com/delinoio/mono/servers/ownsand-api/internal/config"
	"github.com/delinoio/mono/servers/ownsand-api/internal/jobs"
	"github.com/delinoio/mono/servers/ownsand-api/internal/lease"
	"github.com/delinoio/mono/servers/ownsand-api/internal/nonce"
	"github.com/delinoio/mono/servers/ownsand-api/internal/r2"
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
	// Per device ID limit of CreateChallenge, Authenticate and ConfirmRemoval.
	deviceRate  = 1.0
	deviceBurst = 20
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
	Agents  *agent.Hub
	R2      *r2.Client
}

// leaseRef lets the hub ask the lease manager that main connects later.
type leaseRef struct{ a *App }

func (l leaseRef) Held() bool { return l.a.Lease != nil && l.a.Lease.Held() }

// Options replace dependencies in tests.
type Options struct {
	Tokens       auth.TokenValidator
	DisplayNames accounts.DisplayNameSource
	// R2HTTP replaces the R2 HTTP client in tests.
	R2HTTP *http.Client
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
	a.API = &api.Server{Store: st, Logger: logger, DisplayNames: names, InvitationFailures: ratelimit.New(credentialFailureRate, credentialFailureBurst, nil)}
	interceptor := &rpc.Interceptor{Authn: a.Authn, Logger: logger}
	opt := connect.WithInterceptors(interceptor)
	routes := []server.Route{}
	add := func(path string, h http.Handler) { routes = append(routes, server.Route{Path: path, Handler: h}) }
	add(ownsandv1connect.NewOrganizationServiceHandler(api.OrganizationService{Server: a.API}, opt))
	add(ownsandv1connect.NewMemberServiceHandler(api.MemberService{Server: a.API}, opt))
	add(ownsandv1connect.NewApiKeyServiceHandler(api.ApiKeyService{Server: a.API}, opt))
	add(ownsandv1connect.NewDeviceServiceHandler(api.DeviceService{Server: a.API}, opt))
	a.Agents = agent.NewHub(st, logger, leaseRef{a})
	a.Agents.ReleaseManifestURL = cfg.Extra.ReleaseManifestURL
	if cfg.Extra.R2.Configured() {
		a.R2 = &r2.Client{Endpoint: cfg.Extra.R2.Endpoint, Bucket: cfg.Extra.R2.Bucket, AccessKeyID: cfg.Extra.R2.AccessKeyID,
			SecretAccessKey: cfg.Extra.R2.SecretAccessKey, HTTP: opts.R2HTTP}
		a.Agents.SignedURLs = &agent.R2SignedURLs{Store: st, R2: a.R2}
	} else {
		logger.Warn("r2_not_configured", slog.String("effect", "snapshot export is unavailable and R2 clean-up jobs stay pending"))
	}
	if len(cfg.Extra.NonceKey) == 0 {
		logger.Warn("nonce_key_not_configured", slog.String("effect", "device challenges are unavailable"))
	}
	agentService := &agent.Service{
		Hub: a.Agents, Store: st, Logger: logger,
		Nonces:      nonce.Keys{Current: cfg.Extra.NonceKey, Previous: cfg.Extra.NonceKeyPrevious},
		DeviceLimit: ratelimit.New(deviceRate, deviceBurst, nil),
		EnrollLimit: ratelimit.New(credentialFailureRate, credentialFailureBurst, nil),
	}
	add(ownsandv1connect.NewSandboxServiceHandler(api.SandboxService{Server: a.API, Agents: a.Agents}, opt))
	add(ownsandv1connect.NewSnapshotServiceHandler(api.SnapshotService{Server: a.API, Agents: a.Agents, ExportConfigured: a.R2 != nil}, opt))
	add(ownsandv1connect.NewAgentServiceHandler(agentService, opt))
	// Services of later layers answer unimplemented until they are added.
	add(ownsandv1connect.NewDataPathServiceHandler(ownsandv1connect.UnimplementedDataPathServiceHandler{}, opt))
	add(ownsandv1connect.NewRelayServiceHandler(ownsandv1connect.UnimplementedRelayServiceHandler{}, opt))
	add(ownsandv1connect.NewRelayAgentServiceHandler(ownsandv1connect.UnimplementedRelayAgentServiceHandler{}, opt))
	a.Handler = server.NewHandler(routes, cfg.CORSAllowedOrigins)
	return a
}

// UseLease connects the single-active-instance lease. Losing the lease
// closes every agent channel, so agents reconnect to the new holder.
func (a *App) UseLease(m *lease.Manager) {
	a.Lease = m
	prevLost := m.OnLost
	m.OnLost = func() {
		a.Agents.CloseAll("lease lost")
		if prevLost != nil {
			prevLost()
		}
	}
}

// ObjectStore returns the R2 client, or nil when R2 is not configured.
func (a *App) ObjectStore() jobs.ObjectStore {
	if a.R2 == nil {
		return nil
	}
	return a.R2
}

// Start runs the background components until ctx ends.
func (a *App) Start(ctx context.Context) {
	go a.Agents.RunLeaseWork(ctx)
}

// Drain runs when the server starts to shut down: it closes every agent
// channel.
func (a *App) Drain(ctx context.Context) {
	a.Agents.CloseAll("server draining")
}
