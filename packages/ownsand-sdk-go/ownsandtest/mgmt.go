package ownsandtest

import (
	"context"
	"errors"
	"net/http"

	"connectrpc.com/connect"
	"github.com/delinoio/mono/packages/ownsand-sdk-go/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
	"google.golang.org/protobuf/proto"
)

// mgmt is the in-memory state of the management services.
type mgmt struct {
	invitations map[string]*ownsandv1.Invitation
	invSecrets  map[string]string // secret -> invitation ID
	members     map[string]*ownsandv1.Member
	apiKeyList  []*ownsandv1.ApiKey
	tokens      []*ownsandv1.EnrollmentToken
	devices     map[string]*ownsandv1.Device
	snapshots   map[string]*ownsandv1.Snapshot
	templates   map[string]*ownsandv1.Template
	policy      *ownsandv1.AgentVersionPolicy
	orgName     string
}

func (s *Server) initMgmt() {
	s.mgmt = mgmt{
		invitations: map[string]*ownsandv1.Invitation{},
		invSecrets:  map[string]string{},
		members:     map[string]*ownsandv1.Member{},
		devices: map[string]*ownsandv1.Device{"device-1": {OrganizationId: s.Org, DeviceId: "device-1", Name: "fake",
			State: ownsandv1.DeviceState_DEVICE_STATE_ONLINE}},
		snapshots: map[string]*ownsandv1.Snapshot{},
		templates: map[string]*ownsandv1.Template{},
		policy:    &ownsandv1.AgentVersionPolicy{OrganizationId: s.Org, Mode: ownsandv1.AgentVersionPolicyMode_AGENT_VERSION_POLICY_MODE_FOLLOW_DEFAULT},
		orgName:   "fake-org",
	}
}

// AddUser accepts token as the Logto access token of userID, an owner of
// the organization.
func (s *Server) AddUser(token, userID string) {
	s.AddCredential(token, Principal{Kind: ownsandv1.PrincipalKind_PRINCIPAL_KIND_USER, ID: userID})
	s.mu.Lock()
	s.members[userID] = &ownsandv1.Member{OrganizationId: s.Org, UserId: userID, Role: ownsandv1.Role_ROLE_OWNER, MembershipId: datapath.NewID()}
	s.mu.Unlock()
}

// userOnly authorizes an RPC that API keys cannot use.
func (s *Server) userOnly(h http.Header, org string) (Principal, error) {
	p, err := s.checkOrg(h, org)
	if err != nil {
		return p, err
	}
	if p.Kind != ownsandv1.PrincipalKind_PRINCIPAL_KIND_USER {
		return p, connect.NewError(connect.CodePermissionDenied, errors.New("API keys cannot use this RPC"))
	}
	return p, nil
}

// userNoOrg authorizes an RPC without an organization: users only.
func (s *Server) userNoOrg(h http.Header) (Principal, error) {
	p, err := s.principal(h)
	if err != nil {
		return p, err
	}
	if p.Kind != ownsandv1.PrincipalKind_PRINCIPAL_KIND_USER {
		return p, connect.NewError(connect.CodePermissionDenied, errors.New("only users can use this RPC"))
	}
	return p, nil
}

func secret(prefix string) string { return prefix + datapath.NewID() }

type orgSvc struct{ s *Server }

func (h *orgSvc) org() *ownsandv1.Organization {
	return &ownsandv1.Organization{OrganizationId: h.s.Org, Name: h.s.orgName, CallerRole: ownsandv1.Role_ROLE_OWNER}
}

func (h *orgSvc) CreateOrganization(ctx context.Context, req *connect.Request[ownsandv1.CreateOrganizationRequest]) (*connect.Response[ownsandv1.CreateOrganizationResponse], error) {
	if _, err := h.s.userNoOrg(req.Header()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&ownsandv1.CreateOrganizationResponse{Organization: &ownsandv1.Organization{OrganizationId: datapath.NewID(), Name: req.Msg.GetName(), CallerRole: ownsandv1.Role_ROLE_OWNER}}), nil
}

func (h *orgSvc) GetOrganization(ctx context.Context, req *connect.Request[ownsandv1.GetOrganizationRequest]) (*connect.Response[ownsandv1.GetOrganizationResponse], error) {
	if _, err := h.s.userOnly(req.Header(), req.Msg.GetOrganizationId()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&ownsandv1.GetOrganizationResponse{Organization: h.org()}), nil
}

func (h *orgSvc) ListOrganizations(ctx context.Context, req *connect.Request[ownsandv1.ListOrganizationsRequest]) (*connect.Response[ownsandv1.ListOrganizationsResponse], error) {
	if _, err := h.s.userNoOrg(req.Header()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&ownsandv1.ListOrganizationsResponse{Organizations: []*ownsandv1.Organization{h.org()}}), nil
}

func (h *orgSvc) DeleteOrganization(ctx context.Context, req *connect.Request[ownsandv1.DeleteOrganizationRequest]) (*connect.Response[ownsandv1.DeleteOrganizationResponse], error) {
	if _, err := h.s.userOnly(req.Header(), req.Msg.GetOrganizationId()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&ownsandv1.DeleteOrganizationResponse{}), nil
}

func (h *orgSvc) TransferOwnership(ctx context.Context, req *connect.Request[ownsandv1.TransferOwnershipRequest]) (*connect.Response[ownsandv1.TransferOwnershipResponse], error) {
	if _, err := h.s.userOnly(req.Header(), req.Msg.GetOrganizationId()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&ownsandv1.TransferOwnershipResponse{}), nil
}

type memberSvc struct{ s *Server }

func (h *memberSvc) CreateInvitation(ctx context.Context, req *connect.Request[ownsandv1.CreateInvitationRequest]) (*connect.Response[ownsandv1.CreateInvitationResponse], error) {
	s := h.s
	if _, err := s.userOnly(req.Header(), req.Msg.GetOrganizationId()); err != nil {
		return nil, err
	}
	sec := secret("osi_")
	inv := &ownsandv1.Invitation{OrganizationId: s.Org, InvitationId: datapath.NewID(), Role: req.Msg.GetRole(),
		State: ownsandv1.InvitationState_INVITATION_STATE_PENDING, SecretPrefix: sec[:8]}
	s.mu.Lock()
	s.invitations[inv.InvitationId] = inv
	s.invSecrets[sec] = inv.InvitationId
	s.mu.Unlock()
	return connect.NewResponse(&ownsandv1.CreateInvitationResponse{Invitation: proto.Clone(inv).(*ownsandv1.Invitation), InvitationSecret: sec}), nil
}

func (h *memberSvc) ListInvitations(ctx context.Context, req *connect.Request[ownsandv1.ListInvitationsRequest]) (*connect.Response[ownsandv1.ListInvitationsResponse], error) {
	s := h.s
	if _, err := s.userOnly(req.Header(), req.Msg.GetOrganizationId()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	resp := &ownsandv1.ListInvitationsResponse{}
	for _, inv := range s.invitations {
		resp.Invitations = append(resp.Invitations, proto.Clone(inv).(*ownsandv1.Invitation))
	}
	return connect.NewResponse(resp), nil
}

func (h *memberSvc) RevokeInvitation(ctx context.Context, req *connect.Request[ownsandv1.RevokeInvitationRequest]) (*connect.Response[ownsandv1.RevokeInvitationResponse], error) {
	s := h.s
	if _, err := s.userOnly(req.Header(), req.Msg.GetOrganizationId()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	inv := s.invitations[req.Msg.GetInvitationId()]
	if inv == nil {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("invitation not found"))
	}
	inv.State = ownsandv1.InvitationState_INVITATION_STATE_REVOKED
	return connect.NewResponse(&ownsandv1.RevokeInvitationResponse{}), nil
}

func (h *memberSvc) AcceptInvitation(ctx context.Context, req *connect.Request[ownsandv1.AcceptInvitationRequest]) (*connect.Response[ownsandv1.AcceptInvitationResponse], error) {
	s := h.s
	p, err := s.userNoOrg(req.Header())
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.invSecrets[req.Msg.GetInvitationSecret()]
	if !ok || s.invitations[id].GetState() != ownsandv1.InvitationState_INVITATION_STATE_PENDING {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("invitation not found"))
	}
	inv := s.invitations[id]
	inv.State = ownsandv1.InvitationState_INVITATION_STATE_ACCEPTED
	m := &ownsandv1.Member{OrganizationId: s.Org, MembershipId: datapath.NewID(), UserId: p.ID, Role: inv.GetRole()}
	s.members[p.ID] = m
	return connect.NewResponse(&ownsandv1.AcceptInvitationResponse{Organization: &ownsandv1.Organization{OrganizationId: s.Org, Name: s.orgName, CallerRole: m.Role}, Member: proto.Clone(m).(*ownsandv1.Member)}), nil
}

func (h *memberSvc) ListMembers(ctx context.Context, req *connect.Request[ownsandv1.ListMembersRequest]) (*connect.Response[ownsandv1.ListMembersResponse], error) {
	s := h.s
	if _, err := s.userOnly(req.Header(), req.Msg.GetOrganizationId()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	resp := &ownsandv1.ListMembersResponse{}
	for _, m := range s.members {
		resp.Members = append(resp.Members, proto.Clone(m).(*ownsandv1.Member))
	}
	return connect.NewResponse(resp), nil
}

func (h *memberSvc) UpdateMemberRole(ctx context.Context, req *connect.Request[ownsandv1.UpdateMemberRoleRequest]) (*connect.Response[ownsandv1.UpdateMemberRoleResponse], error) {
	s := h.s
	if _, err := s.userOnly(req.Header(), req.Msg.GetOrganizationId()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.members[req.Msg.GetUserId()]
	if m == nil {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("member not found"))
	}
	m.Role = req.Msg.GetRole()
	return connect.NewResponse(&ownsandv1.UpdateMemberRoleResponse{Member: proto.Clone(m).(*ownsandv1.Member)}), nil
}

func (h *memberSvc) RemoveMember(ctx context.Context, req *connect.Request[ownsandv1.RemoveMemberRequest]) (*connect.Response[ownsandv1.RemoveMemberResponse], error) {
	s := h.s
	if _, err := s.userOnly(req.Header(), req.Msg.GetOrganizationId()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.members, req.Msg.GetUserId())
	return connect.NewResponse(&ownsandv1.RemoveMemberResponse{}), nil
}

type apiKeySvc struct{ s *Server }

func (h *apiKeySvc) CreateApiKey(ctx context.Context, req *connect.Request[ownsandv1.CreateApiKeyRequest]) (*connect.Response[ownsandv1.CreateApiKeyResponse], error) {
	s := h.s
	if _, err := s.userOnly(req.Header(), req.Msg.GetOrganizationId()); err != nil {
		return nil, err
	}
	sec := secret("osk_")
	k := &ownsandv1.ApiKey{OrganizationId: s.Org, ApiKeyId: datapath.NewID(), Name: req.Msg.GetName(), State: ownsandv1.ApiKeyState_API_KEY_STATE_ACTIVE, SecretPrefix: sec[:8]}
	s.mu.Lock()
	s.apiKeyList = append(s.apiKeyList, k)
	s.creds[sec] = Principal{Kind: ownsandv1.PrincipalKind_PRINCIPAL_KIND_API_KEY, ID: k.ApiKeyId}
	s.mu.Unlock()
	return connect.NewResponse(&ownsandv1.CreateApiKeyResponse{ApiKey: proto.Clone(k).(*ownsandv1.ApiKey), ApiKeySecret: sec}), nil
}

func (h *apiKeySvc) ListApiKeys(ctx context.Context, req *connect.Request[ownsandv1.ListApiKeysRequest]) (*connect.Response[ownsandv1.ListApiKeysResponse], error) {
	s := h.s
	if _, err := s.userOnly(req.Header(), req.Msg.GetOrganizationId()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	resp := &ownsandv1.ListApiKeysResponse{}
	for _, k := range s.apiKeyList {
		resp.ApiKeys = append(resp.ApiKeys, proto.Clone(k).(*ownsandv1.ApiKey))
	}
	return connect.NewResponse(resp), nil
}

func (h *apiKeySvc) RevokeApiKey(ctx context.Context, req *connect.Request[ownsandv1.RevokeApiKeyRequest]) (*connect.Response[ownsandv1.RevokeApiKeyResponse], error) {
	s := h.s
	if _, err := s.userOnly(req.Header(), req.Msg.GetOrganizationId()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range s.apiKeyList {
		if k.ApiKeyId == req.Msg.GetApiKeyId() {
			k.State = ownsandv1.ApiKeyState_API_KEY_STATE_REVOKED
			return connect.NewResponse(&ownsandv1.RevokeApiKeyResponse{}), nil
		}
	}
	return nil, connect.NewError(connect.CodeNotFound, errors.New("API key not found"))
}

type deviceSvc struct {
	s *Server
}

func (h *deviceSvc) CreateEnrollmentToken(ctx context.Context, req *connect.Request[ownsandv1.CreateEnrollmentTokenRequest]) (*connect.Response[ownsandv1.CreateEnrollmentTokenResponse], error) {
	s := h.s
	if _, err := s.userOnly(req.Header(), req.Msg.GetOrganizationId()); err != nil {
		return nil, err
	}
	sec := secret("ose_")
	t := &ownsandv1.EnrollmentToken{OrganizationId: s.Org, EnrollmentTokenId: datapath.NewID(), State: ownsandv1.EnrollmentTokenState_ENROLLMENT_TOKEN_STATE_PENDING, SecretPrefix: sec[:8]}
	s.mu.Lock()
	s.tokens = append(s.tokens, t)
	s.mu.Unlock()
	return connect.NewResponse(&ownsandv1.CreateEnrollmentTokenResponse{EnrollmentToken: proto.Clone(t).(*ownsandv1.EnrollmentToken), EnrollmentTokenSecret: sec}), nil
}

func (h *deviceSvc) ListEnrollmentTokens(ctx context.Context, req *connect.Request[ownsandv1.ListEnrollmentTokensRequest]) (*connect.Response[ownsandv1.ListEnrollmentTokensResponse], error) {
	s := h.s
	if _, err := s.userOnly(req.Header(), req.Msg.GetOrganizationId()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	resp := &ownsandv1.ListEnrollmentTokensResponse{}
	for _, t := range s.tokens {
		resp.EnrollmentTokens = append(resp.EnrollmentTokens, proto.Clone(t).(*ownsandv1.EnrollmentToken))
	}
	return connect.NewResponse(resp), nil
}

func (h *deviceSvc) RevokeEnrollmentToken(ctx context.Context, req *connect.Request[ownsandv1.RevokeEnrollmentTokenRequest]) (*connect.Response[ownsandv1.RevokeEnrollmentTokenResponse], error) {
	if _, err := h.s.userOnly(req.Header(), req.Msg.GetOrganizationId()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&ownsandv1.RevokeEnrollmentTokenResponse{}), nil
}

func (h *deviceSvc) ListDevices(ctx context.Context, req *connect.Request[ownsandv1.ListDevicesRequest]) (*connect.Response[ownsandv1.ListDevicesResponse], error) {
	s := h.s
	if _, err := s.checkOrg(req.Header(), req.Msg.GetOrganizationId()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	resp := &ownsandv1.ListDevicesResponse{}
	for _, d := range s.devices {
		resp.Devices = append(resp.Devices, proto.Clone(d).(*ownsandv1.Device))
	}
	return connect.NewResponse(resp), nil
}

func (h *deviceSvc) GetDevice(ctx context.Context, req *connect.Request[ownsandv1.GetDeviceRequest]) (*connect.Response[ownsandv1.GetDeviceResponse], error) {
	s := h.s
	if _, err := s.checkOrg(req.Header(), req.Msg.GetOrganizationId()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.devices[req.Msg.GetDeviceId()]
	if d == nil {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("device not found"))
	}
	return connect.NewResponse(&ownsandv1.GetDeviceResponse{Device: proto.Clone(d).(*ownsandv1.Device)}), nil
}

func (h *deviceSvc) UpdateDeviceLimits(ctx context.Context, req *connect.Request[ownsandv1.UpdateDeviceLimitsRequest]) (*connect.Response[ownsandv1.UpdateDeviceLimitsResponse], error) {
	s := h.s
	if _, err := s.userOnly(req.Header(), req.Msg.GetOrganizationId()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.devices[req.Msg.GetDeviceId()]
	if d == nil {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("device not found"))
	}
	d.Limits = req.Msg.GetLimits()
	return connect.NewResponse(&ownsandv1.UpdateDeviceLimitsResponse{Device: proto.Clone(d).(*ownsandv1.Device)}), nil
}

func (h *deviceSvc) RemoveDevice(ctx context.Context, req *connect.Request[ownsandv1.RemoveDeviceRequest]) (*connect.Response[ownsandv1.RemoveDeviceResponse], error) {
	if _, err := h.s.userOnly(req.Header(), req.Msg.GetOrganizationId()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&ownsandv1.RemoveDeviceResponse{}), nil
}

func (h *deviceSvc) GetAgentVersionPolicy(ctx context.Context, req *connect.Request[ownsandv1.GetAgentVersionPolicyRequest]) (*connect.Response[ownsandv1.GetAgentVersionPolicyResponse], error) {
	s := h.s
	if _, err := s.checkOrg(req.Header(), req.Msg.GetOrganizationId()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return connect.NewResponse(&ownsandv1.GetAgentVersionPolicyResponse{Policy: proto.Clone(s.policy).(*ownsandv1.AgentVersionPolicy)}), nil
}

func (h *deviceSvc) UpdateAgentVersionPolicy(ctx context.Context, req *connect.Request[ownsandv1.UpdateAgentVersionPolicyRequest]) (*connect.Response[ownsandv1.UpdateAgentVersionPolicyResponse], error) {
	s := h.s
	if _, err := s.userOnly(req.Header(), req.Msg.GetOrganizationId()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.policy = &ownsandv1.AgentVersionPolicy{OrganizationId: s.Org, Mode: req.Msg.GetMode(), PinnedVersion: req.Msg.GetPinnedVersion()}
	return connect.NewResponse(&ownsandv1.UpdateAgentVersionPolicyResponse{Policy: proto.Clone(s.policy).(*ownsandv1.AgentVersionPolicy)}), nil
}

type snapshotSvc struct {
	s *Server
}

var _ ownsandv1connect.SnapshotServiceHandler = (*snapshotSvc)(nil)

func (h *snapshotSvc) CreateSnapshot(ctx context.Context, req *connect.Request[ownsandv1.CreateSnapshotRequest]) (*connect.Response[ownsandv1.CreateSnapshotResponse], error) {
	s := h.s
	if _, err := s.checkOrg(req.Header(), req.Msg.GetOrganizationId()); err != nil {
		return nil, err
	}
	sn := &ownsandv1.Snapshot{OrganizationId: s.Org, SnapshotId: datapath.NewID(), SandboxId: req.Msg.GetSandboxId(), DeviceId: "device-1",
		State: ownsandv1.SnapshotState_SNAPSHOT_STATE_CREATING, ExportState: ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_NOT_EXPORTED}
	s.mu.Lock()
	s.snapshots[sn.SnapshotId] = sn
	s.mu.Unlock()
	return connect.NewResponse(&ownsandv1.CreateSnapshotResponse{Snapshot: proto.Clone(sn).(*ownsandv1.Snapshot)}), nil
}

func (h *snapshotSvc) snapshot(hdr http.Header, org, id string) (*ownsandv1.Snapshot, error) {
	s := h.s
	if _, err := s.checkOrg(hdr, org); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sn := s.snapshots[id]
	if sn == nil {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("snapshot not found"))
	}
	return sn, nil
}

func (h *snapshotSvc) GetSnapshot(ctx context.Context, req *connect.Request[ownsandv1.GetSnapshotRequest]) (*connect.Response[ownsandv1.GetSnapshotResponse], error) {
	sn, err := h.snapshot(req.Header(), req.Msg.GetOrganizationId(), req.Msg.GetSnapshotId())
	if err != nil {
		return nil, err
	}
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	return connect.NewResponse(&ownsandv1.GetSnapshotResponse{Snapshot: proto.Clone(sn).(*ownsandv1.Snapshot)}), nil
}

func (h *snapshotSvc) ListSnapshots(ctx context.Context, req *connect.Request[ownsandv1.ListSnapshotsRequest]) (*connect.Response[ownsandv1.ListSnapshotsResponse], error) {
	s := h.s
	if _, err := s.checkOrg(req.Header(), req.Msg.GetOrganizationId()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	resp := &ownsandv1.ListSnapshotsResponse{}
	for _, sn := range s.snapshots {
		resp.Snapshots = append(resp.Snapshots, proto.Clone(sn).(*ownsandv1.Snapshot))
	}
	return connect.NewResponse(resp), nil
}

func (h *snapshotSvc) DeleteSnapshot(ctx context.Context, req *connect.Request[ownsandv1.DeleteSnapshotRequest]) (*connect.Response[ownsandv1.DeleteSnapshotResponse], error) {
	if _, err := h.snapshot(req.Header(), req.Msg.GetOrganizationId(), req.Msg.GetSnapshotId()); err != nil {
		return nil, err
	}
	h.s.mu.Lock()
	delete(h.s.snapshots, req.Msg.GetSnapshotId())
	h.s.mu.Unlock()
	return connect.NewResponse(&ownsandv1.DeleteSnapshotResponse{}), nil
}

func (h *snapshotSvc) ExportSnapshot(ctx context.Context, req *connect.Request[ownsandv1.ExportSnapshotRequest]) (*connect.Response[ownsandv1.ExportSnapshotResponse], error) {
	sn, err := h.snapshot(req.Header(), req.Msg.GetOrganizationId(), req.Msg.GetSnapshotId())
	if err != nil {
		return nil, err
	}
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	sn.ExportState = ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTING
	return connect.NewResponse(&ownsandv1.ExportSnapshotResponse{Snapshot: proto.Clone(sn).(*ownsandv1.Snapshot)}), nil
}

func (h *snapshotSvc) CreateTemplate(ctx context.Context, req *connect.Request[ownsandv1.CreateTemplateRequest]) (*connect.Response[ownsandv1.CreateTemplateResponse], error) {
	if _, err := h.snapshot(req.Header(), req.Msg.GetOrganizationId(), req.Msg.GetSnapshotId()); err != nil {
		return nil, err
	}
	t := &ownsandv1.Template{OrganizationId: h.s.Org, TemplateId: datapath.NewID(), Name: req.Msg.GetName(), SnapshotId: req.Msg.GetSnapshotId()}
	h.s.mu.Lock()
	h.s.templates[t.TemplateId] = t
	h.s.mu.Unlock()
	return connect.NewResponse(&ownsandv1.CreateTemplateResponse{Template: proto.Clone(t).(*ownsandv1.Template)}), nil
}

func (h *snapshotSvc) ListTemplates(ctx context.Context, req *connect.Request[ownsandv1.ListTemplatesRequest]) (*connect.Response[ownsandv1.ListTemplatesResponse], error) {
	s := h.s
	if _, err := s.checkOrg(req.Header(), req.Msg.GetOrganizationId()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	resp := &ownsandv1.ListTemplatesResponse{}
	for _, t := range s.templates {
		resp.Templates = append(resp.Templates, proto.Clone(t).(*ownsandv1.Template))
	}
	return connect.NewResponse(resp), nil
}

func (h *snapshotSvc) DeleteTemplate(ctx context.Context, req *connect.Request[ownsandv1.DeleteTemplateRequest]) (*connect.Response[ownsandv1.DeleteTemplateResponse], error) {
	s := h.s
	if _, err := s.checkOrg(req.Header(), req.Msg.GetOrganizationId()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.templates, req.Msg.GetTemplateId())
	return connect.NewResponse(&ownsandv1.DeleteTemplateResponse{}), nil
}
