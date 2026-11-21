// SPDX-License-Identifier: FSL-1.1-ALv2

// Package api implements the Connect services of accounts-api. The protos
// contract (docs/protos-delino-accounts-api-contract.md) owns every RPC, its
// request rules and its error codes; the API server contract
// (docs/servers-accounts-api-foundation.md) owns roles, storage and logging.
//
// Service holds the dependencies that every Connect service shares. Each
// Connect service is a small handler type that embeds *Service, and Mounts
// returns the path and the handler of every service that the server serves.
package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/go/uuidv7"
	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	"github.com/delinoio/mono/protos/delino/accounts/v1/accountsv1connect"
	"github.com/delinoio/mono/servers/accounts-api/internal/auth"
	"github.com/delinoio/mono/servers/accounts-api/internal/billing"
	"github.com/delinoio/mono/servers/accounts-api/internal/db"
	"github.com/delinoio/mono/servers/accounts-api/internal/directory"
	"github.com/delinoio/mono/servers/accounts-api/internal/email"
	"github.com/delinoio/mono/servers/accounts-api/internal/server"
)

// Deps are the dependencies of the services.
type Deps struct {
	DB *db.DB
	// Billing is the credit, subscription and top-up logic.
	Billing   *billing.Service
	Directory directory.Directory
	Email     email.Sender
	Logger    *slog.Logger
	// Now is the clock of business times (invitation expiry, audit events);
	// nil selects time.Now.
	Now func() time.Time
	// WebOrigin is the origin of the web app in invitation links.
	WebOrigin string
	// UsageClients maps each machine-to-machine client ID to the services
	// whose UsageService RPCs it may call (USAGE_SERVICE_CLIENTS).
	UsageClients map[string][]accountsv1.ServiceId
}

// Service is the shared state of the Connect services.
type Service struct {
	db        *db.DB
	billing   *billing.Service
	directory directory.Directory
	email     email.Sender
	logger    *slog.Logger
	now       func() time.Time
	webOrigin string

	usageClients map[string][]accountsv1.ServiceId
}

// New returns the shared service state.
func New(deps Deps) *Service {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	return &Service{
		db:        deps.DB,
		billing:   deps.Billing,
		directory: deps.Directory,
		email:     deps.Email,
		logger:    deps.Logger,
		now:       deps.Now,
		webOrigin: deps.WebOrigin,

		usageClients: deps.UsageClients,
	}
}

// Mounts returns the Connect handlers of every served service with the
// interceptors applied in order. A new service adds its handler here.
func (s *Service) Mounts(interceptors ...connect.Interceptor) []server.Mount {
	opts := []connect.HandlerOption{connect.WithInterceptors(interceptors...)}
	var mounts []server.Mount
	add := func(path string, handler http.Handler) {
		mounts = append(mounts, server.Mount{Path: path, Handler: handler})
	}
	add(accountsv1connect.NewBillingAccountServiceHandler(&BillingAccountHandler{s}, opts...))
	add(accountsv1connect.NewCreditServiceHandler(&CreditHandler{s}, opts...))
	add(accountsv1connect.NewBillingUsageServiceHandler(&BillingUsageHandler{s}, opts...))
	add(accountsv1connect.NewUsageServiceHandler(&UsageHandler{s}, opts...))
	return mounts
}

// timestamp returns the current business time at the precision of Postgres
// (microseconds), so a value written and read back compares equal.
func (s *Service) timestamp() time.Time {
	return s.now().UTC().Truncate(time.Microsecond)
}

// newID returns a new UUID v7 in canonical text form.
func newID() (string, error) {
	return uuidv7.NewString()
}

// callerUserID returns the Logto user ID of the authenticated user. The
// authentication interceptor guarantees a user caller on the user services.
func callerUserID(ctx context.Context) (string, error) {
	caller, ok := auth.CallerFrom(ctx)
	if !ok || caller.Kind != auth.TokenKindUser || caller.UserID() == "" {
		return "", connect.NewError(connect.CodeUnauthenticated, errors.New("missing access token"))
	}
	return caller.UserID(), nil
}

// Connect errors. Messages never contain secrets, tokens, email addresses or
// payment data.
func invalidArgument(msg string) error {
	return connect.NewError(connect.CodeInvalidArgument, errors.New(msg))
}

func notFound(msg string) error {
	return connect.NewError(connect.CodeNotFound, errors.New(msg))
}

func permissionDenied(msg string) error {
	return connect.NewError(connect.CodePermissionDenied, errors.New(msg))
}

func failedPrecondition(msg string) error {
	return connect.NewError(connect.CodeFailedPrecondition, errors.New(msg))
}

func alreadyExists(msg string) error {
	return connect.NewError(connect.CodeAlreadyExists, errors.New(msg))
}

func unavailable(msg string) error {
	return connect.NewError(connect.CodeUnavailable, errors.New(msg))
}

// internal logs err and returns a Connect internal error without its detail.
// Errors that already are Connect errors pass through unchanged.
func (s *Service) internal(ctx context.Context, op string, err error) error {
	var connectErr *connect.Error
	if errors.As(err, &connectErr) {
		return connectErr
	}
	if errors.Is(err, context.Canceled) {
		return connect.NewError(connect.CodeCanceled, errors.New("request canceled"))
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return connect.NewError(connect.CodeDeadlineExceeded, errors.New("request deadline exceeded"))
	}
	s.logger.ErrorContext(ctx, "rpc_internal_error", slog.String("operation", op), slog.Any("error", err))
	return connect.NewError(connect.CodeInternal, errors.New("internal error"))
}
