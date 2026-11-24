// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/delinoio/mono/servers/tide-api/internal/auth"
)

// Extension hooks of later layers. A layer registers its hooks with
// app.Registry.OnMembershipEnd and app.Registry.OnOrganizationDeletion while
// the app is wired. Hooks run in registration order.
//
// Each hook has an in-transaction step and an after-commit step:
//
//   - InTx runs in the transaction of the core change, under the locks that
//     the core took. It must do database work only (no network call) and
//     must record every durable consequence there, for example "end these
//     dev sessions" as a state change that a lease component completes. An
//     error rolls back the whole RPC.
//   - AfterCommit runs after the commit with a context that is not canceled
//     when the caller goes away. It may call external services. It is best
//     effort: a crash can skip it, so it must only speed up work that InTx
//     already recorded and that a lease component (registered with
//     app.Registry.AddLeaseComponent) finishes in any case.

// MembershipEndReason tells how a membership ended.
type MembershipEndReason int

const (
	MembershipEndUnspecified MembershipEndReason = iota
	// MembershipRemoved: an owner or admin removed another member.
	MembershipRemoved
	// MembershipLeft: the member removed their own membership.
	MembershipLeft
)

// MembershipEnd describes an ended membership (RemoveMember). When it is
// passed to the hooks, the membership row is deleted and the user's personal
// API keys of the organization are revoked in the same transaction.
// Organization deletion does not run these hooks; it runs the organization
// deletion hooks instead.
type MembershipEnd struct {
	OrganizationID string
	UserID         string
	MembershipID   string
	Reason         MembershipEndReason
	// Actor is the remover, or the user who left.
	Actor auth.Principal
	// RevokedAPIKeyIDs are the keys that the core revoked.
	RevokedAPIKeyIDs []string
}

// MembershipEndHook lets a layer act when a membership ends. The dev session
// layer uses it to end the user's dev sessions of the organization: InTx
// cancels the queued ones with MEMBER_REMOVED and marks the others for
// deletion, AfterCommit starts the sandbox deletions.
type MembershipEndHook struct {
	// Name is the stable hook name in log records.
	Name        string
	InTx        func(ctx context.Context, tx pgx.Tx, e MembershipEnd) error
	AfterCommit func(ctx context.Context, e MembershipEnd)
}

// OrganizationDeletion describes a DeleteOrganization call.
type OrganizationDeletion struct {
	OrganizationID string
	Actor          auth.Principal
}

// OrganizationDeletionHook prepares a layer for one deletion attempt.
// Prepare runs after the owner pre-check, before the deletion transaction.
// It may call external services. On failure it must undo its own work.
type OrganizationDeletionHook struct {
	// Name is the stable hook name in log records.
	Name string
	// Prepare is required.
	Prepare func(ctx context.Context, d OrganizationDeletion) (OrganizationDeletionSteps, error)
}

// OrganizationDeletionSteps hold the callbacks for one deletion attempt.
// They can capture state acquired during Prepare.
type OrganizationDeletionSteps struct {
	// InTx runs after the organization row lock and owner check, before
	// the core revokes the keys and deletes the rows.
	InTx func(ctx context.Context, tx pgx.Tx) error
	// AfterCommit runs after the organization is deleted.
	AfterCommit func(ctx context.Context)
	// Abort undoes successful preparation if a later hook fails or the
	// deletion transaction rolls back.
	Abort func(ctx context.Context)
}

type preparedOrganizationDeletionHook struct {
	name string
	OrganizationDeletionSteps
}

// Hooks holds the registered extension hooks.
type Hooks struct {
	MembershipEnd        []MembershipEndHook
	OrganizationDeletion []OrganizationDeletionHook
}

func (s *Service) membershipEndInTx(ctx context.Context, tx pgx.Tx, e MembershipEnd) error {
	for _, h := range s.Hooks.MembershipEnd {
		if h.InTx == nil {
			continue
		}
		if err := h.InTx(ctx, tx, e); err != nil {
			return fmt.Errorf("membership end hook %s: %w", h.Name, err)
		}
	}
	return nil
}

func (s *Service) membershipEndAfterCommit(ctx context.Context, e MembershipEnd) {
	ctx = context.WithoutCancel(ctx)
	for _, h := range s.Hooks.MembershipEnd {
		if h.AfterCommit != nil {
			h.AfterCommit(ctx, e)
		}
	}
	s.Logger.InfoContext(ctx, "membership_ended", slog.String("organization_id", e.OrganizationID),
		slog.String("user_id", e.UserID), slog.Int("revoked_api_keys", len(e.RevokedAPIKeyIDs)))
}

func (s *Service) organizationDeletionPrepare(ctx context.Context, d OrganizationDeletion) ([]preparedOrganizationDeletionHook, error) {
	prepared := make([]preparedOrganizationDeletionHook, 0, len(s.Hooks.OrganizationDeletion))
	for _, h := range s.Hooks.OrganizationDeletion {
		steps, err := h.Prepare(ctx, d)
		if err != nil {
			s.Logger.WarnContext(ctx, "organization_deletion_prepare_failed", slog.String("hook", h.Name),
				slog.String("organization_id", d.OrganizationID), slog.Any("error", err))
			organizationDeletionAbort(ctx, prepared)
			return nil, err
		}
		prepared = append(prepared, preparedOrganizationDeletionHook{name: h.Name, OrganizationDeletionSteps: steps})
	}
	return prepared, nil
}

func organizationDeletionAbort(ctx context.Context, prepared []preparedOrganizationDeletionHook) {
	ctx = context.WithoutCancel(ctx)
	for _, h := range prepared {
		if h.Abort != nil {
			h.Abort(ctx)
		}
	}
}

func organizationDeletionInTx(ctx context.Context, tx pgx.Tx, prepared []preparedOrganizationDeletionHook) error {
	for _, h := range prepared {
		if h.InTx == nil {
			continue
		}
		if err := h.InTx(ctx, tx); err != nil {
			return fmt.Errorf("organization deletion hook %s: %w", h.name, err)
		}
	}
	return nil
}

func (s *Service) organizationDeletionAfterCommit(ctx context.Context, d OrganizationDeletion, prepared []preparedOrganizationDeletionHook) {
	ctx = context.WithoutCancel(ctx)
	for _, h := range prepared {
		if h.AfterCommit != nil {
			h.AfterCommit(ctx)
		}
	}
	s.Logger.InfoContext(ctx, "organization_deleted", slog.String("organization_id", d.OrganizationID))
}
