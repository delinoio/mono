// SPDX-License-Identifier: Apache-2.0

package ownsand

import (
	"context"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// fillOrganization sets the organization_id field of req from the client
// default when the request leaves it empty. A request type with the field
// fails with ErrNoOrganization when no organization is known.
func (c *Client) fillOrganization(req proto.Message) error {
	m := req.ProtoReflect()
	fd := m.Descriptor().Fields().ByName("organization_id")
	if fd == nil || fd.Kind() != protoreflect.StringKind {
		return nil
	}
	org, err := c.organization(m.Get(fd).String())
	if err != nil {
		return err
	}
	m.Set(fd, protoreflect.ValueOfString(org))
	return nil
}

// unary sends one unary RPC with the organization filled in.
func unary[Req, Res any, PReq interface {
	*Req
	proto.Message
}](ctx context.Context, c *Client, fn func(context.Context, *connect.Request[Req]) (*connect.Response[Res], error), req PReq) (*Res, error) {
	if req == nil {
		req = PReq(new(Req))
	}
	if err := c.fillOrganization(req); err != nil {
		return nil, err
	}
	res, err := fn(ctx, connect.NewRequest((*Req)(req)))
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

// unaryNoOrg sends one unary RPC that takes no organization.
func unaryNoOrg[Req, Res any](ctx context.Context, fn func(context.Context, *connect.Request[Req]) (*connect.Response[Res], error), req *Req) (*Res, error) {
	if req == nil {
		req = new(Req)
	}
	res, err := fn(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

// Organizations returns the OrganizationService wrapper.
func (c *Client) Organizations() *OrganizationService { return &OrganizationService{c} }

// Members returns the MemberService wrapper.
func (c *Client) Members() *MemberService { return &MemberService{c} }

// Devices returns the DeviceService wrapper.
func (c *Client) Devices() *DeviceService { return &DeviceService{c} }

// APIKeys returns the ApiKeyService wrapper.
func (c *Client) APIKeys() *APIKeyService { return &APIKeyService{c} }

// Sandboxes returns the SandboxService wrapper. Prefer CreateSandbox and
// the Sandbox handle for the sandbox lifecycle and the data path.
func (c *Client) Sandboxes() *SandboxService { return &SandboxService{c} }

// Snapshots returns the SnapshotService wrapper, including templates.
func (c *Client) Snapshots() *SnapshotService { return &SnapshotService{c} }

// DataPath returns the DataPathService wrapper. The sandbox handle calls it
// itself; programs need it only for custom data path handling.
func (c *Client) DataPath() *DataPathService { return &DataPathService{c} }

// OrganizationService wraps delino.ownsand.v1.OrganizationService.
type OrganizationService struct{ c *Client }

// Create creates an organization; the caller becomes its owner.
func (s *OrganizationService) Create(ctx context.Context, req *ownsandv1.CreateOrganizationRequest) (*ownsandv1.CreateOrganizationResponse, error) {
	return unaryNoOrg(ctx, s.c.organizations.CreateOrganization, req)
}

// Get reads one organization.
func (s *OrganizationService) Get(ctx context.Context, req *ownsandv1.GetOrganizationRequest) (*ownsandv1.GetOrganizationResponse, error) {
	return unary(ctx, s.c, s.c.organizations.GetOrganization, req)
}

// List lists the organizations of the signed-in user.
func (s *OrganizationService) List(ctx context.Context, req *ownsandv1.ListOrganizationsRequest) (*ownsandv1.ListOrganizationsResponse, error) {
	return unaryNoOrg(ctx, s.c.organizations.ListOrganizations, req)
}

// Delete deletes the organization.
func (s *OrganizationService) Delete(ctx context.Context, req *ownsandv1.DeleteOrganizationRequest) (*ownsandv1.DeleteOrganizationResponse, error) {
	return unary(ctx, s.c, s.c.organizations.DeleteOrganization, req)
}

// TransferOwnership makes another member the owner.
func (s *OrganizationService) TransferOwnership(ctx context.Context, req *ownsandv1.TransferOwnershipRequest) (*ownsandv1.TransferOwnershipResponse, error) {
	return unary(ctx, s.c, s.c.organizations.TransferOwnership, req)
}

// MemberService wraps delino.ownsand.v1.MemberService.
type MemberService struct{ c *Client }

// CreateInvitation creates a one-time invitation and returns its secret once.
func (s *MemberService) CreateInvitation(ctx context.Context, req *ownsandv1.CreateInvitationRequest) (*ownsandv1.CreateInvitationResponse, error) {
	return unary(ctx, s.c, s.c.members.CreateInvitation, req)
}

// ListInvitations lists pending invitations.
func (s *MemberService) ListInvitations(ctx context.Context, req *ownsandv1.ListInvitationsRequest) (*ownsandv1.ListInvitationsResponse, error) {
	return unary(ctx, s.c, s.c.members.ListInvitations, req)
}

// RevokeInvitation revokes a pending invitation.
func (s *MemberService) RevokeInvitation(ctx context.Context, req *ownsandv1.RevokeInvitationRequest) (*ownsandv1.RevokeInvitationResponse, error) {
	return unary(ctx, s.c, s.c.members.RevokeInvitation, req)
}

// AcceptInvitation joins the organization of an invitation secret.
func (s *MemberService) AcceptInvitation(ctx context.Context, req *ownsandv1.AcceptInvitationRequest) (*ownsandv1.AcceptInvitationResponse, error) {
	return unaryNoOrg(ctx, s.c.members.AcceptInvitation, req)
}

// List lists members.
func (s *MemberService) List(ctx context.Context, req *ownsandv1.ListMembersRequest) (*ownsandv1.ListMembersResponse, error) {
	return unary(ctx, s.c, s.c.members.ListMembers, req)
}

// UpdateRole changes a member role.
func (s *MemberService) UpdateRole(ctx context.Context, req *ownsandv1.UpdateMemberRoleRequest) (*ownsandv1.UpdateMemberRoleResponse, error) {
	return unary(ctx, s.c, s.c.members.UpdateMemberRole, req)
}

// Remove removes a member.
func (s *MemberService) Remove(ctx context.Context, req *ownsandv1.RemoveMemberRequest) (*ownsandv1.RemoveMemberResponse, error) {
	return unary(ctx, s.c, s.c.members.RemoveMember, req)
}

// DeviceService wraps delino.ownsand.v1.DeviceService.
type DeviceService struct{ c *Client }

// CreateEnrollmentToken creates a one-time enrollment token.
func (s *DeviceService) CreateEnrollmentToken(ctx context.Context, req *ownsandv1.CreateEnrollmentTokenRequest) (*ownsandv1.CreateEnrollmentTokenResponse, error) {
	return unary(ctx, s.c, s.c.devices.CreateEnrollmentToken, req)
}

// ListEnrollmentTokens lists enrollment tokens.
func (s *DeviceService) ListEnrollmentTokens(ctx context.Context, req *ownsandv1.ListEnrollmentTokensRequest) (*ownsandv1.ListEnrollmentTokensResponse, error) {
	return unary(ctx, s.c, s.c.devices.ListEnrollmentTokens, req)
}

// RevokeEnrollmentToken revokes an unused enrollment token.
func (s *DeviceService) RevokeEnrollmentToken(ctx context.Context, req *ownsandv1.RevokeEnrollmentTokenRequest) (*ownsandv1.RevokeEnrollmentTokenResponse, error) {
	return unary(ctx, s.c, s.c.devices.RevokeEnrollmentToken, req)
}

// List lists devices.
func (s *DeviceService) List(ctx context.Context, req *ownsandv1.ListDevicesRequest) (*ownsandv1.ListDevicesResponse, error) {
	return unary(ctx, s.c, s.c.devices.ListDevices, req)
}

// Get reads one device.
func (s *DeviceService) Get(ctx context.Context, req *ownsandv1.GetDeviceRequest) (*ownsandv1.GetDeviceResponse, error) {
	return unary(ctx, s.c, s.c.devices.GetDevice, req)
}

// ReplaceLimits replaces all four device limits. Every value must be present;
// the server rejects a missing value with invalid_argument, and an explicit
// zero is stored. New code uses this method.
func (s *DeviceService) ReplaceLimits(ctx context.Context, req *ownsandv1.ReplaceDeviceLimitsRequest) (*ownsandv1.ReplaceDeviceLimitsResponse, error) {
	return unary(ctx, s.c, s.c.devices.ReplaceDeviceLimits, req)
}

// UpdateLimits sets all four device limits with implicit field presence: a
// missing value reads as zero. Legacy, kept for v1 clients; use
// ReplaceLimits.
func (s *DeviceService) UpdateLimits(ctx context.Context, req *ownsandv1.UpdateDeviceLimitsRequest) (*ownsandv1.UpdateDeviceLimitsResponse, error) {
	return unary(ctx, s.c, s.c.devices.UpdateDeviceLimits, req)
}

// Remove revokes the device credentials and requests local clean-up.
func (s *DeviceService) Remove(ctx context.Context, req *ownsandv1.RemoveDeviceRequest) (*ownsandv1.RemoveDeviceResponse, error) {
	return unary(ctx, s.c, s.c.devices.RemoveDevice, req)
}

// GetAgentVersionPolicy reads the agent version policy.
func (s *DeviceService) GetAgentVersionPolicy(ctx context.Context, req *ownsandv1.GetAgentVersionPolicyRequest) (*ownsandv1.GetAgentVersionPolicyResponse, error) {
	return unary(ctx, s.c, s.c.devices.GetAgentVersionPolicy, req)
}

// UpdateAgentVersionPolicy follows the default, pins a version or holds updates.
func (s *DeviceService) UpdateAgentVersionPolicy(ctx context.Context, req *ownsandv1.UpdateAgentVersionPolicyRequest) (*ownsandv1.UpdateAgentVersionPolicyResponse, error) {
	return unary(ctx, s.c, s.c.devices.UpdateAgentVersionPolicy, req)
}

// APIKeyService wraps delino.ownsand.v1.ApiKeyService.
type APIKeyService struct{ c *Client }

// Create creates an API key and returns its secret once.
func (s *APIKeyService) Create(ctx context.Context, req *ownsandv1.CreateApiKeyRequest) (*ownsandv1.CreateApiKeyResponse, error) {
	return unary(ctx, s.c, s.c.apiKeys.CreateApiKey, req)
}

// List lists API keys.
func (s *APIKeyService) List(ctx context.Context, req *ownsandv1.ListApiKeysRequest) (*ownsandv1.ListApiKeysResponse, error) {
	return unary(ctx, s.c, s.c.apiKeys.ListApiKeys, req)
}

// Revoke revokes an API key.
func (s *APIKeyService) Revoke(ctx context.Context, req *ownsandv1.RevokeApiKeyRequest) (*ownsandv1.RevokeApiKeyResponse, error) {
	return unary(ctx, s.c, s.c.apiKeys.RevokeApiKey, req)
}

// SandboxService wraps delino.ownsand.v1.SandboxService.
type SandboxService struct{ c *Client }

// Create sends one CreateSandbox request as given, without retries. Prefer
// Client.CreateSandbox, which generates the idempotency key and retries.
func (s *SandboxService) Create(ctx context.Context, req *ownsandv1.CreateSandboxRequest) (*ownsandv1.CreateSandboxResponse, error) {
	return unary(ctx, s.c, s.c.sandboxes.CreateSandbox, req)
}

// Get reads one sandbox.
func (s *SandboxService) Get(ctx context.Context, req *ownsandv1.GetSandboxRequest) (*ownsandv1.GetSandboxResponse, error) {
	return unary(ctx, s.c, s.c.sandboxes.GetSandbox, req)
}

// List lists sandboxes.
func (s *SandboxService) List(ctx context.Context, req *ownsandv1.ListSandboxesRequest) (*ownsandv1.ListSandboxesResponse, error) {
	return unary(ctx, s.c, s.c.sandboxes.ListSandboxes, req)
}

// Delete deletes a sandbox.
func (s *SandboxService) Delete(ctx context.Context, req *ownsandv1.DeleteSandboxRequest) (*ownsandv1.DeleteSandboxResponse, error) {
	return unary(ctx, s.c, s.c.sandboxes.DeleteSandbox, req)
}

// Pause pauses a running sandbox.
func (s *SandboxService) Pause(ctx context.Context, req *ownsandv1.PauseSandboxRequest) (*ownsandv1.PauseSandboxResponse, error) {
	return unary(ctx, s.c, s.c.sandboxes.PauseSandbox, req)
}

// Resume resumes a paused sandbox.
func (s *SandboxService) Resume(ctx context.Context, req *ownsandv1.ResumeSandboxRequest) (*ownsandv1.ResumeSandboxResponse, error) {
	return unary(ctx, s.c, s.c.sandboxes.ResumeSandbox, req)
}

// SnapshotService wraps delino.ownsand.v1.SnapshotService.
type SnapshotService struct{ c *Client }

// Create takes a snapshot of a sandbox.
func (s *SnapshotService) Create(ctx context.Context, req *ownsandv1.CreateSnapshotRequest) (*ownsandv1.CreateSnapshotResponse, error) {
	return unary(ctx, s.c, s.c.snapshots.CreateSnapshot, req)
}

// Get reads one snapshot.
func (s *SnapshotService) Get(ctx context.Context, req *ownsandv1.GetSnapshotRequest) (*ownsandv1.GetSnapshotResponse, error) {
	return unary(ctx, s.c, s.c.snapshots.GetSnapshot, req)
}

// List lists snapshots.
func (s *SnapshotService) List(ctx context.Context, req *ownsandv1.ListSnapshotsRequest) (*ownsandv1.ListSnapshotsResponse, error) {
	return unary(ctx, s.c, s.c.snapshots.ListSnapshots, req)
}

// Delete deletes a snapshot.
func (s *SnapshotService) Delete(ctx context.Context, req *ownsandv1.DeleteSnapshotRequest) (*ownsandv1.DeleteSnapshotResponse, error) {
	return unary(ctx, s.c, s.c.snapshots.DeleteSnapshot, req)
}

// Export exports a snapshot to R2.
func (s *SnapshotService) Export(ctx context.Context, req *ownsandv1.ExportSnapshotRequest) (*ownsandv1.ExportSnapshotResponse, error) {
	return unary(ctx, s.c, s.c.snapshots.ExportSnapshot, req)
}

// CreateTemplate makes a named template from a snapshot.
func (s *SnapshotService) CreateTemplate(ctx context.Context, req *ownsandv1.CreateTemplateRequest) (*ownsandv1.CreateTemplateResponse, error) {
	return unary(ctx, s.c, s.c.snapshots.CreateTemplate, req)
}

// ListTemplates lists templates.
func (s *SnapshotService) ListTemplates(ctx context.Context, req *ownsandv1.ListTemplatesRequest) (*ownsandv1.ListTemplatesResponse, error) {
	return unary(ctx, s.c, s.c.snapshots.ListTemplates, req)
}

// DeleteTemplate deletes a template.
func (s *SnapshotService) DeleteTemplate(ctx context.Context, req *ownsandv1.DeleteTemplateRequest) (*ownsandv1.DeleteTemplateResponse, error) {
	return unary(ctx, s.c, s.c.snapshots.DeleteTemplate, req)
}

// DataPathService wraps delino.ownsand.v1.DataPathService.
type DataPathService struct{ c *Client }

// GetIceServers returns the STUN server URLs.
func (s *DataPathService) GetIceServers(ctx context.Context, req *ownsandv1.GetIceServersRequest) (*ownsandv1.GetIceServersResponse, error) {
	return unary(ctx, s.c, s.c.dataPath.GetIceServers, req)
}

// OpenSession signals a peer-to-peer data channel.
func (s *DataPathService) OpenSession(ctx context.Context, req *ownsandv1.OpenSessionRequest) (*ownsandv1.OpenSessionResponse, error) {
	return unary(ctx, s.c, s.c.dataPath.OpenSession, req)
}

// AuthorizeRelay grants relay access to one operation.
func (s *DataPathService) AuthorizeRelay(ctx context.Context, req *ownsandv1.AuthorizeRelayRequest) (*ownsandv1.AuthorizeRelayResponse, error) {
	return unary(ctx, s.c, s.c.dataPath.AuthorizeRelay, req)
}
