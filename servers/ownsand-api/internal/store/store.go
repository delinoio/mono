// SPDX-License-Identifier: FSL-1.1-ALv2

// Package store is the Postgres storage of the control plane. Every query of
// an organization-owned table filters on organization_id. Each exported
// mutation runs in one transaction together with its audit event and its
// control event, according to docs/servers-ownsand-api-foundation.md.
package store

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/delinoio/mono/packages/ownsand-sdk-go/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// Errors returned by store methods.
var (
	ErrNotFound      = errors.New("not found")
	ErrAlreadyExists = errors.New("already exists")
	ErrConflict      = errors.New("conflict")
)

// Store is the Postgres storage.
type Store struct {
	Pool *pgxpool.Pool
	// Now is the clock; tests replace it.
	Now func() time.Time
}

// Open connects to Postgres and checks the connection.
func Open(ctx context.Context, url string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		// pgx error messages can include the connection string; do not wrap.
		return nil, errors.New("DATABASE_URL is not a valid Postgres connection string")
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	return &Store{Pool: pool, Now: time.Now}, nil
}

// Close closes the pool.
func (s *Store) Close() { s.Pool.Close() }

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC().Truncate(time.Microsecond)
	}
	return time.Now().UTC().Truncate(time.Microsecond)
}

// tx runs fn in a transaction.
func (s *Store) tx(ctx context.Context, fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, s.Pool, fn)
}

// NewID returns a new UUID v7 identifier.
func NewID() string { return datapath.NewID() }

// ValidID reports whether s is a canonical UUID. Malformed IDs are treated as
// unknown entities by the handlers.
func ValidID(s string) bool {
	_, err := datapath.ParseUUID(s)
	return err == nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// Page is a decoded list position.
type Page struct {
	Size  int
	After string
}

// Page size defaults and limits of every list RPC.
const (
	DefaultPageSize = 50
	MaxPageSize     = 200
)

// ErrBadPageToken is returned for a page token that this server did not issue.
var ErrBadPageToken = errors.New("invalid page token")

// ParsePage validates page_size and page_token.
func ParsePage(size int32, token string) (Page, error) {
	if size < 0 {
		return Page{}, errors.New("page_size must not be negative")
	}
	p := Page{Size: int(size)}
	if p.Size == 0 {
		p.Size = DefaultPageSize
	}
	if p.Size > MaxPageSize {
		p.Size = MaxPageSize
	}
	if token != "" {
		raw, err := base64.RawURLEncoding.DecodeString(token)
		if err != nil || !ValidID(string(raw)) {
			return Page{}, ErrBadPageToken
		}
		p.After = string(raw)
	}
	return p, nil
}

// NextToken returns the page token after the last ID of a full page.
func NextToken(ids []string, size int) string {
	if len(ids) < size || len(ids) == 0 {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString([]byte(ids[len(ids)-1]))
}

// afterArg returns the SQL argument for keyset pagination by UUID v7 ID.
func afterArg(p Page) string {
	if p.After == "" {
		return "00000000-0000-0000-0000-000000000000"
	}
	return p.After
}

// Actor is the principal recorded in audit events and created_by columns.
type Actor struct {
	Kind ownsandv1.PrincipalKind
	ID   string
}

// AuditEvent is one audit record.
type AuditEvent struct {
	OrganizationID string
	Action         ownsandv1.AuditAction
	Actor          Actor
	EndReason      ownsandv1.SandboxEndReason
	TargetKind     ownsandv1.AuditTargetKind
	TargetID       string
	RequestID      string
}

func (s *Store) audit(ctx context.Context, tx pgx.Tx, e AuditEvent) error {
	_, err := tx.Exec(ctx, `INSERT INTO audit_events
		(audit_event_id, organization_id, action, actor_kind, actor_id, end_reason, target_kind, target_id, request_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		NewID(), e.OrganizationID, int16(e.Action), int16(e.Actor.Kind), e.Actor.ID, int16(e.EndReason),
		int16(e.TargetKind), e.TargetID, e.RequestID, s.now())
	return err
}

// ControlEventKind identifies a cross-instance event.
type ControlEventKind int16

const (
	ControlEventUnspecified ControlEventKind = iota
	// An API key was revoked: end its sessions and relay grants.
	ControlEventAPIKeyRevoked
	// A membership ended: end the user's sessions and grants in the organization.
	ControlEventMembershipEnded
	// A sandbox delete was recorded: end its sessions and grants and send the
	// delete command to its connected device.
	ControlEventSandboxDeleteRequested
	// Device limits changed: send the new configuration.
	ControlEventDeviceLimitsChanged
	// The organization's agent version policy changed: send new targets.
	ControlEventAgentPolicyChanged
	// A device was removed: send clean-up and close its channel.
	ControlEventDeviceRemoved
)

// ControlEvent is one row of control_events.
type ControlEvent struct {
	ID             int64
	Kind           ControlEventKind
	OrganizationID string
	APIKeyID       string
	UserID         string
	SandboxID      string
	DeviceID       string
	CreatedAt      time.Time
}

func (s *Store) controlEvent(ctx context.Context, tx pgx.Tx, e ControlEvent) error {
	_, err := tx.Exec(ctx, `INSERT INTO control_events (kind, organization_id, api_key_id, user_id, sandbox_id, device_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		int16(e.Kind), e.OrganizationID, nullString(e.APIKeyID), nullString(e.UserID), nullString(e.SandboxID), nullString(e.DeviceID), s.now())
	return err
}

// ControlEventsAfter returns control events with an ID above after, in order.
func (s *Store) ControlEventsAfter(ctx context.Context, after int64, limit int) ([]ControlEvent, error) {
	rows, err := s.Pool.Query(ctx, `SELECT event_id, kind, organization_id, coalesce(api_key_id::text, ''), coalesce(user_id, ''),
		coalesce(sandbox_id::text, ''), coalesce(device_id::text, ''), created_at
		FROM control_events WHERE event_id > $1 ORDER BY event_id LIMIT $2`, after, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (ControlEvent, error) {
		var e ControlEvent
		var kind int16
		err := r.Scan(&e.ID, &kind, &e.OrganizationID, &e.APIKeyID, &e.UserID, &e.SandboxID, &e.DeviceID, &e.CreatedAt)
		e.Kind = ControlEventKind(kind)
		return e, err
	})
}

// LatestControlEventID returns the highest control event ID (0 when none).
func (s *Store) LatestControlEventID(ctx context.Context) (int64, error) {
	var id int64
	err := s.Pool.QueryRow(ctx, `SELECT coalesce(max(event_id), 0) FROM control_events`).Scan(&id)
	return id, err
}

func nullString(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}
