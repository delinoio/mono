// SPDX-License-Identifier: FSL-1.1-ALv2

// Package api implements the Connect handlers of the control plane. The
// owning contracts are docs/servers-ownsand-api-foundation.md and
// docs/protos-ownsand-api-contract.md.
package api

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/delinoio/mono/packages/ownsand-sdk-go/uuidv7"
	"github.com/delinoio/mono/packages/ownsand-sdk-go/validate"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/auth"
	"github.com/delinoio/mono/servers/ownsand-api/internal/authz"
	"github.com/delinoio/mono/servers/ownsand-api/internal/db"
	"github.com/delinoio/mono/servers/ownsand-api/internal/ratelimit"
)

// Durations of the contract.
const (
	invitationLifetime     = 7 * 24 * time.Hour
	deviceSessionLifetime  = 15 * time.Minute
	tombstoneLifetime      = 365 * 24 * time.Hour
	signedURLLifetime      = 15 * time.Minute
	maxNameLength          = 100
	organizationKeyPrefix  = "organizations/"
	snapshotObjectsSegment = "/snapshots/"
)

// DisplayNameSource reads member display names from the delino-accounts
// API. A nil source, or an error, gives members without display names.
type DisplayNameSource interface {
	DisplayNames(ctx context.Context, userIDs []string) (map[string]string, error)
}

// Limits holds the rate limiters of the unauthenticated and secret-taking
// RPCs (see the Security section of the contract).
type Limits struct {
	// PerDevice limits Enroll, CreateChallenge, Authenticate and
	// ConfirmRemoval per device ID (Enroll per token prefix).
	PerDevice *ratelimit.Limiter
	// PerPrefix limits AcceptInvitation per invitation prefix and failed API
	// key validations per key prefix.
	PerPrefix *ratelimit.Limiter
	// PerClient limits the same RPCs per client address when a trusted
	// client address is known.
	PerClient *ratelimit.Limiter
}

// DefaultLimits returns the recorded limits: 10 requests per device ID or
// prefix per minute with a burst of 10, and 120 per client address per
// minute with a burst of 60.
func DefaultLimits(now func() time.Time) Limits {
	return Limits{
		PerDevice: ratelimit.New(10.0/60, 10, now),
		PerPrefix: ratelimit.New(10.0/60, 10, now),
		PerClient: ratelimit.New(2, 60, now),
	}
}

// Service holds the dependencies of every handler.
type Service struct {
	DB           *db.DB
	Logger       *slog.Logger
	Now          func() time.Time
	Nonces       *auth.NonceIssuer
	Limits       Limits
	DisplayNames DisplayNameSource
	// Notify, when set, tells the lease holder's event processor that a
	// transaction wrote control events, so it does not wait for its next
	// poll.
	Notify func()
}

func (s *Service) notify() {
	if s.Notify != nil {
		s.Notify()
	}
}

// afterDeviceRemoved runs after a device removal committed. The lease
// holder sends the clean-up command and closes the channel when it
// processes the KindDeviceRemoved event.
func (s *Service) afterDeviceRemoved(ctx context.Context, deviceID string) {
	s.Logger.InfoContext(ctx, "device_removed", slog.String("device_id", deviceID))
	s.notify()
}

func (s *Service) now() time.Time { return s.Now().UTC() }

// newID returns a new UUID v7 string.
func newID() string {
	id, err := uuidv7.NewString()
	if err != nil {
		// crypto/rand does not fail on supported platforms.
		panic(err)
	}
	return id
}

// Errors. Messages never contain secrets or sandbox content.

func errInvalid(err error) error { return connect.NewError(connect.CodeInvalidArgument, err) }

func errInvalidf(format string, args ...any) error {
	return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf(format, args...))
}

func errNotFound(what string) error {
	return connect.NewError(connect.CodeNotFound, fmt.Errorf("%s not found", what))
}

func errPermission(msg string) error {
	return connect.NewError(connect.CodePermissionDenied, errors.New(msg))
}

func errPrecondition(msg string) error {
	return connect.NewError(connect.CodeFailedPrecondition, errors.New(msg))
}

func errUnavailable(msg string) error {
	return connect.NewError(connect.CodeUnavailable, errors.New(msg))
}

func errAlreadyExists(msg string) error {
	return connect.NewError(connect.CodeAlreadyExists, errors.New(msg))
}

func errUnauthenticated(msg string) error {
	return connect.NewError(connect.CodeUnauthenticated, errors.New(msg))
}

// errRateLimited returns resource_exhausted with the retry delay in an
// ErrorDetail.
func errRateLimited(retry time.Duration) error {
	e := connect.NewError(connect.CodeResourceExhausted, errors.New("rate limit exceeded"))
	if d, err := connect.NewErrorDetail(&ownsandv1.ErrorDetail{
		Code:       ownsandv1.ErrorCode_ERROR_CODE_RESOURCE_EXHAUSTED,
		Message:    "rate limit exceeded",
		RetryAfter: durationProto(retry),
	}); err == nil {
		e.AddDetail(d)
	}
	return e
}

// internal wraps an unexpected error. The detail goes to the log, not to
// the caller.
func (s *Service) internal(ctx context.Context, op string, err error) error {
	if ce := new(connect.Error); errors.As(err, &ce) {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return connect.NewError(connect.CodeCanceled, errors.New("request canceled"))
	}
	s.Logger.ErrorContext(ctx, "internal_error", slog.String("op", op), slog.Any("error", err))
	return connect.NewError(connect.CodeInternal, errors.New("internal error"))
}

// validateID checks a required UUID v7 field.
func validateID(field, value string) error {
	if err := validate.UUIDv7(field, value); err != nil {
		return errInvalid(err)
	}
	return nil
}

// validateName checks a display name: 1 to 100 bytes of valid UTF-8 without
// control characters.
func validateName(field, name string) error {
	if strings.TrimSpace(name) == "" {
		return errInvalidf("invalid %s: is required", field)
	}
	if len(name) > maxNameLength {
		return errInvalidf("invalid %s: must be at most %d bytes", field, maxNameLength)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f || r == 0xfffd {
			return errInvalidf("invalid %s: must not contain control characters", field)
		}
	}
	return nil
}

// principal returns the authenticated principal of ctx.
func principal(ctx context.Context) (auth.Principal, error) {
	p, ok := auth.PrincipalFrom(ctx)
	if !ok {
		return auth.Principal{}, errUnauthenticated("missing credential")
	}
	return p, nil
}

// authorize applies the authorization matrix for an RPC that takes an
// organization_id. A user without a membership and an API key of another
// organization get not_found; a matrix rejection gets permission_denied.
// It returns the principal and, for users, the role.
func (s *Service) authorize(ctx context.Context, q db.Querier, orgID string, action authz.Action) (auth.Principal, ownsandv1.Role, error) {
	p, err := principal(ctx)
	if err != nil {
		return p, 0, err
	}
	if err := validateID("organization_id", orgID); err != nil {
		return p, 0, err
	}
	setRPCOrganization(ctx, orgID)
	switch p.Kind {
	case ownsandv1.PrincipalKind_PRINCIPAL_KIND_USER:
		var role int16
		err := q.QueryRow(ctx, "SELECT role FROM memberships WHERE organization_id = $1 AND user_id = $2", orgID, p.ID).Scan(&role)
		if errors.Is(err, pgx.ErrNoRows) {
			return p, 0, errNotFound("organization")
		}
		if err != nil {
			return p, 0, s.internal(ctx, "load membership", err)
		}
		r := ownsandv1.Role(role)
		if !authz.RoleAllowed(r, action) {
			return p, r, errPermission("the caller's role does not allow this action")
		}
		return p, r, nil
	case ownsandv1.PrincipalKind_PRINCIPAL_KIND_API_KEY:
		if p.OrganizationID != orgID {
			return p, 0, errNotFound("organization")
		}
		if !authz.APIKeyAllowed(action) {
			return p, 0, errPermission("API keys cannot perform this action")
		}
		return p, 0, nil
	}
	return p, 0, errPermission("this credential is not accepted here")
}

// requireUser returns the principal of an RPC without organization_id,
// which accepts only Logto users.
func requireUser(ctx context.Context) (auth.Principal, error) {
	p, err := principal(ctx)
	if err != nil {
		return p, err
	}
	if !p.IsUser() {
		return p, errPermission("only signed-in users can call this RPC")
	}
	return p, nil
}

// Pagination. A page token is the unpadded base64url form of `v1:` and the
// last returned key; it is opaque to callers.

const pageTokenPrefix = "v1:"

func pageArgs(size int32, token string) (int32, string, error) {
	limit, err := validate.PageSize(size)
	if err != nil {
		return 0, "", errInvalid(err)
	}
	if token == "" {
		return limit, "", nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || !strings.HasPrefix(string(raw), pageTokenPrefix) {
		return 0, "", errInvalidf("invalid page_token")
	}
	after := strings.TrimPrefix(string(raw), pageTokenPrefix)
	if !uuidv7.Valid(after) {
		return 0, "", errInvalidf("invalid page_token")
	}
	return limit, after, nil
}

// nextPageToken returns the token after lastKey when the page is full.
func nextPageToken(count int, limit int32, lastKey string) string {
	if count < int(limit) || lastKey == "" {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString([]byte(pageTokenPrefix + lastKey))
}

// afterKey returns the lower bound of a keyset page: the nil UUID when the
// page starts at the beginning.
func afterKey(after string) string {
	if after == "" {
		return "00000000-0000-0000-0000-000000000000"
	}
	return after
}

func ts(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

func tsPtr(t *time.Time) *timestamppb.Timestamp {
	if t == nil {
		return nil
	}
	return timestamppb.New(*t)
}

func principalProto(kind int16, id string) *ownsandv1.Principal {
	return &ownsandv1.Principal{Kind: ownsandv1.PrincipalKind(kind), Id: id}
}
