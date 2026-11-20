package ownsand

import (
	"context"
	"time"

	"connectrpc.com/connect"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"google.golang.org/protobuf/types/known/durationpb"
)

// ListOptions page a list call.
type ListOptions struct {
	// PageSize is the page size (server default 50, maximum 200).
	PageSize int32
	// PageToken is the token of the page to read.
	PageToken string
}

func call[Req, Res any](c *Client, ctx context.Context, f func(context.Context, *connect.Request[Req]) (*connect.Response[Res], error), req *Req) (*Res, error) {
	resp, err := f(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, c.redact(err)
	}
	return resp.Msg, nil
}

// ---------------------------------------------------------------------------
// OrganizationService
// ---------------------------------------------------------------------------

// CreateOrganization creates an organization; the caller becomes its owner.
func (c *Client) CreateOrganization(ctx context.Context, name string) (*ownsandv1.Organization, error) {
	r, err := call(c, ctx, c.orgs.CreateOrganization, &ownsandv1.CreateOrganizationRequest{Name: name})
	return r.GetOrganization(), err
}

// GetOrganization reads the organization.
func (c *Client) GetOrganization(ctx context.Context, opts ...CallOption) (*ownsandv1.Organization, error) {
	org, err := c.orgFor(opts)
	if err != nil {
		return nil, err
	}
	r, err := call(c, ctx, c.orgs.GetOrganization, &ownsandv1.GetOrganizationRequest{OrganizationId: org})
	return r.GetOrganization(), err
}

// ListOrganizations lists the organizations of the signed-in user.
func (c *Client) ListOrganizations(ctx context.Context, lo ListOptions) ([]*ownsandv1.Organization, string, error) {
	r, err := call(c, ctx, c.orgs.ListOrganizations, &ownsandv1.ListOrganizationsRequest{PageSize: lo.PageSize, PageToken: lo.PageToken})
	return r.GetOrganizations(), r.GetNextPageToken(), err
}

// DeleteOrganization deletes the organization.
func (c *Client) DeleteOrganization(ctx context.Context, opts ...CallOption) error {
	org, err := c.orgFor(opts)
	if err != nil {
		return err
	}
	_, err = call(c, ctx, c.orgs.DeleteOrganization, &ownsandv1.DeleteOrganizationRequest{OrganizationId: org})
	return err
}

// TransferOwnership makes another member the owner.
func (c *Client) TransferOwnership(ctx context.Context, newOwnerUserID string, opts ...CallOption) error {
	org, err := c.orgFor(opts)
	if err != nil {
		return err
	}
	_, err = call(c, ctx, c.orgs.TransferOwnership, &ownsandv1.TransferOwnershipRequest{OrganizationId: org, NewOwnerUserId: newOwnerUserID})
	return err
}

// ---------------------------------------------------------------------------
// MemberService
// ---------------------------------------------------------------------------

// CreateInvitation creates a one-time invitation and returns its secret once.
func (c *Client) CreateInvitation(ctx context.Context, role ownsandv1.Role, opts ...CallOption) (*ownsandv1.Invitation, string, error) {
	org, err := c.orgFor(opts)
	if err != nil {
		return nil, "", err
	}
	r, err := call(c, ctx, c.members.CreateInvitation, &ownsandv1.CreateInvitationRequest{OrganizationId: org, Role: role})
	return r.GetInvitation(), r.GetInvitationSecret(), err
}

// ListInvitations lists pending invitations without secrets.
func (c *Client) ListInvitations(ctx context.Context, lo ListOptions, opts ...CallOption) ([]*ownsandv1.Invitation, string, error) {
	org, err := c.orgFor(opts)
	if err != nil {
		return nil, "", err
	}
	r, err := call(c, ctx, c.members.ListInvitations, &ownsandv1.ListInvitationsRequest{OrganizationId: org, PageSize: lo.PageSize, PageToken: lo.PageToken})
	return r.GetInvitations(), r.GetNextPageToken(), err
}

// RevokeInvitation revokes a pending invitation.
func (c *Client) RevokeInvitation(ctx context.Context, invitationID string, opts ...CallOption) error {
	org, err := c.orgFor(opts)
	if err != nil {
		return err
	}
	_, err = call(c, ctx, c.members.RevokeInvitation, &ownsandv1.RevokeInvitationRequest{OrganizationId: org, InvitationId: invitationID})
	return err
}

// AcceptInvitation joins the organization of the invitation secret.
func (c *Client) AcceptInvitation(ctx context.Context, secret string) (*ownsandv1.Organization, *ownsandv1.Member, error) {
	r, err := call(c, ctx, c.members.AcceptInvitation, &ownsandv1.AcceptInvitationRequest{InvitationSecret: secret})
	return r.GetOrganization(), r.GetMember(), err
}

// ListMembers lists members and roles.
func (c *Client) ListMembers(ctx context.Context, lo ListOptions, opts ...CallOption) ([]*ownsandv1.Member, string, error) {
	org, err := c.orgFor(opts)
	if err != nil {
		return nil, "", err
	}
	r, err := call(c, ctx, c.members.ListMembers, &ownsandv1.ListMembersRequest{OrganizationId: org, PageSize: lo.PageSize, PageToken: lo.PageToken})
	return r.GetMembers(), r.GetNextPageToken(), err
}

// UpdateMemberRole changes a member role between admin and member.
func (c *Client) UpdateMemberRole(ctx context.Context, userID string, role ownsandv1.Role, opts ...CallOption) (*ownsandv1.Member, error) {
	org, err := c.orgFor(opts)
	if err != nil {
		return nil, err
	}
	r, err := call(c, ctx, c.members.UpdateMemberRole, &ownsandv1.UpdateMemberRoleRequest{OrganizationId: org, UserId: userID, Role: role})
	return r.GetMember(), err
}

// RemoveMember removes a member.
func (c *Client) RemoveMember(ctx context.Context, userID string, opts ...CallOption) error {
	org, err := c.orgFor(opts)
	if err != nil {
		return err
	}
	_, err = call(c, ctx, c.members.RemoveMember, &ownsandv1.RemoveMemberRequest{OrganizationId: org, UserId: userID})
	return err
}

// ---------------------------------------------------------------------------
// ApiKeyService
// ---------------------------------------------------------------------------

// CreateAPIKey creates an organization API key and returns its secret once.
func (c *Client) CreateAPIKey(ctx context.Context, name string, opts ...CallOption) (*ownsandv1.ApiKey, string, error) {
	org, err := c.orgFor(opts)
	if err != nil {
		return nil, "", err
	}
	r, err := call(c, ctx, c.apiKeys.CreateApiKey, &ownsandv1.CreateApiKeyRequest{OrganizationId: org, Name: name})
	return r.GetApiKey(), r.GetApiKeySecret(), err
}

// ListAPIKeys lists API keys without secrets.
func (c *Client) ListAPIKeys(ctx context.Context, lo ListOptions, opts ...CallOption) ([]*ownsandv1.ApiKey, string, error) {
	org, err := c.orgFor(opts)
	if err != nil {
		return nil, "", err
	}
	r, err := call(c, ctx, c.apiKeys.ListApiKeys, &ownsandv1.ListApiKeysRequest{OrganizationId: org, PageSize: lo.PageSize, PageToken: lo.PageToken})
	return r.GetApiKeys(), r.GetNextPageToken(), err
}

// RevokeAPIKey revokes an API key.
func (c *Client) RevokeAPIKey(ctx context.Context, apiKeyID string, opts ...CallOption) error {
	org, err := c.orgFor(opts)
	if err != nil {
		return err
	}
	_, err = call(c, ctx, c.apiKeys.RevokeApiKey, &ownsandv1.RevokeApiKeyRequest{OrganizationId: org, ApiKeyId: apiKeyID})
	return err
}

// ---------------------------------------------------------------------------
// DeviceService
// ---------------------------------------------------------------------------

// CreateEnrollmentToken creates a one-time enrollment token and returns its
// secret once. A zero expiresIn uses the server default.
func (c *Client) CreateEnrollmentToken(ctx context.Context, expiresIn time.Duration, deviceName string, opts ...CallOption) (*ownsandv1.EnrollmentToken, string, error) {
	org, err := c.orgFor(opts)
	if err != nil {
		return nil, "", err
	}
	req := &ownsandv1.CreateEnrollmentTokenRequest{OrganizationId: org, DeviceName: deviceName}
	if expiresIn > 0 {
		req.ExpiresIn = durationpb.New(expiresIn)
	}
	r, err := call(c, ctx, c.devices.CreateEnrollmentToken, req)
	return r.GetEnrollmentToken(), r.GetEnrollmentTokenSecret(), err
}

// ListEnrollmentTokens lists enrollment tokens without secrets.
func (c *Client) ListEnrollmentTokens(ctx context.Context, lo ListOptions, opts ...CallOption) ([]*ownsandv1.EnrollmentToken, string, error) {
	org, err := c.orgFor(opts)
	if err != nil {
		return nil, "", err
	}
	r, err := call(c, ctx, c.devices.ListEnrollmentTokens, &ownsandv1.ListEnrollmentTokensRequest{OrganizationId: org, PageSize: lo.PageSize, PageToken: lo.PageToken})
	return r.GetEnrollmentTokens(), r.GetNextPageToken(), err
}

// RevokeEnrollmentToken revokes an unused enrollment token.
func (c *Client) RevokeEnrollmentToken(ctx context.Context, tokenID string, opts ...CallOption) error {
	org, err := c.orgFor(opts)
	if err != nil {
		return err
	}
	_, err = call(c, ctx, c.devices.RevokeEnrollmentToken, &ownsandv1.RevokeEnrollmentTokenRequest{OrganizationId: org, EnrollmentTokenId: tokenID})
	return err
}

// ListDevices lists devices.
func (c *Client) ListDevices(ctx context.Context, lo ListOptions, includeRemoved bool, opts ...CallOption) ([]*ownsandv1.Device, string, error) {
	org, err := c.orgFor(opts)
	if err != nil {
		return nil, "", err
	}
	r, err := call(c, ctx, c.devices.ListDevices, &ownsandv1.ListDevicesRequest{OrganizationId: org, PageSize: lo.PageSize, PageToken: lo.PageToken, IncludeRemoved: includeRemoved})
	return r.GetDevices(), r.GetNextPageToken(), err
}

// GetDevice reads one device.
func (c *Client) GetDevice(ctx context.Context, deviceID string, opts ...CallOption) (*ownsandv1.Device, error) {
	org, err := c.orgFor(opts)
	if err != nil {
		return nil, err
	}
	r, err := call(c, ctx, c.devices.GetDevice, &ownsandv1.GetDeviceRequest{OrganizationId: org, DeviceId: deviceID})
	return r.GetDevice(), err
}

// UpdateDeviceLimits sets the upper limits of the device.
func (c *Client) UpdateDeviceLimits(ctx context.Context, deviceID string, limits *ownsandv1.DeviceLimits, opts ...CallOption) (*ownsandv1.Device, error) {
	org, err := c.orgFor(opts)
	if err != nil {
		return nil, err
	}
	r, err := call(c, ctx, c.devices.UpdateDeviceLimits, &ownsandv1.UpdateDeviceLimitsRequest{OrganizationId: org, DeviceId: deviceID, Limits: limits})
	return r.GetDevice(), err
}

// RemoveDevice revokes the device credentials and requests local clean-up.
func (c *Client) RemoveDevice(ctx context.Context, deviceID string, opts ...CallOption) error {
	org, err := c.orgFor(opts)
	if err != nil {
		return err
	}
	_, err = call(c, ctx, c.devices.RemoveDevice, &ownsandv1.RemoveDeviceRequest{OrganizationId: org, DeviceId: deviceID})
	return err
}

// GetAgentVersionPolicy reads the agent version policy.
func (c *Client) GetAgentVersionPolicy(ctx context.Context, opts ...CallOption) (*ownsandv1.AgentVersionPolicy, error) {
	org, err := c.orgFor(opts)
	if err != nil {
		return nil, err
	}
	r, err := call(c, ctx, c.devices.GetAgentVersionPolicy, &ownsandv1.GetAgentVersionPolicyRequest{OrganizationId: org})
	return r.GetPolicy(), err
}

// UpdateAgentVersionPolicy follows the default, pins a version or holds updates.
func (c *Client) UpdateAgentVersionPolicy(ctx context.Context, mode ownsandv1.AgentVersionPolicyMode, pinnedVersion string, opts ...CallOption) (*ownsandv1.AgentVersionPolicy, error) {
	org, err := c.orgFor(opts)
	if err != nil {
		return nil, err
	}
	r, err := call(c, ctx, c.devices.UpdateAgentVersionPolicy, &ownsandv1.UpdateAgentVersionPolicyRequest{OrganizationId: org, Mode: mode, PinnedVersion: pinnedVersion})
	return r.GetPolicy(), err
}

// ---------------------------------------------------------------------------
// SnapshotService
// ---------------------------------------------------------------------------

// GetSnapshot reads one snapshot.
func (c *Client) GetSnapshot(ctx context.Context, snapshotID string, opts ...CallOption) (*ownsandv1.Snapshot, error) {
	org, err := c.orgFor(opts)
	if err != nil {
		return nil, err
	}
	r, err := call(c, ctx, c.snapshots.GetSnapshot, &ownsandv1.GetSnapshotRequest{OrganizationId: org, SnapshotId: snapshotID})
	return r.GetSnapshot(), err
}

// ListSnapshots lists snapshots, optionally of one device.
func (c *Client) ListSnapshots(ctx context.Context, lo ListOptions, deviceID string, opts ...CallOption) ([]*ownsandv1.Snapshot, string, error) {
	org, err := c.orgFor(opts)
	if err != nil {
		return nil, "", err
	}
	req := &ownsandv1.ListSnapshotsRequest{OrganizationId: org, PageSize: lo.PageSize, PageToken: lo.PageToken}
	if deviceID != "" {
		req.DeviceId = &deviceID
	}
	r, err := call(c, ctx, c.snapshots.ListSnapshots, req)
	return r.GetSnapshots(), r.GetNextPageToken(), err
}

// DeleteSnapshot deletes a snapshot.
func (c *Client) DeleteSnapshot(ctx context.Context, snapshotID string, opts ...CallOption) error {
	org, err := c.orgFor(opts)
	if err != nil {
		return err
	}
	_, err = call(c, ctx, c.snapshots.DeleteSnapshot, &ownsandv1.DeleteSnapshotRequest{OrganizationId: org, SnapshotId: snapshotID})
	return err
}

// ExportSnapshot exports a snapshot to R2.
func (c *Client) ExportSnapshot(ctx context.Context, snapshotID string, opts ...CallOption) (*ownsandv1.Snapshot, error) {
	org, err := c.orgFor(opts)
	if err != nil {
		return nil, err
	}
	r, err := call(c, ctx, c.snapshots.ExportSnapshot, &ownsandv1.ExportSnapshotRequest{OrganizationId: org, SnapshotId: snapshotID})
	return r.GetSnapshot(), err
}

// CreateTemplate makes a named template from a snapshot.
func (c *Client) CreateTemplate(ctx context.Context, snapshotID, name string, opts ...CallOption) (*ownsandv1.Template, error) {
	org, err := c.orgFor(opts)
	if err != nil {
		return nil, err
	}
	r, err := call(c, ctx, c.snapshots.CreateTemplate, &ownsandv1.CreateTemplateRequest{OrganizationId: org, SnapshotId: snapshotID, Name: name})
	return r.GetTemplate(), err
}

// ListTemplates lists templates.
func (c *Client) ListTemplates(ctx context.Context, lo ListOptions, opts ...CallOption) ([]*ownsandv1.Template, string, error) {
	org, err := c.orgFor(opts)
	if err != nil {
		return nil, "", err
	}
	r, err := call(c, ctx, c.snapshots.ListTemplates, &ownsandv1.ListTemplatesRequest{OrganizationId: org, PageSize: lo.PageSize, PageToken: lo.PageToken})
	return r.GetTemplates(), r.GetNextPageToken(), err
}

// DeleteTemplate deletes a template. The source snapshot stays.
func (c *Client) DeleteTemplate(ctx context.Context, templateID string, opts ...CallOption) error {
	org, err := c.orgFor(opts)
	if err != nil {
		return err
	}
	_, err = call(c, ctx, c.snapshots.DeleteTemplate, &ownsandv1.DeleteTemplateRequest{OrganizationId: org, TemplateId: templateID})
	return err
}

// GetIceServers returns the STUN server URLs for a peer-to-peer session.
func (c *Client) GetIceServers(ctx context.Context, opts ...CallOption) ([]*ownsandv1.IceServer, error) {
	org, err := c.orgFor(opts)
	if err != nil {
		return nil, err
	}
	r, err := call(c, ctx, c.dataPath.GetIceServers, &ownsandv1.GetIceServersRequest{OrganizationId: org})
	return r.GetIceServers(), err
}
