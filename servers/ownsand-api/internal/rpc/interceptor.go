// SPDX-License-Identifier: FSL-1.1-ALv2

// Package rpc holds the Connect interceptor that authenticates every RPC,
// applies the procedure access rule and writes one log record per RPC.
package rpc

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/auth"
	"github.com/delinoio/mono/servers/ownsand-api/internal/authz"
)

// Interceptor authenticates and logs RPCs.
type Interceptor struct {
	Authn  *auth.Authenticator
	Logger *slog.Logger
}

var _ connect.Interceptor = (*Interceptor)(nil)

type orgRequest interface{ GetOrganizationId() string }

// authenticate validates the bearer credential according to the procedure's
// access rule and returns the context with the principal.
func (i *Interceptor) authenticate(ctx context.Context, procedure, authorization string) (context.Context, auth.Principal, error) {
	rule, ok := authz.Procedures[procedure]
	if !ok {
		return ctx, auth.Principal{}, connect.NewError(connect.CodeUnimplemented, errors.New("unknown procedure"))
	}
	if rule.Access == authz.AccessAgentUnauthenticated {
		return ctx, auth.Principal{}, nil
	}
	token, ok := auth.BearerToken(authorization)
	if !ok {
		return ctx, auth.Principal{}, auth.ErrMissingCredential
	}
	p, err := i.Authn.Authenticate(ctx, token)
	if err != nil {
		return ctx, auth.Principal{}, err
	}
	if _, err := authz.CheckAccess(procedure, p.Kind); err != nil {
		return ctx, p, err
	}
	return auth.WithPrincipal(ctx, p), p, nil
}

// WrapUnary implements connect.Interceptor.
func (i *Interceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if req.Spec().IsClient {
			return next(ctx, req)
		}
		start := time.Now()
		procedure := req.Spec().Procedure
		ctx, p, err := i.authenticate(ctx, procedure, req.Header().Get("Authorization"))
		var resp connect.AnyResponse
		if err == nil {
			resp, err = next(ctx, req)
		}
		orgID := ""
		if m, ok := req.Any().(orgRequest); ok {
			orgID = m.GetOrganizationId()
		}
		i.log(procedure, p, orgID, err, time.Since(start))
		return resp, err
	}
}

// WrapStreamingClient implements connect.Interceptor.
func (i *Interceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

// WrapStreamingHandler implements connect.Interceptor. The organization ID of
// a streaming RPC is logged by the handler itself, because the request
// message arrives after the interceptor runs.
func (i *Interceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		start := time.Now()
		procedure := conn.Spec().Procedure
		ctx, p, err := i.authenticate(ctx, procedure, conn.RequestHeader().Get("Authorization"))
		if err == nil {
			ctx = WithLogFields(ctx)
			err = next(ctx, conn)
		}
		i.log(procedure, p, LogOrganization(ctx), err, time.Since(start))
		return err
	}
}

func (i *Interceptor) log(procedure string, p auth.Principal, orgID string, err error, d time.Duration) {
	code := "ok"
	level := slog.LevelInfo
	if err != nil {
		code = connect.CodeOf(err).String()
		level = slog.LevelWarn
		if connect.CodeOf(err) == connect.CodeInternal || connect.CodeOf(err) == connect.CodeUnknown {
			level = slog.LevelError
		}
	}
	attrs := []slog.Attr{
		slog.String("procedure", procedure),
		slog.String("principal_kind", principalKindName(p.Kind)),
		slog.String("principal_id", p.ID),
		slog.String("organization_id", orgID),
		slog.String("code", code),
		slog.Duration("duration", d),
	}
	if err != nil && level == slog.LevelError {
		// Internal errors carry no credential or sandbox content: handlers
		// wrap storage errors without request values.
		attrs = append(attrs, slog.String("error", err.Error()))
	}
	i.Logger.LogAttrs(context.Background(), level, "rpc", attrs...)
}

func principalKindName(k ownsandv1.PrincipalKind) string {
	switch k {
	case ownsandv1.PrincipalKind_PRINCIPAL_KIND_USER:
		return "user"
	case ownsandv1.PrincipalKind_PRINCIPAL_KIND_API_KEY:
		return "api_key"
	case ownsandv1.PrincipalKind_PRINCIPAL_KIND_DEVICE:
		return "device"
	case ownsandv1.PrincipalKind_PRINCIPAL_KIND_SYSTEM:
		return "system"
	default:
		return "none"
	}
}

type logFieldsKey struct{}

type logFields struct{ organizationID string }

// WithLogFields attaches mutable log fields to a streaming RPC context.
func WithLogFields(ctx context.Context) context.Context {
	return context.WithValue(ctx, logFieldsKey{}, &logFields{})
}

// SetLogOrganization records the organization ID of a streaming RPC for its
// log record.
func SetLogOrganization(ctx context.Context, organizationID string) {
	if f, ok := ctx.Value(logFieldsKey{}).(*logFields); ok {
		f.organizationID = organizationID
	}
}

// LogOrganization returns the organization ID recorded with SetLogOrganization.
func LogOrganization(ctx context.Context) string {
	if f, ok := ctx.Value(logFieldsKey{}).(*logFields); ok {
		return f.organizationID
	}
	return ""
}
