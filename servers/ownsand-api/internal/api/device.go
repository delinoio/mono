// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
	"github.com/delinoio/mono/servers/ownsand-api/internal/authz"
	"github.com/delinoio/mono/servers/ownsand-api/internal/secret"
	"github.com/delinoio/mono/servers/ownsand-api/internal/store"
)

// MaxEnrollmentTokenLifetime is the longest (and default) enrollment token expiry.
const MaxEnrollmentTokenLifetime = 24 * time.Hour

// DeviceService implements ownsandv1connect.DeviceServiceHandler.
type DeviceService struct{ *Server }

var _ ownsandv1connect.DeviceServiceHandler = DeviceService{}

func (s DeviceService) tokenProto(t store.EnrollmentToken) *ownsandv1.EnrollmentToken {
	return &ownsandv1.EnrollmentToken{
		OrganizationId: t.OrganizationID, EnrollmentTokenId: t.ID, State: t.State(s.now()), SecretPrefix: t.SecretPrefix,
		CreateTime: ts(t.CreatedAt), ExpireTime: ts(t.ExpiresAt), CreatedBy: principalProto(t.CreatedBy), DeviceId: t.DeviceID,
	}
}

// DeviceProto converts a device row.
func DeviceProto(d store.Device) *ownsandv1.Device {
	return &ownsandv1.Device{
		OrganizationId: d.OrganizationID, DeviceId: d.ID, Name: d.Name, State: d.State, Capabilities: d.Capabilities,
		Limits:       &ownsandv1.DeviceLimits{VcpuCount: d.Limits.VCPUCount, MemoryMib: d.Limits.MemoryMiB, DiskGib: d.Limits.DiskGiB},
		Allocated:    &ownsandv1.Resources{VcpuCount: d.Allocated.VCPUCount, MemoryMib: d.Allocated.MemoryMiB, DiskGib: d.Allocated.DiskGiB},
		AgentVersion: d.AgentVersion, UpdateState: d.UpdateState, EnrollTime: ts(d.EnrolledAt), LastSeenTime: tsPtr(d.LastSeenAt),
	}
}

func (s DeviceService) CreateEnrollmentToken(ctx context.Context, req *connect.Request[ownsandv1.CreateEnrollmentTokenRequest]) (*connect.Response[ownsandv1.CreateEnrollmentTokenResponse], error) {
	p, _, err := s.authorize(ctx, req.Msg.GetOrganizationId(), authz.ActionManageDevices)
	if err != nil {
		return nil, err
	}
	ttl, err := EnrollmentTokenTTL(req.Msg)
	if err != nil {
		return nil, err
	}
	if err := validName("device_name", req.Msg.GetDeviceName(), false); err != nil {
		return nil, err
	}
	value := secret.New(secret.KindEnrollmentToken)
	t, err := s.Store.CreateEnrollmentToken(ctx, req.Msg.GetOrganizationId(), req.Msg.GetDeviceName(), ttl, actorOf(p), secret.Hash(value), secret.DisplayPrefix(value), requestID())
	if err != nil {
		return nil, s.internal(ctx, "create_enrollment_token", err)
	}
	return connect.NewResponse(&ownsandv1.CreateEnrollmentTokenResponse{EnrollmentToken: s.tokenProto(t), EnrollmentTokenSecret: value}), nil
}

// EnrollmentTokenTTL validates expires_in: absent means 24 hours; otherwise
// it must be positive and at most 24 hours.
func EnrollmentTokenTTL(req *ownsandv1.CreateEnrollmentTokenRequest) (time.Duration, error) {
	if req.GetExpiresIn() == nil {
		return MaxEnrollmentTokenLifetime, nil
	}
	if err := req.GetExpiresIn().CheckValid(); err != nil {
		return 0, invalid("expires_in is not a valid duration")
	}
	d := req.GetExpiresIn().AsDuration()
	if d <= 0 || d > MaxEnrollmentTokenLifetime {
		return 0, invalid("expires_in must be greater than zero and at most 24 hours")
	}
	return d, nil
}

func (s DeviceService) ListEnrollmentTokens(ctx context.Context, req *connect.Request[ownsandv1.ListEnrollmentTokensRequest]) (*connect.Response[ownsandv1.ListEnrollmentTokensResponse], error) {
	if _, _, err := s.authorize(ctx, req.Msg.GetOrganizationId(), authz.ActionManageDevices); err != nil {
		return nil, err
	}
	pg, err := page(req.Msg.GetPageSize(), req.Msg.GetPageToken())
	if err != nil {
		return nil, err
	}
	list, err := s.Store.ListEnrollmentTokens(ctx, req.Msg.GetOrganizationId(), pg)
	if err != nil {
		return nil, s.internal(ctx, "list_enrollment_tokens", err)
	}
	resp := &ownsandv1.ListEnrollmentTokensResponse{}
	var ids []string
	for _, t := range list {
		resp.EnrollmentTokens = append(resp.EnrollmentTokens, s.tokenProto(t))
		ids = append(ids, t.ID)
	}
	resp.NextPageToken = store.NextToken(ids, pg.Size)
	return connect.NewResponse(resp), nil
}

func (s DeviceService) RevokeEnrollmentToken(ctx context.Context, req *connect.Request[ownsandv1.RevokeEnrollmentTokenRequest]) (*connect.Response[ownsandv1.RevokeEnrollmentTokenResponse], error) {
	p, _, err := s.authorize(ctx, req.Msg.GetOrganizationId(), authz.ActionManageDevices)
	if err != nil {
		return nil, err
	}
	err = s.Store.RevokeEnrollmentToken(ctx, req.Msg.GetOrganizationId(), req.Msg.GetEnrollmentTokenId(), actorOf(p), requestID())
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, notFound("enrollment token")
	case errors.Is(err, store.ErrConflict):
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("the enrollment token was used or has expired"))
	case err != nil:
		return nil, s.internal(ctx, "revoke_enrollment_token", err)
	}
	return connect.NewResponse(&ownsandv1.RevokeEnrollmentTokenResponse{}), nil
}

func (s DeviceService) ListDevices(ctx context.Context, req *connect.Request[ownsandv1.ListDevicesRequest]) (*connect.Response[ownsandv1.ListDevicesResponse], error) {
	if _, _, err := s.authorize(ctx, req.Msg.GetOrganizationId(), authz.ActionReadDevices); err != nil {
		return nil, err
	}
	pg, err := page(req.Msg.GetPageSize(), req.Msg.GetPageToken())
	if err != nil {
		return nil, err
	}
	list, err := s.Store.ListDevices(ctx, req.Msg.GetOrganizationId(), req.Msg.GetIncludeRemoved(), pg)
	if err != nil {
		return nil, s.internal(ctx, "list_devices", err)
	}
	resp := &ownsandv1.ListDevicesResponse{}
	var ids []string
	for _, d := range list {
		resp.Devices = append(resp.Devices, DeviceProto(d))
		ids = append(ids, d.ID)
	}
	resp.NextPageToken = store.NextToken(ids, pg.Size)
	return connect.NewResponse(resp), nil
}

func (s DeviceService) GetDevice(ctx context.Context, req *connect.Request[ownsandv1.GetDeviceRequest]) (*connect.Response[ownsandv1.GetDeviceResponse], error) {
	if _, _, err := s.authorize(ctx, req.Msg.GetOrganizationId(), authz.ActionReadDevices); err != nil {
		return nil, err
	}
	d, err := s.Store.GetDevice(ctx, req.Msg.GetOrganizationId(), req.Msg.GetDeviceId())
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFound("device")
	}
	if err != nil {
		return nil, s.internal(ctx, "get_device", err)
	}
	return connect.NewResponse(&ownsandv1.GetDeviceResponse{Device: DeviceProto(d)}), nil
}

// ValidateLimits checks device limits: present and not negative.
func ValidateLimits(l *ownsandv1.DeviceLimits) error {
	if l == nil {
		return invalid("limits is required")
	}
	if l.GetVcpuCount() < 0 || l.GetMemoryMib() < 0 || l.GetDiskGib() < 0 {
		return invalid("limits must not be negative")
	}
	return nil
}

func (s DeviceService) UpdateDeviceLimits(ctx context.Context, req *connect.Request[ownsandv1.UpdateDeviceLimitsRequest]) (*connect.Response[ownsandv1.UpdateDeviceLimitsResponse], error) {
	p, _, err := s.authorize(ctx, req.Msg.GetOrganizationId(), authz.ActionManageDevices)
	if err != nil {
		return nil, err
	}
	if err := ValidateLimits(req.Msg.GetLimits()); err != nil {
		return nil, err
	}
	l := req.Msg.GetLimits()
	err = s.Store.UpdateDeviceLimits(ctx, req.Msg.GetOrganizationId(), req.Msg.GetDeviceId(),
		store.Limits{VCPUCount: l.GetVcpuCount(), MemoryMiB: l.GetMemoryMib(), DiskGiB: l.GetDiskGib()}, actorOf(p), requestID())
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, notFound("device")
	case errors.Is(err, store.ErrConflict):
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("the device was removed"))
	case err != nil:
		return nil, s.internal(ctx, "update_device_limits", err)
	}
	d, err := s.Store.GetDevice(ctx, req.Msg.GetOrganizationId(), req.Msg.GetDeviceId())
	if err != nil {
		return nil, s.internal(ctx, "get_device", err)
	}
	return connect.NewResponse(&ownsandv1.UpdateDeviceLimitsResponse{Device: DeviceProto(d)}), nil
}

func (s DeviceService) RemoveDevice(ctx context.Context, req *connect.Request[ownsandv1.RemoveDeviceRequest]) (*connect.Response[ownsandv1.RemoveDeviceResponse], error) {
	p, _, err := s.authorize(ctx, req.Msg.GetOrganizationId(), authz.ActionManageDevices)
	if err != nil {
		return nil, err
	}
	err = s.Store.RemoveDevice(ctx, req.Msg.GetOrganizationId(), req.Msg.GetDeviceId(), actorOf(p), requestID())
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFound("device")
	}
	if err != nil {
		return nil, s.internal(ctx, "remove_device", err)
	}
	return connect.NewResponse(&ownsandv1.RemoveDeviceResponse{}), nil
}

func policyProto(p store.AgentVersionPolicy) *ownsandv1.AgentVersionPolicy {
	return &ownsandv1.AgentVersionPolicy{OrganizationId: p.OrganizationID, Mode: p.Mode, PinnedVersion: p.PinnedVersion, DefaultVersions: p.DefaultVersions}
}

func (s DeviceService) GetAgentVersionPolicy(ctx context.Context, req *connect.Request[ownsandv1.GetAgentVersionPolicyRequest]) (*connect.Response[ownsandv1.GetAgentVersionPolicyResponse], error) {
	if _, _, err := s.authorize(ctx, req.Msg.GetOrganizationId(), authz.ActionReadDevices); err != nil {
		return nil, err
	}
	pol, err := s.Store.GetAgentVersionPolicy(ctx, req.Msg.GetOrganizationId())
	if err != nil {
		return nil, s.internal(ctx, "get_agent_version_policy", err)
	}
	return connect.NewResponse(&ownsandv1.GetAgentVersionPolicyResponse{Policy: policyProto(pol)}), nil
}

// ValidatePolicy checks an agent version policy update.
func ValidatePolicy(mode ownsandv1.AgentVersionPolicyMode, pinned string) error {
	switch mode {
	case ownsandv1.AgentVersionPolicyMode_AGENT_VERSION_POLICY_MODE_FOLLOW_DEFAULT, ownsandv1.AgentVersionPolicyMode_AGENT_VERSION_POLICY_MODE_HOLD:
		if pinned != "" {
			return invalid("pinned_version is allowed only with AGENT_VERSION_POLICY_MODE_PIN")
		}
	case ownsandv1.AgentVersionPolicyMode_AGENT_VERSION_POLICY_MODE_PIN:
		if pinned == "" || len(pinned) > 64 {
			return invalid("pinned_version is required with AGENT_VERSION_POLICY_MODE_PIN and must be at most 64 bytes")
		}
	default:
		return invalid("mode is required")
	}
	return nil
}

func (s DeviceService) UpdateAgentVersionPolicy(ctx context.Context, req *connect.Request[ownsandv1.UpdateAgentVersionPolicyRequest]) (*connect.Response[ownsandv1.UpdateAgentVersionPolicyResponse], error) {
	p, _, err := s.authorize(ctx, req.Msg.GetOrganizationId(), authz.ActionManageDevices)
	if err != nil {
		return nil, err
	}
	if err := ValidatePolicy(req.Msg.GetMode(), req.Msg.GetPinnedVersion()); err != nil {
		return nil, err
	}
	err = s.Store.UpdateAgentVersionPolicy(ctx, req.Msg.GetOrganizationId(), req.Msg.GetMode(), req.Msg.GetPinnedVersion(), actorOf(p), requestID())
	if errors.Is(err, store.ErrRevokedVersion) {
		return nil, invalid("the pinned version is revoked")
	}
	if err != nil {
		return nil, s.internal(ctx, "update_agent_version_policy", err)
	}
	pol, err := s.Store.GetAgentVersionPolicy(ctx, req.Msg.GetOrganizationId())
	if err != nil {
		return nil, s.internal(ctx, "get_agent_version_policy", err)
	}
	return connect.NewResponse(&ownsandv1.UpdateAgentVersionPolicyResponse{Policy: policyProto(pol)}), nil
}
