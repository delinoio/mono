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

	"github.com/delinoio/mono/go/ownsand/validate"
	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/auth"
	"github.com/delinoio/mono/servers/ownsand-api/internal/authz"
	"github.com/delinoio/mono/servers/ownsand-api/internal/db"
	"github.com/delinoio/mono/servers/ownsand-api/internal/events"
	"github.com/delinoio/mono/servers/ownsand-api/internal/hub"
	"github.com/delinoio/mono/servers/ownsand-api/internal/objectstore"
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
	// client address is known. It holds one bucket per address, keyed by the
	// bare address, that the device RPCs, AcceptInvitation and failed API key
	// validations share.
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

	// Agent channel dependencies.
	Hub   *hub.Hub
	Lease LeaseHolder
	// Objects signs snapshot part URLs and deletes R2 prefixes; nil when R2
	// is not configured.
	Objects objectstore.Store
	// STUNURLs are sent to agents in the configuration message and to
	// callers by GetIceServers.
	STUNURLs []string
	// SnapshotPartSizeBytes is the export part size.
	SnapshotPartSizeBytes int64
	// Timing of the contract; tests shorten them.
	Timing Timing

	// DataPath handles the data path envelopes of the agent channel
	// (signaling answers, revocation and relay grant acknowledgements and
	// releases) and the revocation events. Nil until the data path exists.
	DataPath DataPathHooks

	watch    sandboxWatch
	commands commandStore // pending commands by command ID
	// imports holds the snapshot imports that importThenCreate waits for;
	// only a channel with an entry gets download URLs for that snapshot.
	imports activeImports
}

// LeaseHolder reports whether this instance holds the instance lease.
type LeaseHolder interface {
	Held() bool
	// WhileHeld runs fn only while the instance holds the lease, with the
	// ownership lock held, and reports whether it ran. The transition that
	// clears ownership takes the same lock before it closes the agent
	// channels, so a channel registered in fn is either closed by that
	// transition or never registered. fn must do no I/O and must not call
	// the lease.
	WhileHeld(fn func()) bool
}

// DataPathHooks receives the data path parts of the agent channel.
type DataPathHooks interface {
	// HandleEnvelope handles a data path envelope from the agent and reports
	// whether it was one.
	HandleEnvelope(ctx context.Context, conn *hub.Conn, env *ownsandv1.AgentEnvelope) bool
	// HandleEvent applies a revocation event.
	HandleEvent(ctx context.Context, e events.Event)
	// SweepExpired deletes the pending revocations that ended.
	SweepExpired(ctx context.Context)
}

// Timing holds the durations of the agent channel rules.
type Timing struct {
	// HeartbeatTimeout closes a channel without a heartbeat (45 seconds).
	HeartbeatTimeout time.Duration
	// CommandWait bounds the wait of PauseSandbox, ResumeSandbox and
	// DeleteSandbox for the agent result (60 seconds).
	CommandWait time.Duration
	// EventPoll is the poll interval of the control event processor.
	EventPoll time.Duration
	// OfflineSweep is the interval of the check that marks devices offline
	// that are online without a channel on the lease holder.
	OfflineSweep time.Duration
	// CleanupPoll is the interval of the R2 clean-up worker.
	CleanupPoll time.Duration
	// ImportWait bounds a snapshot import before the create.
	ImportWait time.Duration
	// CommandGrace is how long the server keeps the record of a command
	// whose channel closed before it was answered (2 minutes); zero means
	// the default. The sweep that removes expired records runs every
	// OfflineSweep.
	CommandGrace time.Duration
	// WriterDrainTimeout bounds the wait for the channel writer after a
	// channel closed; a writer that is still blocked in a send after it
	// loses its transport write (5 seconds); zero means the default.
	WriterDrainTimeout time.Duration
	// ExportApplyRetryWindow bounds how long the channel read loop retries
	// to apply an export result after a transient database error (15
	// seconds, below the heartbeat timeout because the loop reads no
	// heartbeat meanwhile); zero means the default.
	ExportApplyRetryWindow time.Duration
	// ExportApplyRetryBackoff is the first wait between two attempts to
	// apply an export result (250 milliseconds); it doubles up to 2 seconds;
	// zero means the default.
	ExportApplyRetryBackoff time.Duration
}

// DefaultTiming returns the contract values.
func DefaultTiming() Timing {
	return Timing{
		HeartbeatTimeout: 45 * time.Second,
		CommandWait:      60 * time.Second,
		EventPoll:        time.Second,
		OfflineSweep:     5 * time.Second,
		CleanupPoll:      30 * time.Second,
		ImportWait:       2 * time.Hour,
		CommandGrace:     DefaultCommandGrace,

		WriterDrainTimeout: DefaultWriterDrainTimeout,

		ExportApplyRetryWindow:  DefaultExportApplyRetryWindow,
		ExportApplyRetryBackoff: DefaultExportApplyRetryBackoff,
	}
}

// writerDrainTimeout returns the wait for the channel writer after a channel
// closed.
func (s *Service) writerDrainTimeout() time.Duration {
	if s.Timing.WriterDrainTimeout > 0 {
		return s.Timing.WriterDrainTimeout
	}
	return DefaultWriterDrainTimeout
}

// commandGrace returns the retention window of unanswered commands whose
// channel closed.
func (s *Service) commandGrace() time.Duration {
	if s.Timing.CommandGrace > 0 {
		return s.Timing.CommandGrace
	}
	return DefaultCommandGrace
}

// PendingCommands returns the number of command records that wait for a
// result, including the records that started to expire (tests).
func (s *Service) PendingCommands() int { return s.commands.len() }

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

// normalizeID validates a required UUID v7 field like validateID and returns
// its lowercase canonical text. The API accepts either hex case, but identity
// comparisons and rate-limit keys must use the value, not its spelling.
func normalizeID(field, value string) (string, error) {
	if err := validateID(field, value); err != nil {
		return "", err
	}
	id, err := uuidv7.Parse(value)
	if err != nil {
		return "", errInvalid(err)
	}
	return id.String(), nil
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
	orgID, err = normalizeID("organization_id", orgID)
	if err != nil {
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
