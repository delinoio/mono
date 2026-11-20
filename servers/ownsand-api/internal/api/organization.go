// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
	"github.com/delinoio/mono/servers/ownsand-api/internal/authz"
	"github.com/delinoio/mono/servers/ownsand-api/internal/secret"
	"github.com/delinoio/mono/servers/ownsand-api/internal/store"
)

// InvitationLifetime is the expiry of an invitation.
const InvitationLifetime = 7 * 24 * time.Hour

// OrganizationService implements ownsandv1connect.OrganizationServiceHandler.
type OrganizationService struct{ *Server }

var _ ownsandv1connect.OrganizationServiceHandler = OrganizationService{}

func orgProto(o store.Organization) *ownsandv1.Organization {
	return &ownsandv1.Organization{OrganizationId: o.ID, Name: o.Name, CreateTime: ts(o.CreatedAt), CallerRole: o.CallerRole}
}

func (s OrganizationService) CreateOrganization(ctx context.Context, req *connect.Request[ownsandv1.CreateOrganizationRequest]) (*connect.Response[ownsandv1.CreateOrganizationResponse], error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	if err := validName("name", req.Msg.GetName(), true); err != nil {
		return nil, err
	}
	org, err := s.Store.CreateOrganization(ctx, req.Msg.GetName(), p.ID, requestID())
	if err != nil {
		return nil, s.internal(ctx, "create_organization", err)
	}
	return connect.NewResponse(&ownsandv1.CreateOrganizationResponse{Organization: orgProto(org)}), nil
}

func (s OrganizationService) GetOrganization(ctx context.Context, req *connect.Request[ownsandv1.GetOrganizationRequest]) (*connect.Response[ownsandv1.GetOrganizationResponse], error) {
	_, m, err := s.authorize(ctx, req.Msg.GetOrganizationId(), authz.ActionReadOrganization)
	if err != nil {
		return nil, err
	}
	org, err := s.Store.GetOrganization(ctx, req.Msg.GetOrganizationId())
	if errors.Is(err, store.ErrNotFound) {
		return nil, authz.ErrNotFound
	}
	if err != nil {
		return nil, s.internal(ctx, "get_organization", err)
	}
	if m != nil {
		org.CallerRole = m.Role
	}
	return connect.NewResponse(&ownsandv1.GetOrganizationResponse{Organization: orgProto(org)}), nil
}

func (s OrganizationService) ListOrganizations(ctx context.Context, req *connect.Request[ownsandv1.ListOrganizationsRequest]) (*connect.Response[ownsandv1.ListOrganizationsResponse], error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	pg, err := page(req.Msg.GetPageSize(), req.Msg.GetPageToken())
	if err != nil {
		return nil, err
	}
	orgs, err := s.Store.ListOrganizations(ctx, p.ID, pg)
	if err != nil {
		return nil, s.internal(ctx, "list_organizations", err)
	}
	resp := &ownsandv1.ListOrganizationsResponse{}
	ids := make([]string, 0, len(orgs))
	for _, o := range orgs {
		resp.Organizations = append(resp.Organizations, orgProto(o))
		ids = append(ids, o.ID)
	}
	resp.NextPageToken = store.NextToken(ids, pg.Size)
	return connect.NewResponse(resp), nil
}

func (s OrganizationService) DeleteOrganization(ctx context.Context, req *connect.Request[ownsandv1.DeleteOrganizationRequest]) (*connect.Response[ownsandv1.DeleteOrganizationResponse], error) {
	p, _, err := s.authorize(ctx, req.Msg.GetOrganizationId(), authz.ActionDeleteOrganization)
	if err != nil {
		return nil, err
	}
	err = s.Store.DeleteOrganization(ctx, req.Msg.GetOrganizationId(), actorOf(p), requestID())
	if errors.Is(err, store.ErrNotFound) {
		return nil, authz.ErrNotFound
	}
	if err != nil {
		return nil, s.internal(ctx, "delete_organization", err)
	}
	s.Logger.InfoContext(ctx, "organization_deleted", slog.String("organization_id", req.Msg.GetOrganizationId()))
	return connect.NewResponse(&ownsandv1.DeleteOrganizationResponse{}), nil
}

func (s OrganizationService) TransferOwnership(ctx context.Context, req *connect.Request[ownsandv1.TransferOwnershipRequest]) (*connect.Response[ownsandv1.TransferOwnershipResponse], error) {
	p, _, err := s.authorize(ctx, req.Msg.GetOrganizationId(), authz.ActionTransferOwnership)
	if err != nil {
		return nil, err
	}
	if req.Msg.GetNewOwnerUserId() == "" {
		return nil, invalid("new_owner_user_id is required")
	}
	err = s.Store.TransferOwnership(ctx, req.Msg.GetOrganizationId(), actorOf(p), req.Msg.GetNewOwnerUserId(), requestID())
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFound("member")
	}
	if err != nil {
		return nil, s.internal(ctx, "transfer_ownership", err)
	}
	return connect.NewResponse(&ownsandv1.TransferOwnershipResponse{}), nil
}

// MemberService implements ownsandv1connect.MemberServiceHandler.
type MemberService struct{ *Server }

var _ ownsandv1connect.MemberServiceHandler = MemberService{}

func (s MemberService) invitationProto(i store.Invitation) *ownsandv1.Invitation {
	return &ownsandv1.Invitation{
		OrganizationId: i.OrganizationID, InvitationId: i.ID, Role: i.Role, State: i.State(s.now()),
		CreateTime: ts(i.CreatedAt), ExpireTime: ts(i.ExpiresAt), CreatedBy: principalProto(i.CreatedBy), SecretPrefix: i.SecretPrefix,
	}
}

func assignableRole(r ownsandv1.Role) bool {
	return r == ownsandv1.Role_ROLE_ADMIN || r == ownsandv1.Role_ROLE_MEMBER
}

func (s MemberService) CreateInvitation(ctx context.Context, req *connect.Request[ownsandv1.CreateInvitationRequest]) (*connect.Response[ownsandv1.CreateInvitationResponse], error) {
	p, _, err := s.authorize(ctx, req.Msg.GetOrganizationId(), authz.ActionManageMembers)
	if err != nil {
		return nil, err
	}
	if !assignableRole(req.Msg.GetRole()) {
		return nil, invalid("role must be ROLE_ADMIN or ROLE_MEMBER")
	}
	value := secret.New(secret.KindInvitation)
	inv, err := s.Store.CreateInvitation(ctx, req.Msg.GetOrganizationId(), req.Msg.GetRole(), actorOf(p), secret.Hash(value), secret.DisplayPrefix(value), InvitationLifetime, requestID())
	if err != nil {
		return nil, s.internal(ctx, "create_invitation", err)
	}
	return connect.NewResponse(&ownsandv1.CreateInvitationResponse{Invitation: s.invitationProto(inv), InvitationSecret: value}), nil
}

func (s MemberService) ListInvitations(ctx context.Context, req *connect.Request[ownsandv1.ListInvitationsRequest]) (*connect.Response[ownsandv1.ListInvitationsResponse], error) {
	if _, _, err := s.authorize(ctx, req.Msg.GetOrganizationId(), authz.ActionManageMembers); err != nil {
		return nil, err
	}
	pg, err := page(req.Msg.GetPageSize(), req.Msg.GetPageToken())
	if err != nil {
		return nil, err
	}
	list, err := s.Store.ListInvitations(ctx, req.Msg.GetOrganizationId(), pg)
	if err != nil {
		return nil, s.internal(ctx, "list_invitations", err)
	}
	resp := &ownsandv1.ListInvitationsResponse{}
	var ids []string
	for _, i := range list {
		resp.Invitations = append(resp.Invitations, s.invitationProto(i))
		ids = append(ids, i.ID)
	}
	resp.NextPageToken = store.NextToken(ids, pg.Size)
	return connect.NewResponse(resp), nil
}

func (s MemberService) RevokeInvitation(ctx context.Context, req *connect.Request[ownsandv1.RevokeInvitationRequest]) (*connect.Response[ownsandv1.RevokeInvitationResponse], error) {
	p, _, err := s.authorize(ctx, req.Msg.GetOrganizationId(), authz.ActionManageMembers)
	if err != nil {
		return nil, err
	}
	err = s.Store.RevokeInvitation(ctx, req.Msg.GetOrganizationId(), req.Msg.GetInvitationId(), actorOf(p), requestID())
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, notFound("invitation")
	case errors.Is(err, store.ErrConflict):
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("the invitation is no longer pending"))
	case err != nil:
		return nil, s.internal(ctx, "revoke_invitation", err)
	}
	return connect.NewResponse(&ownsandv1.RevokeInvitationResponse{}), nil
}

// errInvitationInvalid is the single error of an unknown, expired, revoked or
// used invitation.
var errInvitationInvalid = connect.NewError(connect.CodeNotFound, errors.New("invitation not found or no longer valid"))

func (s MemberService) AcceptInvitation(ctx context.Context, req *connect.Request[ownsandv1.AcceptInvitationRequest]) (*connect.Response[ownsandv1.AcceptInvitationResponse], error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	value := req.Msg.GetInvitationSecret()
	limitKey := "invitation:" + secret.DisplayPrefix(value)
	if s.InvitationFailures != nil && !s.InvitationFailures.Peek(limitKey) {
		return nil, connect.NewError(connect.CodeResourceExhausted, errors.New("too many failed invitation attempts; retry later"))
	}
	fail := func() error {
		if s.InvitationFailures != nil {
			s.InvitationFailures.Allow(limitKey)
		}
		return errInvitationInvalid
	}
	if secret.Classify(value) != secret.KindInvitation {
		return nil, fail()
	}
	org, m, err := s.Store.AcceptInvitation(ctx, secret.Hash(value), p.ID, requestID())
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, fail()
	case errors.Is(err, store.ErrAlreadyExists):
		return nil, connect.NewError(connect.CodeAlreadyExists, errors.New("the caller is already a member of the organization"))
	case err != nil:
		return nil, s.internal(ctx, "accept_invitation", err)
	}
	return connect.NewResponse(&ownsandv1.AcceptInvitationResponse{
		Organization: orgProto(org),
		Member:       &ownsandv1.Member{OrganizationId: m.OrganizationID, MembershipId: m.ID, UserId: m.UserID, Role: m.Role, JoinTime: ts(m.CreatedAt)},
	}), nil
}

func (s MemberService) ListMembers(ctx context.Context, req *connect.Request[ownsandv1.ListMembersRequest]) (*connect.Response[ownsandv1.ListMembersResponse], error) {
	if _, _, err := s.authorize(ctx, req.Msg.GetOrganizationId(), authz.ActionReadOrganization); err != nil {
		return nil, err
	}
	pg, err := page(req.Msg.GetPageSize(), req.Msg.GetPageToken())
	if err != nil {
		return nil, err
	}
	list, err := s.Store.ListMembers(ctx, req.Msg.GetOrganizationId(), pg)
	if err != nil {
		return nil, s.internal(ctx, "list_members", err)
	}
	userIDs := make([]string, 0, len(list))
	for _, m := range list {
		userIDs = append(userIDs, m.UserID)
	}
	// The membership check above happens before the lookup, and only users of
	// this page are asked for. A failure leaves the names empty.
	names := map[string]string{}
	if s.DisplayNames != nil && len(userIDs) > 0 {
		lookupCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		got, err := s.DisplayNames.DisplayNames(lookupCtx, userIDs)
		cancel()
		if err != nil {
			s.Logger.DebugContext(ctx, "display_names_unavailable", slog.Any("error", err))
		} else {
			names = got
		}
	}
	resp := &ownsandv1.ListMembersResponse{}
	var ids []string
	for _, m := range list {
		resp.Members = append(resp.Members, &ownsandv1.Member{
			OrganizationId: m.OrganizationID, MembershipId: m.ID, UserId: m.UserID, Role: m.Role,
			DisplayName: names[m.UserID], JoinTime: ts(m.CreatedAt),
		})
		ids = append(ids, m.ID)
	}
	resp.NextPageToken = store.NextToken(ids, pg.Size)
	return connect.NewResponse(resp), nil
}

func (s MemberService) UpdateMemberRole(ctx context.Context, req *connect.Request[ownsandv1.UpdateMemberRoleRequest]) (*connect.Response[ownsandv1.UpdateMemberRoleResponse], error) {
	p, _, err := s.authorize(ctx, req.Msg.GetOrganizationId(), authz.ActionManageMembers)
	if err != nil {
		return nil, err
	}
	if !assignableRole(req.Msg.GetRole()) {
		return nil, invalid("role must be ROLE_ADMIN or ROLE_MEMBER; use TransferOwnership to change the owner")
	}
	m, err := s.Store.UpdateMemberRole(ctx, req.Msg.GetOrganizationId(), req.Msg.GetUserId(), req.Msg.GetRole(), actorOf(p), requestID())
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, notFound("member")
	case errors.Is(err, store.ErrForbidden):
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("the owner's role changes only with TransferOwnership"))
	case err != nil:
		return nil, s.internal(ctx, "update_member_role", err)
	}
	return connect.NewResponse(&ownsandv1.UpdateMemberRoleResponse{Member: &ownsandv1.Member{
		OrganizationId: m.OrganizationID, MembershipId: m.ID, UserId: m.UserID, Role: m.Role, JoinTime: ts(m.CreatedAt),
	}}), nil
}

func (s MemberService) RemoveMember(ctx context.Context, req *connect.Request[ownsandv1.RemoveMemberRequest]) (*connect.Response[ownsandv1.RemoveMemberResponse], error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	action := authz.ActionManageMembers
	if p.Kind == ownsandv1.PrincipalKind_PRINCIPAL_KIND_USER && req.Msg.GetUserId() == p.ID {
		action = authz.ActionLeaveOrganization
	}
	p, _, err = s.authorize(ctx, req.Msg.GetOrganizationId(), action)
	if err != nil {
		return nil, err
	}
	err = s.Store.RemoveMember(ctx, req.Msg.GetOrganizationId(), req.Msg.GetUserId(), actorOf(p), requestID())
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, notFound("member")
	case errors.Is(err, store.ErrForbidden):
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("the owner cannot be removed"))
	case err != nil:
		return nil, s.internal(ctx, "remove_member", err)
	}
	return connect.NewResponse(&ownsandv1.RemoveMemberResponse{}), nil
}

// ApiKeyService implements ownsandv1connect.ApiKeyServiceHandler.
type ApiKeyService struct{ *Server }

var _ ownsandv1connect.ApiKeyServiceHandler = ApiKeyService{}

func apiKeyProto(k store.APIKey) *ownsandv1.ApiKey {
	state := ownsandv1.ApiKeyState_API_KEY_STATE_ACTIVE
	if k.RevokedAt != nil {
		state = ownsandv1.ApiKeyState_API_KEY_STATE_REVOKED
	}
	return &ownsandv1.ApiKey{
		OrganizationId: k.OrganizationID, ApiKeyId: k.ID, Name: k.Name, State: state, SecretPrefix: k.SecretPrefix,
		CreateTime: ts(k.CreatedAt), CreatedBy: principalProto(k.CreatedBy), RevokeTime: tsPtr(k.RevokedAt),
	}
}

func (s ApiKeyService) CreateApiKey(ctx context.Context, req *connect.Request[ownsandv1.CreateApiKeyRequest]) (*connect.Response[ownsandv1.CreateApiKeyResponse], error) {
	p, _, err := s.authorize(ctx, req.Msg.GetOrganizationId(), authz.ActionManageAPIKeys)
	if err != nil {
		return nil, err
	}
	if err := validName("name", req.Msg.GetName(), true); err != nil {
		return nil, err
	}
	value := secret.New(secret.KindAPIKey)
	k, err := s.Store.CreateAPIKey(ctx, req.Msg.GetOrganizationId(), req.Msg.GetName(), actorOf(p), secret.Hash(value), secret.DisplayPrefix(value), requestID())
	if err != nil {
		return nil, s.internal(ctx, "create_api_key", err)
	}
	return connect.NewResponse(&ownsandv1.CreateApiKeyResponse{ApiKey: apiKeyProto(k), ApiKeySecret: value}), nil
}

func (s ApiKeyService) ListApiKeys(ctx context.Context, req *connect.Request[ownsandv1.ListApiKeysRequest]) (*connect.Response[ownsandv1.ListApiKeysResponse], error) {
	if _, _, err := s.authorize(ctx, req.Msg.GetOrganizationId(), authz.ActionManageAPIKeys); err != nil {
		return nil, err
	}
	pg, err := page(req.Msg.GetPageSize(), req.Msg.GetPageToken())
	if err != nil {
		return nil, err
	}
	list, err := s.Store.ListAPIKeys(ctx, req.Msg.GetOrganizationId(), pg)
	if err != nil {
		return nil, s.internal(ctx, "list_api_keys", err)
	}
	resp := &ownsandv1.ListApiKeysResponse{}
	var ids []string
	for _, k := range list {
		resp.ApiKeys = append(resp.ApiKeys, apiKeyProto(k))
		ids = append(ids, k.ID)
	}
	resp.NextPageToken = store.NextToken(ids, pg.Size)
	return connect.NewResponse(resp), nil
}

func (s ApiKeyService) RevokeApiKey(ctx context.Context, req *connect.Request[ownsandv1.RevokeApiKeyRequest]) (*connect.Response[ownsandv1.RevokeApiKeyResponse], error) {
	p, _, err := s.authorize(ctx, req.Msg.GetOrganizationId(), authz.ActionManageAPIKeys)
	if err != nil {
		return nil, err
	}
	err = s.Store.RevokeAPIKey(ctx, req.Msg.GetOrganizationId(), req.Msg.GetApiKeyId(), actorOf(p), requestID())
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFound("API key")
	}
	if err != nil {
		return nil, s.internal(ctx, "revoke_api_key", err)
	}
	return connect.NewResponse(&ownsandv1.RevokeApiKeyResponse{}), nil
}
