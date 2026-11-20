// SPDX-License-Identifier: FSL-1.1-ALv2

// Package api implements the Connect handlers of the OwnSand public services.
package api

import (
	"context"
	"errors"
	"log/slog"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/accounts"
	"github.com/delinoio/mono/servers/ownsand-api/internal/auth"
	"github.com/delinoio/mono/servers/ownsand-api/internal/authz"
	"github.com/delinoio/mono/servers/ownsand-api/internal/store"
)

// Server holds the dependencies of the public service handlers.
type Server struct {
	Store        *store.Store
	Logger       *slog.Logger
	DisplayNames accounts.DisplayNameSource
	Now          func() time.Time
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Limits of free-form names.
const maxNameBytes = 200

var errInternal = connect.NewError(connect.CodeInternal, errors.New("internal error"))

// principal returns the authenticated principal; the interceptor guarantees
// one for every procedure that reaches a public handler.
func principal(ctx context.Context) (auth.Principal, error) {
	p, ok := auth.FromContext(ctx)
	if !ok {
		return auth.Principal{}, auth.ErrMissingCredential
	}
	return p, nil
}

// authorize loads the caller's membership (for a user) and applies the
// authorization matrix for the organization.
func (s *Server) authorize(ctx context.Context, organizationID string, action authz.Action) (auth.Principal, *authz.Membership, error) {
	p, err := principal(ctx)
	if err != nil {
		return p, nil, err
	}
	var membership *authz.Membership
	if p.Kind == ownsandv1.PrincipalKind_PRINCIPAL_KIND_USER {
		m, err := s.Store.GetMembership(ctx, organizationID, p.ID)
		switch {
		case errors.Is(err, store.ErrNotFound):
		case err != nil:
			return p, nil, s.internal(ctx, "load_membership", err)
		default:
			membership = &authz.Membership{MembershipID: m.ID, Role: m.Role}
		}
	}
	if p.Kind == ownsandv1.PrincipalKind_PRINCIPAL_KIND_API_KEY && p.OrganizationID == organizationID {
		// The key's organization was live when the key was validated.
		if live, err := s.Store.OrganizationLive(ctx, organizationID); err != nil {
			return p, nil, s.internal(ctx, "load_organization", err)
		} else if !live {
			return p, nil, authz.ErrNotFound
		}
	}
	return p, membership, authz.Decide(p, organizationID, membership, action)
}

// internal logs a storage failure without request values and returns an
// internal error.
func (s *Server) internal(ctx context.Context, op string, err error) error {
	if errors.Is(err, context.Canceled) {
		return connect.NewError(connect.CodeCanceled, err)
	}
	s.Logger.ErrorContext(ctx, "storage_error", slog.String("operation", op), slog.Any("error", err))
	return errInternal
}

func actorOf(p auth.Principal) store.Actor { return store.Actor{Kind: p.Kind, ID: p.ID} }

func requestID() string { return store.NewID() }

func invalid(msg string) error {
	return connect.NewError(connect.CodeInvalidArgument, errors.New(msg))
}

func notFound(what string) error {
	return connect.NewError(connect.CodeNotFound, errors.New(what+" not found"))
}

func validName(field, v string, required bool) error {
	if required && v == "" {
		return invalid(field + " is required")
	}
	if len(v) > maxNameBytes || !utf8.ValidString(v) {
		return invalid(field + " must be valid UTF-8 of at most 200 bytes")
	}
	return nil
}

func page(size int32, token string) (store.Page, error) {
	p, err := store.ParsePage(size, token)
	if err != nil {
		return p, invalid(err.Error())
	}
	return p, nil
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

func principalProto(a store.Actor) *ownsandv1.Principal {
	return &ownsandv1.Principal{Kind: a.Kind, Id: a.ID}
}
