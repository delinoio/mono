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
	"slices"
	"sync"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/go/uuidv7"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
	"github.com/delinoio/mono/servers/ownsand-api/internal/api"
	"github.com/delinoio/mono/servers/ownsand-api/internal/auth"
	"github.com/delinoio/mono/servers/ownsand-api/internal/config"
	"github.com/delinoio/mono/servers/ownsand-api/internal/db"
	"github.com/delinoio/mono/servers/ownsand-api/internal/hub"
	"github.com/delinoio/mono/servers/ownsand-api/internal/janitor"
	"github.com/delinoio/mono/servers/ownsand-api/internal/lease"
	"github.com/delinoio/mono/servers/ownsand-api/internal/objectstore"
	"github.com/delinoio/mono/servers/ownsand-api/internal/relay"
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

	CORSOrigins []string
	// ClientAddressMode selects the source of the client address for rate
	// limits. The zero value gives requests no client address.
	ClientAddressMode config.ClientAddressMode

	DisplayNames api.DisplayNameSource
	// Limits overrides the default rate limits (tests).
	Limits *api.Limits
	// JanitorInterval is the retention job interval; zero selects the
	// default.
	JanitorInterval time.Duration

	// STUNURLs are sent to agents and callers.
	STUNURLs []string
	// Objects signs snapshot part URLs; nil disables exports.
	Objects               objectstore.Store
	SnapshotPartSizeBytes int64
	// Timing overrides the contract durations (tests).
	Timing *api.Timing
	// InstanceID names this instance in the lease; empty selects a new
	// UUID v7.
	InstanceID string
	// LeaseDeadlineNow is the clock of the local lease ownership deadline;
	// nil selects the real clock (tests).
	LeaseDeadlineNow func() time.Time

	// RelayLimits and RelayTiming override the relay limits and durations
	// (tests).
	RelayLimits *relay.Limits
	RelayTiming *api.RelayTiming
}

// App is a wired control plane.
type App struct {
	opts    Options
	Service *api.Service
	Lease   *lease.Lease
	Control *api.Control
	Plane   *api.DataPlane
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
		opts.Logger.Warn("nonce_key_ephemeral", slog.String("detail", "NONCE_KEY is unset; nonces are valid only in this process"))
	}
	limits := api.DefaultLimits(opts.Now)
	if opts.Limits != nil {
		limits = *opts.Limits
	}
	timing := api.DefaultTiming()
	if opts.Timing != nil {
		timing = *opts.Timing
	}
	if opts.InstanceID == "" {
		id, err := uuidv7.NewString()
		if err != nil {
			return nil, err
		}
		opts.InstanceID = id
	}
	if opts.SnapshotPartSizeBytes == 0 {
		opts.SnapshotPartSizeBytes = 64 << 20
	}
	svc := &api.Service{
		DB:                    opts.DB,
		Logger:                opts.Logger,
		Now:                   opts.Now,
		Nonces:                auth.NewNonceIssuer(key, opts.NonceKeyPrevious, opts.Now),
		Limits:                limits,
		DisplayNames:          opts.DisplayNames,
		Hub:                   hub.New(),
		Objects:               opts.Objects,
		STUNURLs:              opts.STUNURLs,
		SnapshotPartSizeBytes: opts.SnapshotPartSizeBytes,
		Timing:                timing,
	}
	control := api.NewControl(svc)
	l := &lease.Lease{DB: opts.DB, InstanceID: opts.InstanceID, Now: opts.Now, Logger: opts.Logger,
		DeadlineNow: opts.LeaseDeadlineNow, OnAcquire: control.LeaseAcquired, OnLose: control.LeaseLost}
	svc.Lease = l
	relayLimits := relay.DefaultLimits()
	if opts.RelayLimits != nil {
		relayLimits = *opts.RelayLimits
	}
	relayTiming := api.DefaultRelayTiming()
	if opts.RelayTiming != nil {
		relayTiming = *opts.RelayTiming
	}
	plane := api.NewDataPlane(svc, relayLimits, relayTiming)
	authn := &api.Authenticator{Service: svc, Logto: opts.Logto}
	handlerOpts := []connect.HandlerOption{connect.WithInterceptors(svc.Interceptors(authn, opts.ClientAddressMode)...)}

	var mounts []server.Mount
	add := func(path string, h http.Handler) { mounts = append(mounts, server.Mount{Path: path, Handler: h}) }
	add(ownsandv1connect.NewOrganizationServiceHandler(api.OrganizationHandler{Service: svc}, handlerOpts...))
	add(ownsandv1connect.NewMemberServiceHandler(api.MemberHandler{Service: svc}, handlerOpts...))
	add(ownsandv1connect.NewApiKeyServiceHandler(api.APIKeyHandler{Service: svc}, handlerOpts...))
	add(ownsandv1connect.NewDeviceServiceHandler(api.DeviceHandler{Service: svc}, handlerOpts...))
	agentPath, agentHandler := ownsandv1connect.NewAgentServiceHandler(api.AgentHandler{Service: svc}, handlerOpts...)
	add(agentPath, api.WithStreamControl(agentHandler))
	add(ownsandv1connect.NewSandboxServiceHandler(api.SandboxHandler{Service: svc}, handlerOpts...))
	add(ownsandv1connect.NewSnapshotServiceHandler(api.SnapshotHandler{Service: svc}, handlerOpts...))
	add(ownsandv1connect.NewDataPathServiceHandler(api.DataPathHandler{DataPlane: plane}, handlerOpts...))
	// The relay gate opens the relay stream and arms the request deadline
	// before Connect reads the request body; RelayReadDeadlines gives it the
	// means to interrupt a body read that blocks.
	relayOpts := append(slices.Clone(handlerOpts), connect.WithRequestGate(plane.RelayGate(authn, opts.ClientAddressMode)))
	relayPath, relayHandler := ownsandv1connect.NewRelayServiceHandler(api.RelayHandler{DataPlane: plane}, relayOpts...)
	add(relayPath, api.RelayReadDeadlines(relayHandler))
	relayAgentPath, relayAgentHandler := ownsandv1connect.NewRelayAgentServiceHandler(api.RelayAgentHandler{DataPlane: plane}, handlerOpts...)
	add(relayAgentPath, api.RelayAgentWriteInterrupt(relayAgentHandler))

	return &App{
		opts:    opts,
		Service: svc,
		Lease:   l,
		Control: control,
		Plane:   plane,
		handler: server.NewHandler(server.Options{Logger: opts.Logger, CORSOrigins: opts.CORSOrigins, Mounts: mounts}),
	}, nil
}

// Handler returns the root HTTP handler.
func (a *App) Handler() http.Handler { return a.handler }

// Run runs the background workers until ctx ends and waits for them.
func (a *App) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		janitor.Run(ctx, a.opts.DB, a.opts.Logger, a.opts.Now, a.opts.JanitorInterval)
	}()
	go func() {
		defer wg.Done()
		// Run releases the lease when ctx ends, which closes every agent
		// channel before the HTTP server drains.
		a.Lease.Run(ctx)
	}()
	go func() {
		defer wg.Done()
		a.Control.Run(ctx)
	}()
	wg.Wait()
}
