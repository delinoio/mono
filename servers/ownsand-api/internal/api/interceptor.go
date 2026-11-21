// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
	"github.com/delinoio/mono/servers/ownsand-api/internal/auth"
	"github.com/delinoio/mono/servers/ownsand-api/internal/config"
)

// credentialPolicy says which credentials an RPC accepts.
type credentialPolicy int

const (
	// policyUserOrAPIKey: a Logto access token or an API key (every public
	// RPC with an organization_id).
	policyUserOrAPIKey credentialPolicy = iota
	// policyUserOnly: a Logto access token only (the three public RPCs
	// without organization_id).
	policyUserOnly
	// policyNone: no bearer credential (Enroll, CreateChallenge,
	// Authenticate, ConfirmRemoval).
	policyNone
	// policyDevice: an `osd_` device session credential (Channel and
	// RelayAgentService.Connect).
	policyDevice
)

func policyFor(procedure string) credentialPolicy {
	switch procedure {
	case ownsandv1connect.AgentServiceEnrollProcedure,
		ownsandv1connect.AgentServiceCreateChallengeProcedure,
		ownsandv1connect.AgentServiceAuthenticateProcedure,
		ownsandv1connect.AgentServiceConfirmRemovalProcedure:
		return policyNone
	case ownsandv1connect.AgentServiceChannelProcedure,
		ownsandv1connect.RelayAgentServiceConnectProcedure:
		return policyDevice
	case ownsandv1connect.OrganizationServiceCreateOrganizationProcedure,
		ownsandv1connect.OrganizationServiceListOrganizationsProcedure,
		ownsandv1connect.MemberServiceAcceptInvitationProcedure:
		return policyUserOnly
	}
	return policyUserOrAPIKey
}

// rpcInfo collects the fields of the per-RPC log record. The authentication
// interceptor and the handlers fill it.
type rpcInfo struct {
	mu             sync.Mutex
	requestID      string
	principal      auth.Principal
	organizationID string
	clientAddr     string
	// entered is set when the logging interceptor runs, which tells the
	// relay request gate whether the RPC ever reached the interceptor chain.
	entered bool
}

type rpcInfoKey struct{}

func rpcInfoFrom(ctx context.Context) *rpcInfo {
	info, _ := ctx.Value(rpcInfoKey{}).(*rpcInfo)
	return info
}

// requestID returns the request ID of the RPC, which audit events record.
func requestID(ctx context.Context) string {
	if info := rpcInfoFrom(ctx); info != nil {
		return info.requestID
	}
	return ""
}

// setRPCOrganization records the organization of the RPC for its log record.
func setRPCOrganization(ctx context.Context, orgID string) {
	if info := rpcInfoFrom(ctx); info != nil {
		info.mu.Lock()
		info.organizationID = orgID
		info.mu.Unlock()
	}
}

func clientAddr(ctx context.Context) string {
	if info := rpcInfoFrom(ctx); info != nil {
		return info.clientAddr
	}
	return ""
}

// Interceptors returns the logging, metering and authentication
// interceptors in the order that handlers must apply them.
func (s *Service) Interceptors(authn *Authenticator, mode config.ClientAddressMode) []connect.Interceptor {
	return []connect.Interceptor{
		&logInterceptor{logger: s.Logger, now: s.Now, addressMode: mode},
		&meterInterceptor{s: s},
		&authInterceptor{authn: authn},
	}
}

// ClientAddress returns the client address of a request for the mode, or ""
// when the request has none, in which case the per-client rate limits do not
// apply. Verified proxy mode uses only the last entry of the last
// X-Forwarded-For header, which is the one that the Caddy reverse proxy
// appends, and never falls back to the TCP peer (the proxy itself). Direct
// mode uses the TCP peer and ignores forwarded headers. Any other mode,
// including the zero value, gives no address.
func ClientAddress(peerAddr string, header http.Header, mode config.ClientAddressMode) string {
	switch mode {
	case config.ClientAddressVerifiedProxy:
		values := header.Values("X-Forwarded-For")
		if len(values) == 0 {
			return ""
		}
		parts := strings.Split(values[len(values)-1], ",")
		if last := strings.TrimSpace(parts[len(parts)-1]); net.ParseIP(last) != nil {
			return last
		}
		return ""
	case config.ClientAddressDirect:
		host, _, err := net.SplitHostPort(peerAddr)
		if err != nil {
			return ""
		}
		return host
	default:
		return ""
	}
}

type logInterceptor struct {
	logger      *slog.Logger
	now         func() time.Time
	addressMode config.ClientAddressMode
}

// start returns ctx with the RPC's log fields. The relay request gate starts
// them before the interceptor chain runs, and the chain then continues with
// the same fields and request ID.
func (l *logInterceptor) start(ctx context.Context, peer connect.Peer, header http.Header) (context.Context, *rpcInfo) {
	if info := rpcInfoFrom(ctx); info != nil {
		info.mu.Lock()
		info.entered = true
		info.mu.Unlock()
		return ctx, info
	}
	info := &rpcInfo{requestID: newID(), clientAddr: ClientAddress(peer.Addr, header, l.addressMode)}
	return context.WithValue(ctx, rpcInfoKey{}, info), info
}

func (l *logInterceptor) log(ctx context.Context, procedure string, info *rpcInfo, start time.Time, err error) {
	info.mu.Lock()
	attrs := []slog.Attr{
		slog.String("procedure", procedure),
		slog.String("request_id", info.requestID),
		slog.String("principal_kind", info.principal.Kind.String()),
		slog.String("principal_id", info.principal.ID),
		slog.String("organization_id", info.organizationID),
		slog.String("code", codeString(err)),
		slog.Int64("duration_ms", l.now().Sub(start).Milliseconds()),
	}
	info.mu.Unlock()
	l.logger.LogAttrs(ctx, slog.LevelInfo, "rpc", attrs...)
}

// codeString is the Connect code that the logs record for err. A handler
// that ends because its caller went away returns the bare context error,
// which connect.CodeOf reports as unknown.
func codeString(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, context.Canceled):
		return connect.CodeCanceled.String()
	case errors.Is(err, context.DeadlineExceeded):
		return connect.CodeDeadlineExceeded.String()
	}
	return connect.CodeOf(err).String()
}

func (l *logInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		start := l.now()
		ctx, info := l.start(ctx, req.Peer(), req.Header())
		resp, err := next(ctx, req)
		l.log(ctx, req.Spec().Procedure, info, start, err)
		return resp, err
	}
}

func (l *logInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (l *logInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		start := l.now()
		ctx, info := l.start(ctx, conn.Peer(), conn.RequestHeader())
		err := next(ctx, conn)
		l.log(ctx, conn.Spec().Procedure, info, start, err)
		return err
	}
}

type authInterceptor struct {
	authn *Authenticator
}

func (a *authInterceptor) authenticate(ctx context.Context, procedure string, header http.Header) (context.Context, error) {
	policy := policyFor(procedure)
	if policy == policyNone {
		return ctx, nil
	}
	// The relay request gate authenticates before the request body is read
	// and applies the same credential policy; only that gate stores a
	// principal before this interceptor runs, so it is not authenticated twice.
	if _, ok := auth.PrincipalFrom(ctx); ok {
		return ctx, nil
	}
	p, err := a.authn.Authenticate(ctx, header.Get("Authorization"), clientAddr(ctx))
	if err != nil {
		return ctx, err
	}
	if info := rpcInfoFrom(ctx); info != nil {
		info.mu.Lock()
		info.principal = p
		if p.OrganizationID != "" {
			info.organizationID = p.OrganizationID
		}
		info.mu.Unlock()
	}
	switch policy {
	case policyDevice:
		if !p.IsDevice() {
			return ctx, errPermission("only a device session credential is accepted here")
		}
	case policyUserOnly:
		if !p.IsUser() {
			return ctx, errPermission("only signed-in users can call this RPC")
		}
	case policyUserOrAPIKey:
		if !p.IsUser() && !p.IsAPIKey() {
			return ctx, errPermission("this credential is not accepted here")
		}
	}
	return auth.WithPrincipal(ctx, p), nil
}

func (a *authInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		ctx, err := a.authenticate(ctx, req.Spec().Procedure, req.Header())
		if err != nil {
			return nil, err
		}
		return next(ctx, req)
	}
}

func (a *authInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (a *authInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		ctx, err := a.authenticate(ctx, conn.Spec().Procedure, conn.RequestHeader())
		if err != nil {
			return err
		}
		return next(ctx, conn)
	}
}

// Authenticator selects the validator from the credential shape and never
// tries a second validator after the first one rejects the credential.
type Authenticator struct {
	Service *Service
	Logto   TokenValidator
}

// TokenValidator validates a Logto access token and returns the user ID.
type TokenValidator interface {
	Validate(ctx context.Context, raw string) (string, error)
}

// Authenticate validates the Authorization header value.
func (a *Authenticator) Authenticate(ctx context.Context, header, client string) (auth.Principal, error) {
	const bearer = "Bearer "
	if len(header) <= len(bearer) || !strings.EqualFold(header[:len(bearer)], bearer) {
		return auth.Principal{}, errUnauthenticated("missing bearer credential")
	}
	token := strings.TrimSpace(header[len(bearer):])
	switch auth.ClassifySecret(token) {
	case auth.SecretAPIKey:
		return a.apiKey(ctx, token, client)
	case auth.SecretDeviceSession:
		return a.deviceSession(ctx, token)
	case auth.SecretEnrollmentToken, auth.SecretInvitation:
		return auth.Principal{}, errUnauthenticated("this secret is not a bearer credential")
	}
	sub, err := a.Logto.Validate(ctx, token)
	if err != nil {
		return auth.Principal{}, errUnauthenticated("invalid access token")
	}
	return auth.Principal{Kind: ownsandv1.PrincipalKind_PRINCIPAL_KIND_USER, ID: sub}, nil
}

func (a *Authenticator) apiKey(ctx context.Context, token, client string) (auth.Principal, error) {
	s := a.Service
	prefix := auth.DisplayPrefix(token)
	if ok, retry := s.Limits.PerPrefix.Peek("apikey:" + prefix); !ok {
		return auth.Principal{}, errRateLimited(retry)
	}
	if client != "" {
		if ok, retry := s.Limits.PerClient.Peek(client); !ok {
			return auth.Principal{}, errRateLimited(retry)
		}
	}
	fail := func() (auth.Principal, error) {
		s.Limits.PerPrefix.Allow("apikey:"+prefix, 1)
		if client != "" {
			s.Limits.PerClient.Allow(client, 1)
		}
		return auth.Principal{}, errUnauthenticated("invalid API key")
	}
	if !auth.WellFormed(auth.SecretAPIKey, token) {
		return fail()
	}
	hash := auth.HashSecret(token)
	var id, orgID string
	var stored []byte
	err := s.DB.Pool.QueryRow(ctx, `SELECT k.api_key_id::text, k.organization_id::text, k.secret_sha256
		FROM api_keys k JOIN organizations o ON o.organization_id = k.organization_id
		WHERE k.secret_sha256 = $1 AND k.revoke_time IS NULL`, hash).Scan(&id, &orgID, &stored)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !auth.HashEqual(stored, hash)) {
		return fail()
	}
	if err != nil {
		return auth.Principal{}, s.internal(ctx, "load api key", err)
	}
	return auth.Principal{Kind: ownsandv1.PrincipalKind_PRINCIPAL_KIND_API_KEY, ID: id, OrganizationID: orgID}, nil
}

func (a *Authenticator) deviceSession(ctx context.Context, token string) (auth.Principal, error) {
	s := a.Service
	if !auth.WellFormed(auth.SecretDeviceSession, token) {
		return auth.Principal{}, errUnauthenticated("invalid device session credential")
	}
	hash := auth.HashSecret(token)
	var deviceID, orgID string
	var stored []byte
	err := s.DB.Pool.QueryRow(ctx, `SELECT d.device_id::text, d.organization_id::text, ds.credential_sha256
		FROM device_sessions ds JOIN devices d ON d.device_id = ds.device_id
		WHERE ds.credential_sha256 = $1 AND ds.expire_time > $2 AND d.state <> $3`,
		hash, s.now(), int16(ownsandv1.DeviceState_DEVICE_STATE_REMOVED)).Scan(&deviceID, &orgID, &stored)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !auth.HashEqual(stored, hash)) {
		return auth.Principal{}, errUnauthenticated("invalid device session credential")
	}
	if err != nil {
		return auth.Principal{}, s.internal(ctx, "load device session", err)
	}
	return auth.Principal{Kind: ownsandv1.PrincipalKind_PRINCIPAL_KIND_DEVICE, ID: deviceID, OrganizationID: orgID}, nil
}
