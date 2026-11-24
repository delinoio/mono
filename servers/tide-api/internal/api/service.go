// SPDX-License-Identifier: FSL-1.1-ALv2

// Package api implements the Connect handlers of the tide-api core layer
// (OrganizationService, MemberService, ApiKeyService and AuditService) and
// the shared pieces that every later layer uses: the authentication
// interceptor, Service.Authorize, the audit writer, the rate limits, the
// error helpers and the extension hooks (hooks.go). The owning contracts are
// docs/servers-tide-api-foundation.md and docs/protos-tide-api-contract.md.
//
// A later layer that lives in its own package builds its handlers on a
// *Service and mounts them through app.Registry, which applies the same
// interceptors. The exported helpers below are the stable surface for those
// layers.
package api

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/delinoio/mono/go/uuidv7"
	"github.com/delinoio/mono/servers/tide-api/internal/db"
	"github.com/delinoio/mono/servers/tide-api/internal/ratelimit"
	"github.com/delinoio/mono/servers/tide-api/internal/secrets"
)

// Durations and sizes of the contract.
const (
	// InvitationLifetime is the validity of an invitation secret.
	InvitationLifetime = 7 * 24 * time.Hour
	// maxOrganizationNameLength bounds an organization name in characters.
	maxOrganizationNameLength = 100
	// maxAPIKeyNameLength bounds a personal API key name in characters.
	maxAPIKeyNameLength = 64
	// apiKeyUseWriteInterval is the shortest interval between two writes of
	// the last use time of one personal API key.
	apiKeyUseWriteInterval = time.Minute
	// MaxRequestBytes bounds a request message. The largest request is a
	// WriteDevSessionFile chunk of at most 1 MiB with its path and
	// identifiers, so 2 MiB leaves room without allowing large messages.
	MaxRequestBytes = 2 << 20
)

// DisplayNameSource reads member display names from the delino-accounts
// API. A nil source, or an error, gives members without display names. The
// delino-accounts API has no display name RPC yet (see the Dependencies and
// Integrations section of docs/servers-ownsand-api-foundation.md), so
// production wires no source and ListMembers returns empty display names,
// as in ownsand-api, until that RPC exists.
type DisplayNameSource interface {
	DisplayNames(ctx context.Context, userIDs []string) (map[string]string, error)
}

// Limits holds the rate limiters of the Security section of the contract.
// The service runs as one replica, so in-memory buckets are service-wide.
type Limits struct {
	// PerPrefix limits AcceptInvitation per invitation display prefix and
	// failed personal API key authentications per key display prefix (10
	// per minute, burst 10). Keys are namespaced ("invitation:",
	// "apikey:").
	PerPrefix *ratelimit.Limiter
	// PerUser limits AcceptInvitation per user and the OwnSand link
	// validation of SetOwnSandLink per user (10 per minute, burst 10). Keys
	// are namespaced ("accept-invitation:", "ownsand-link:").
	PerUser *ratelimit.Limiter
	// PerClient is the one bucket per client address (120 per minute, burst
	// 60) that the limits above share when the request has a client
	// address.
	PerClient *ratelimit.Limiter
}

// DefaultLimits returns the recorded limits.
func DefaultLimits(now func() time.Time) Limits {
	return Limits{
		PerPrefix: ratelimit.New(10.0/60, 10, now),
		PerUser:   ratelimit.New(10.0/60, 10, now),
		PerClient: ratelimit.New(2, 60, now),
	}
}

// Service holds the dependencies of every handler, of the core layer and of
// later layers.
type Service struct {
	DB     *db.DB
	Logger *slog.Logger
	Now    func() time.Time
	// Secrets encrypts stored organization credentials.
	Secrets      *secrets.Keyring
	Limits       Limits
	DisplayNames DisplayNameSource
	// Metrics provides the meters of the tide metrics; nil records none.
	Metrics metric.MeterProvider
	// Hooks are the extension hooks of later layers (see hooks.go). They are
	// set while the app is wired and must not change afterwards.
	Hooks Hooks
	// OnWorkChange is set by the work layer: it tells the dispatcher of this
	// instance that the work of an organization changed (a request arrived,
	// a profile or the link changed), so it runs a pass at once. Without it,
	// or on an instance that does not hold the lease, the periodic pass
	// picks the change up.
	OnWorkChange func(orgID string)

	apiKeyUses apiKeyUseCache
}

// WorkChanged calls OnWorkChange when it is set.
func (s *Service) WorkChanged(orgID string) {
	if s.OnWorkChange != nil {
		s.OnWorkChange(orgID)
	}
}

// NowUTC returns the current time in UTC.
func (s *Service) NowUTC() time.Time { return s.Now().UTC() }

// NewID returns a new UUID v7 string.
func NewID() string {
	id, err := uuidv7.NewString()
	if err != nil {
		// crypto/rand does not fail on supported platforms.
		panic(err)
	}
	return id
}

// Errors. Messages never contain secrets, credentials or user content.

// ErrInvalidf returns invalid_argument.
func ErrInvalidf(format string, args ...any) error {
	return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf(format, args...))
}

// ErrNotFound returns not_found for an entity.
func ErrNotFound(what string) error {
	return connect.NewError(connect.CodeNotFound, fmt.Errorf("%s not found", what))
}

// ErrPermission returns permission_denied.
func ErrPermission(msg string) error {
	return connect.NewError(connect.CodePermissionDenied, errors.New(msg))
}

// ErrPrecondition returns failed_precondition.
func ErrPrecondition(msg string) error {
	return connect.NewError(connect.CodeFailedPrecondition, errors.New(msg))
}

// ErrAlreadyExists returns already_exists.
func ErrAlreadyExists(msg string) error {
	return connect.NewError(connect.CodeAlreadyExists, errors.New(msg))
}

// ErrUnauthenticated returns unauthenticated.
func ErrUnauthenticated(msg string) error {
	return connect.NewError(connect.CodeUnauthenticated, errors.New(msg))
}

// ErrUnavailable returns unavailable.
func ErrUnavailable(msg string) error {
	return connect.NewError(connect.CodeUnavailable, errors.New(msg))
}

// ErrRateLimited returns resource_exhausted with the retry delay in whole
// seconds.
func ErrRateLimited(retry time.Duration) error {
	seconds := int64(math.Ceil(retry.Seconds()))
	return connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("rate limit exceeded; retry in %d s", max(seconds, 1)))
}

// Internal passes through Connect errors and maps context errors to their
// Connect codes. It logs unexpected errors and returns a generic message.
func (s *Service) Internal(ctx context.Context, op string, err error) error {
	if ce := new(connect.Error); errors.As(err, &ce) {
		return err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return connect.NewError(connect.CodeDeadlineExceeded, errors.New("request deadline exceeded"))
	}
	if errors.Is(err, context.Canceled) {
		return connect.NewError(connect.CodeCanceled, errors.New("request canceled"))
	}
	s.Logger.ErrorContext(ctx, "internal_error", slog.String("op", op), slog.Any("error", err))
	return connect.NewError(connect.CodeInternal, errors.New("internal error"))
}

// NormalizeID validates a required UUID v7 field and returns its lowercase
// canonical text, so identity comparisons and rate limit keys use the value,
// not its spelling.
func NormalizeID(field, value string) (string, error) {
	if value == "" {
		return "", ErrInvalidf("invalid %s: is required", field)
	}
	id, err := uuidv7.Parse(value)
	if err != nil {
		return "", ErrInvalidf("invalid %s: must be a UUID v7", field)
	}
	return id.String(), nil
}

// ValidateName checks a display name: 1 to maxChars characters of valid
// UTF-8, not only spaces, without control characters.
func ValidateName(field, name string, maxChars int) error {
	if strings.TrimSpace(name) == "" {
		return ErrInvalidf("invalid %s: is required", field)
	}
	if !utf8.ValidString(name) {
		return ErrInvalidf("invalid %s: must be valid UTF-8", field)
	}
	if utf8.RuneCountInString(name) > maxChars {
		return ErrInvalidf("invalid %s: must be at most %d characters", field, maxChars)
	}
	for _, r := range name {
		if r < 0x20 || (r >= 0x7f && r < 0xa0) {
			return ErrInvalidf("invalid %s: must not contain control characters", field)
		}
	}
	return nil
}

// Pagination. A page token is the unpadded base64url form of `v1:` and the
// last returned UUID; it is opaque to callers.

const pageTokenPrefix = "v1:"

// Page size rules of the protos contract.
const (
	defaultPageSize = 50
	maxPageSize     = 200
)

// PageArgs validates page_size and page_token and returns the limit and the
// UUID after which the page starts ("" for the first page).
func PageArgs(size int32, token string) (int32, string, error) {
	switch {
	case size < 0:
		return 0, "", ErrInvalidf("invalid page_size: must not be negative")
	case size == 0:
		size = defaultPageSize
	case size > maxPageSize:
		size = maxPageSize
	}
	if token == "" {
		return size, "", nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || !strings.HasPrefix(string(raw), pageTokenPrefix) {
		return 0, "", ErrInvalidf("invalid page_token")
	}
	after := strings.TrimPrefix(string(raw), pageTokenPrefix)
	if !uuidv7.Valid(after) {
		return 0, "", ErrInvalidf("invalid page_token")
	}
	return size, after, nil
}

// NextPageToken returns the token after lastKey when the page is full.
func NextPageToken(count int, limit int32, lastKey string) string {
	if count < int(limit) || lastKey == "" {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString([]byte(pageTokenPrefix + lastKey))
}

// AfterKey returns the lower bound of an ascending keyset page: the nil
// UUID when the page starts at the beginning.
func AfterKey(after string) string {
	if after == "" {
		return "00000000-0000-0000-0000-000000000000"
	}
	return after
}

// BeforeKey returns the upper bound of a descending keyset page: the max
// UUID when the page starts at the beginning.
func BeforeKey(after string) string {
	if after == "" {
		return "ffffffff-ffff-ffff-ffff-ffffffffffff"
	}
	return after
}

// Timestamp converts a time; the zero time gives nil.
func Timestamp(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

// TimestampPtr converts a nullable time.
func TimestampPtr(t *time.Time) *timestamppb.Timestamp {
	if t == nil {
		return nil
	}
	return timestamppb.New(*t)
}
