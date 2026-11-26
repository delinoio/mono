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

	"github.com/delinoio/mono/servers/sandrun-api/internal/auth"
	"github.com/delinoio/mono/servers/sandrun-api/internal/authz"
	"github.com/delinoio/mono/servers/sandrun-api/internal/config"
)

// rpcInfo collects the fields of the per-RPC log record. The interceptors
// and Authorize fill it.
type rpcInfo struct {
	mu             sync.Mutex
	rpcID          string
	principal      auth.Principal
	organizationID string
	clientAddr     string
}

type rpcInfoKey struct{}

func rpcInfoFrom(ctx context.Context) *rpcInfo {
	info, _ := ctx.Value(rpcInfoKey{}).(*rpcInfo)
	return info
}

// RPCID returns the correlation ID of the RPC (log field rpc_id), which audit
// events record. It is not the SandRun request ID (request_id), which names a
// unit of demand.
func RPCID(ctx context.Context) string {
	if info := rpcInfoFrom(ctx); info != nil {
		return info.rpcID
	}
	return ""
}

// SetRPCOrganization records the organization of the RPC for its log record.
func SetRPCOrganization(ctx context.Context, orgID string) {
	if info := rpcInfoFrom(ctx); info != nil {
		info.mu.Lock()
		info.organizationID = orgID
		info.mu.Unlock()
	}
}

// ClientAddr returns the client address of the RPC, or "" when it has none.
func ClientAddr(ctx context.Context) string {
	if info := rpcInfoFrom(ctx); info != nil {
		return info.clientAddr
	}
	return ""
}

// Interceptors returns the logging and authentication interceptors in the
// order that every handler (core and later layers) must apply them.
func (s *Service) Interceptors(authn *Authenticator, mode config.ClientAddressMode) []connect.Interceptor {
	return []connect.Interceptor{
		&logInterceptor{logger: s.Logger, now: s.Now, addressMode: mode},
		&authInterceptor{authn: authn},
	}
}

// ClientAddress returns the client address of a request for the mode, or ""
// when the request has none, in which case the per-client rate limit does
// not apply. The rules are the same as in ownsand-api: verified proxy mode
// uses only the last entry of the last X-Forwarded-For header, which the
// Caddy reverse proxy appends, and never falls back to the TCP peer (the
// proxy itself); direct mode uses the TCP peer and ignores forwarded headers.
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

func (l *logInterceptor) start(ctx context.Context, peer connect.Peer, header http.Header) (context.Context, *rpcInfo) {
	info := &rpcInfo{rpcID: NewID(), clientAddr: ClientAddress(peer.Addr, header, l.addressMode)}
	return context.WithValue(ctx, rpcInfoKey{}, info), info
}

func (l *logInterceptor) log(ctx context.Context, procedure string, info *rpcInfo, start time.Time, err error) {
	info.mu.Lock()
	attrs := []slog.Attr{
		slog.String("procedure", procedure),
		slog.String("rpc_id", info.rpcID),
		slog.String("principal_kind", info.principal.Kind.String()),
		slog.String("user_id", info.principal.UserID),
		slog.String("organization_id", info.organizationID),
		slog.String("code", codeString(err)),
		slog.Int64("duration_ms", l.now().Sub(start).Milliseconds()),
	}
	if info.principal.APIKeyID != "" {
		attrs = append(attrs, slog.String("api_key_id", info.principal.APIKeyID))
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

// authInterceptor authenticates every RPC. Every public RPC needs a
// credential. A personal API key is accepted only on DevSessionService RPCs
// (authz.APIKeyAcceptedForProcedure); on every other RPC a valid key gets
// permission_denied, and an invalid key gets unauthenticated like any
// invalid credential.
type authInterceptor struct {
	authn *Authenticator
}

func (a *authInterceptor) authenticate(ctx context.Context, procedure string, header http.Header) (context.Context, error) {
	p, err := a.authn.Authenticate(ctx, header.Get("Authorization"), ClientAddr(ctx))
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
	if p.IsAPIKey() {
		if !authz.APIKeyAcceptedForProcedure(procedure) {
			return ctx, ErrPermission("a personal API key is accepted only on DevSessionService RPCs")
		}
		// Only an accepted use counts as the last use of the key.
		a.authn.recordAPIKeyUse(ctx, p.APIKeyID)
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

// TokenValidator validates a Logto access token and returns the user ID.
type TokenValidator interface {
	Validate(ctx context.Context, raw string) (string, error)
}

// Authenticator selects the validator from the credential shape (the
// `sandrun_` prefix selects the personal API key validator) and never tries a
// second validator after the first one rejects the credential.
type Authenticator struct {
	Service *Service
	Logto   TokenValidator
}

// Authenticate validates the Authorization header value.
func (a *Authenticator) Authenticate(ctx context.Context, header, client string) (auth.Principal, error) {
	const bearer = "Bearer "
	if len(header) <= len(bearer) || !strings.EqualFold(header[:len(bearer)], bearer) {
		return auth.Principal{}, ErrUnauthenticated("missing bearer credential")
	}
	token := strings.TrimSpace(header[len(bearer):])
	switch auth.ClassifySecret(token) {
	case auth.SecretAPIKey:
		return a.apiKey(ctx, token, client)
	case auth.SecretInvitation:
		return auth.Principal{}, ErrUnauthenticated("an invitation secret is not a bearer credential")
	}
	sub, err := a.Logto.Validate(ctx, token)
	if err != nil {
		return auth.Principal{}, ErrUnauthenticated("invalid access token")
	}
	return auth.Principal{Kind: auth.PrincipalUser, UserID: sub}, nil
}

// apiKey validates a personal API key. Only failures take rate limit tokens,
// and a display prefix (or client address) without tokens is rejected before
// validation.
func (a *Authenticator) apiKey(ctx context.Context, token, client string) (auth.Principal, error) {
	s := a.Service
	prefixKey := "apikey:" + auth.DisplayPrefix(auth.SecretAPIKey, token)
	if ok, retry := s.Limits.PerPrefix.Peek(prefixKey); !ok {
		return auth.Principal{}, ErrRateLimited(retry)
	}
	if client != "" {
		if ok, retry := s.Limits.PerClient.Peek(client); !ok {
			return auth.Principal{}, ErrRateLimited(retry)
		}
	}
	fail := func() (auth.Principal, error) {
		s.Limits.PerPrefix.Allow(prefixKey, 1)
		if client != "" {
			s.Limits.PerClient.Allow(client, 1)
		}
		return auth.Principal{}, ErrUnauthenticated("invalid personal API key")
	}
	if !auth.WellFormed(auth.SecretAPIKey, token) {
		return fail()
	}
	hash := auth.HashSecret(token)
	var keyID, orgID, userID string
	var stored []byte
	var expires, revoked *time.Time
	err := s.DB.Pool.QueryRow(ctx, `SELECT api_key_id::text, organization_id::text, user_id, secret_sha256, expire_time, revoke_time
		FROM api_keys WHERE secret_sha256 = $1`, hash).Scan(&keyID, &orgID, &userID, &stored, &expires, &revoked)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !auth.HashEqual(stored, hash)) {
		return fail()
	}
	if err != nil {
		return auth.Principal{}, s.Internal(ctx, "load api key", err)
	}
	if revoked != nil || (expires != nil && !s.NowUTC().Before(*expires)) {
		return fail()
	}
	return auth.Principal{Kind: auth.PrincipalAPIKey, UserID: userID, APIKeyID: keyID, OrganizationID: orgID}, nil
}

// apiKeyUseCache remembers when this process last wrote the last use time of
// each key, so most calls skip the database write.
type apiKeyUseCache struct {
	mu      sync.Mutex
	written map[string]time.Time
}

// recordAPIKeyUse writes the last use time of a key at most once per minute.
// The UPDATE condition keeps the limit across instances too. A failed write
// is logged and does not fail the RPC.
func (a *Authenticator) recordAPIKeyUse(ctx context.Context, keyID string) {
	s := a.Service
	now := s.NowUTC()
	c := &s.apiKeyUses
	c.mu.Lock()
	if last, ok := c.written[keyID]; ok && now.Sub(last) < apiKeyUseWriteInterval {
		c.mu.Unlock()
		return
	}
	if c.written == nil || len(c.written) > 100_000 {
		c.written = map[string]time.Time{}
	}
	c.written[keyID] = now
	c.mu.Unlock()
	_, err := s.DB.Pool.Exec(ctx, `UPDATE api_keys SET last_used_time = $2
		WHERE api_key_id = $1 AND (last_used_time IS NULL OR last_used_time <= $2::timestamptz - interval '1 minute')`, keyID, now)
	if err != nil {
		s.Logger.WarnContext(ctx, "api_key_last_use_write_failed", slog.String("api_key_id", keyID), slog.Any("error", err))
	}
}
