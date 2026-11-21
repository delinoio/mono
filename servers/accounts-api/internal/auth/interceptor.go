// SPDX-License-Identifier: FSL-1.1-ALv2

package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/protos/delino/accounts/v1/accountsv1connect"
)

// Scopes of the accounts-api API resource.
const (
	// ScopeBilling is the scope of the user services.
	ScopeBilling = "accounts:billing"
	// ScopeReportUsage is the scope of the service-facing UsageService.
	ScopeReportUsage = "accounts:report-usage"
)

// TokenKind is the kind of Logto access token that a service accepts.
type TokenKind int

const (
	// TokenKindUser is an end-user access token; its `sub` is the caller.
	TokenKindUser TokenKind = iota + 1
	// TokenKindMachineToMachine is a client credentials token of a consuming
	// service.
	TokenKindMachineToMachine
)

func (k TokenKind) String() string {
	switch k {
	case TokenKindUser:
		return "user"
	case TokenKindMachineToMachine:
		return "machine_to_machine"
	default:
		return "unknown"
	}
}

// Requirement is the scope and the token kind that a Connect service needs.
type Requirement struct {
	Scope string
	Kind  TokenKind
}

// Policy maps a fully qualified Connect service name to its requirement. A
// procedure of a service without an entry is denied.
type Policy map[string]Requirement

// DefaultPolicy is the requirement of every mounted service. Add the entry of
// a service in the change that mounts its handler.
var DefaultPolicy = Policy{
	accountsv1connect.BillingAccountServiceName: {Scope: ScopeBilling, Kind: TokenKindUser},
	accountsv1connect.CreditServiceName:         {Scope: ScopeBilling, Kind: TokenKindUser},
	accountsv1connect.BillingUsageServiceName:   {Scope: ScopeBilling, Kind: TokenKindUser},
	accountsv1connect.UsageServiceName:          {Scope: ScopeReportUsage, Kind: TokenKindMachineToMachine},
}

// Validator validates a raw bearer token.
type Validator interface {
	Validate(ctx context.Context, raw string) (Token, error)
}

// Caller is the authenticated caller of an RPC.
type Caller struct {
	Token Token
	Kind  TokenKind
}

// UserID returns the Logto user ID of a user caller.
func (c Caller) UserID() string { return c.Token.Subject }

type callerKey struct{}

// WithCaller stores the caller in ctx.
func WithCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerKey{}, c)
}

// CallerFrom returns the caller of ctx.
func CallerFrom(ctx context.Context) (Caller, bool) {
	c, ok := ctx.Value(callerKey{}).(Caller)
	return c, ok
}

// Additional rejection reasons of the interceptor.
const (
	RejectNoPolicy     RejectReason = "no_policy"
	RejectTokenKind    RejectReason = "token_kind"
	RejectMissingScope RejectReason = "missing_scope"
)

// Interceptor authenticates every RPC and stores its Caller in the context.
type Interceptor struct {
	validator Validator
	policy    Policy
	// resource is the API resource indicator, for the logs.
	resource string
	logger   *slog.Logger
}

// NewInterceptor returns the authentication interceptor.
func NewInterceptor(validator Validator, policy Policy, resource string, logger *slog.Logger) *Interceptor {
	return &Interceptor{validator: validator, policy: policy, resource: resource, logger: logger}
}

// serviceOf returns the service name of a procedure `/<service>/<method>`.
func serviceOf(procedure string) string {
	service, _, _ := strings.Cut(strings.TrimPrefix(procedure, "/"), "/")
	return service
}

// bearer returns the token of an `Authorization: Bearer <token>` header.
func bearer(header http.Header) string {
	value := header.Get("Authorization")
	scheme, token, ok := strings.Cut(value, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}

// authenticate returns the caller of a request or the Connect error.
func (i *Interceptor) authenticate(ctx context.Context, procedure string, header http.Header) (Caller, error) {
	raw := bearer(header)
	if raw == "" {
		i.reject(ctx, procedure, RejectMissingToken, "")
		return Caller{}, connect.NewError(connect.CodeUnauthenticated, errors.New("missing access token"))
	}
	token, err := i.validator.Validate(ctx, raw)
	if err != nil {
		reason := RejectMalformed
		var invalidErr *InvalidTokenError
		if errors.As(err, &invalidErr) {
			reason = invalidErr.Reason
		}
		i.reject(ctx, procedure, reason, "")
		return Caller{}, connect.NewError(connect.CodeUnauthenticated, errors.New("invalid access token"))
	}
	requirement, ok := i.policy[serviceOf(procedure)]
	if !ok {
		i.reject(ctx, procedure, RejectNoPolicy, token.Subject)
		return Caller{}, connect.NewError(connect.CodePermissionDenied, errors.New("the procedure is not available"))
	}
	kind := TokenKindUser
	if token.MachineToMachine() {
		kind = TokenKindMachineToMachine
	}
	if kind != requirement.Kind {
		i.reject(ctx, procedure, RejectTokenKind, token.Subject)
		return Caller{}, connect.NewError(connect.CodePermissionDenied, errors.New("this service does not accept this kind of access token"))
	}
	if !token.HasScope(requirement.Scope) {
		i.reject(ctx, procedure, RejectMissingScope, token.Subject)
		return Caller{}, connect.NewError(connect.CodePermissionDenied, errors.New("the access token lacks the scope "+requirement.Scope))
	}
	return Caller{Token: token, Kind: kind}, nil
}

func (i *Interceptor) reject(ctx context.Context, procedure string, reason RejectReason, subject string) {
	i.logger.LogAttrs(ctx, slog.LevelInfo, "auth_rejected",
		slog.String("procedure", procedure),
		slog.String("api_resource", i.resource),
		slog.String("reason", string(reason)),
		slog.String("user_id", subject))
}

// WrapUnary implements connect.Interceptor.
func (i *Interceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		caller, err := i.authenticate(ctx, req.Spec().Procedure, req.Header())
		if err != nil {
			return nil, err
		}
		return next(WithCaller(ctx, caller), req)
	}
}

// WrapStreamingClient implements connect.Interceptor; the server has no
// clients of its own API.
func (i *Interceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

// WrapStreamingHandler implements connect.Interceptor with the same checks
// as WrapUnary.
func (i *Interceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		caller, err := i.authenticate(ctx, conn.Spec().Procedure, conn.RequestHeader())
		if err != nil {
			return err
		}
		return next(WithCaller(ctx, caller), conn)
	}
}
