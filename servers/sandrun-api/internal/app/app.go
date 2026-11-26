// SPDX-License-Identifier: FSL-1.1-ALv2

// Package app wires sandrun-api: the Connect handlers with their interceptors,
// the plain HTTP endpoints, the instance lease and the lease-scoped
// components. cmd/sandrun-api and the in-process integration tests both build
// it.
//
// Later layers plug in through Extensions. Each Extension receives a
// Registry, which gives it the shared *api.Service and lets it mount Connect
// services with the same interceptors, mount plain HTTP endpoints
// (webhooks), register lease-scoped components and daily jobs, and register
// membership end and organization deletion hooks. For example, the dev
// session layer would add to cmd/sandrun-api:
//
//	app.Options{..., Extensions: []app.Extension{devsession.Register}}
//
//	func Register(r *app.Registry) error {
//		h := &Handler{Service: r.Service}
//		r.Handle(sandrunv1connect.NewDevSessionServiceHandler(h, r.HandlerOptions()...))
//		r.OnMembershipEnd(api.MembershipEndHook{Name: "dev_sessions", InTx: h.endSessionsTx, AfterCommit: h.deleteSandboxes})
//		r.AddLeaseComponent(lease.ComponentFunc{ComponentName: "dev_session_reconciler", RunFunc: h.reconcile})
//		r.AddDailyJob(jobs.SQLJob(r.Service.DB, "ended_requests", "DELETE FROM requests WHERE ..."))
//		return nil
//	}
package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"connectrpc.com/connect"
	"go.opentelemetry.io/otel/metric"

	"github.com/delinoio/mono/protos/delino/sandrun/v1/sandrunv1connect"
	"github.com/delinoio/mono/servers/sandrun-api/internal/api"
	"github.com/delinoio/mono/servers/sandrun-api/internal/config"
	"github.com/delinoio/mono/servers/sandrun-api/internal/db"
	"github.com/delinoio/mono/servers/sandrun-api/internal/jobs"
	"github.com/delinoio/mono/servers/sandrun-api/internal/lease"
	"github.com/delinoio/mono/servers/sandrun-api/internal/secrets"
	"github.com/delinoio/mono/servers/sandrun-api/internal/server"
)

// Extension adds a later layer to the control plane. New calls each
// extension once, in order, after the core services are registered.
type Extension func(r *Registry) error

// Options configures the control plane.
type Options struct {
	DB     *db.DB
	Logger *slog.Logger
	Now    func() time.Time

	Logto   api.TokenValidator
	Secrets *secrets.Keyring

	CORSOrigins []string
	// ClientAddressMode selects the source of the client address for rate
	// limits. The zero value gives requests no client address.
	ClientAddressMode config.ClientAddressMode

	DisplayNames api.DisplayNameSource
	// Limits overrides the default rate limits (tests).
	Limits *api.Limits
	// Metrics provides the meters of the SandRun metrics; nil records none.
	Metrics metric.MeterProvider

	// InstanceID names this instance in the lease; empty selects a new
	// UUID v7.
	InstanceID string
	// LeaseDeadlineNow is the clock of the local lease ownership deadline;
	// nil selects the real clock (tests).
	LeaseDeadlineNow func() time.Time
	// DailyJobInterval overrides the 24-hour interval of the daily jobs
	// (tests).
	DailyJobInterval time.Duration

	// Extensions are the later layers.
	Extensions []Extension
}

// App is a wired control plane.
type App struct {
	Service *api.Service
	Lease   *lease.Lease
	handler http.Handler
}

// Registry collects what the core and the extensions add to the control
// plane. It is valid only during New.
type Registry struct {
	// Service holds the shared dependencies: the database, the logger, the
	// clock, the secrets keyring, the rate limits, the metrics and
	// Service.Authorize and the other helpers of package api.
	Service *api.Service
	// Lease reports whether this instance holds the lease (Lease.Held), for
	// work that must run only on the holder outside a lease component.
	Lease *lease.Lease

	handlerOptions []connect.HandlerOption
	mounts         []server.Mount
	patterns       map[string]bool
	supervisor     *lease.Supervisor
	daily          *jobs.Daily
	err            error
}

// HandlerOptions returns the Connect handler options of every public
// service: the logging and authentication interceptors and the request size
// limit. Build every Connect handler with them; a handler without them
// would serve RPCs without authentication.
func (r *Registry) HandlerOptions() []connect.HandlerOption {
	return slices.Clone(r.handlerOptions)
}

// Handle mounts a handler on an http.ServeMux pattern. Use it with the path
// and handler that a generated sandrunv1connect.New...ServiceHandler returns,
// or for a plain HTTP endpoint such as "POST /webhooks/github", which must
// verify its sender itself and bound its body. A repeated pattern fails New.
func (r *Registry) Handle(pattern string, h http.Handler) {
	if r.patterns[pattern] {
		r.err = fmt.Errorf("app: pattern %q is registered twice", pattern)
		return
	}
	r.patterns[pattern] = true
	r.mounts = append(r.mounts, server.Mount{Path: pattern, Handler: h})
}

// AddLeaseComponent registers a component that runs only while this
// instance holds the lease (see lease.Component).
func (r *Registry) AddLeaseComponent(c lease.Component) { r.supervisor.Add(c) }

// AddDailyJob registers a daily job of the lease holder (see jobs.Job).
func (r *Registry) AddDailyJob(j jobs.Job) { r.daily.Jobs = append(r.daily.Jobs, j) }

// OnMembershipEnd registers a membership end hook (see api.MembershipEndHook).
func (r *Registry) OnMembershipEnd(h api.MembershipEndHook) {
	r.Service.Hooks.MembershipEnd = append(r.Service.Hooks.MembershipEnd, h)
}

// OnOrganizationDeletion registers an organization deletion hook (see
// api.OrganizationDeletionHook). Register the hooks in the clean-up order
// of the contract.
func (r *Registry) OnOrganizationDeletion(h api.OrganizationDeletionHook) {
	r.Service.Hooks.OrganizationDeletion = append(r.Service.Hooks.OrganizationDeletion, h)
}

// registerCore mounts the services of the core layer and its daily job.
func registerCore(r *Registry) error {
	s := r.Service
	r.Handle(sandrunv1connect.NewOrganizationServiceHandler(api.OrganizationHandler{Service: s}, r.HandlerOptions()...))
	r.Handle(sandrunv1connect.NewMemberServiceHandler(api.MemberHandler{Service: s}, r.HandlerOptions()...))
	r.Handle(sandrunv1connect.NewApiKeyServiceHandler(api.APIKeyHandler{Service: s}, r.HandlerOptions()...))
	r.Handle(sandrunv1connect.NewAuditServiceHandler(api.AuditHandler{Service: s}, r.HandlerOptions()...))
	r.AddDailyJob(jobs.AuditRetention(s.DB))
	return nil
}

// New wires the control plane.
func New(opts Options) (*App, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Secrets == nil {
		return nil, fmt.Errorf("app: a secrets keyring is required")
	}
	limits := api.DefaultLimits(opts.Now)
	if opts.Limits != nil {
		limits = *opts.Limits
	}
	if opts.InstanceID == "" {
		opts.InstanceID = api.NewID()
	}
	svc := &api.Service{
		DB:           opts.DB,
		Logger:       opts.Logger,
		Now:          opts.Now,
		Secrets:      opts.Secrets,
		Limits:       limits,
		DisplayNames: opts.DisplayNames,
		Metrics:      opts.Metrics,
	}
	supervisor := &lease.Supervisor{Logger: opts.Logger}
	l := &lease.Lease{
		DB: opts.DB, InstanceID: opts.InstanceID, Logger: opts.Logger, DeadlineNow: opts.LeaseDeadlineNow,
		OnAcquire: func(context.Context) { supervisor.Start() },
		OnLose:    supervisor.Stop,
	}
	daily := &jobs.Daily{Logger: opts.Logger, Now: opts.Now, Interval: opts.DailyJobInterval}
	authn := &api.Authenticator{Service: svc, Logto: opts.Logto}
	r := &Registry{
		Service: svc,
		Lease:   l,
		// WithReadMaxBytes runs before the interceptors, so an oversized
		// request ends with resource_exhausted before authentication.
		handlerOptions: []connect.HandlerOption{
			connect.WithInterceptors(svc.Interceptors(authn, opts.ClientAddressMode)...),
			connect.WithReadMaxBytes(api.MaxRequestBytes),
		},
		patterns:   map[string]bool{},
		supervisor: supervisor,
		daily:      daily,
	}
	for _, ext := range append([]Extension{registerCore}, opts.Extensions...) {
		if err := ext(r); err != nil {
			return nil, err
		}
		if r.err != nil {
			return nil, r.err
		}
	}
	// The daily jobs run as one lease component after the components of the
	// layers.
	supervisor.Add(daily)
	return &App{
		Service: svc,
		Lease:   l,
		handler: server.NewHandler(server.Options{Logger: opts.Logger, CORSOrigins: opts.CORSOrigins, Mounts: r.mounts}),
	}, nil
}

// Handler returns the root HTTP handler.
func (a *App) Handler() http.Handler { return a.handler }

// Run keeps the lease until ctx ends and then releases it. While the lease
// is held, the registered components run. Run returns after the release, so
// the components have stopped before the HTTP server drains.
func (a *App) Run(ctx context.Context) {
	a.Lease.Run(ctx)
}
