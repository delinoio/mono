// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
	"github.com/delinoio/mono/servers/ownsand-api/internal/agent"
	"github.com/delinoio/mono/servers/ownsand-api/internal/authz"
	"github.com/delinoio/mono/servers/ownsand-api/internal/store"
)

// SnapshotService implements ownsandv1connect.SnapshotServiceHandler.
type SnapshotService struct {
	*Server
	Agents *agent.Hub
	// ExportConfigured is false when R2 is not configured; exports then fail
	// with unavailable.
	ExportConfigured bool
}

var _ ownsandv1connect.SnapshotServiceHandler = SnapshotService{}

// SnapshotProto converts a snapshot row.
func SnapshotProto(sn store.Snapshot) *ownsandv1.Snapshot {
	return &ownsandv1.Snapshot{
		OrganizationId: sn.OrganizationID, SnapshotId: sn.ID, SandboxId: sn.SandboxID, DeviceId: sn.DeviceID, State: sn.State, ExportState: sn.ExportState,
		GuestOs: sn.GuestOS, Arch: sn.Arch, HostOs: sn.HostOS,
		Resources:   &ownsandv1.Resources{VcpuCount: sn.Resources.VCPUCount, MemoryMib: sn.Resources.MemoryMiB, DiskGib: sn.Resources.DiskGiB},
		NetworkMode: sn.NetworkMode, HasMemoryState: sn.HasMemoryState, SizeBytes: sn.SizeBytes, MacosHardwareModel: sn.MacOSHardwareModel,
		CreatedBy: principalProto(sn.CreatedBy), CreateTime: ts(sn.CreatedAt),
	}
}

func templateProto(t store.Template) *ownsandv1.Template {
	return &ownsandv1.Template{OrganizationId: t.OrganizationID, TemplateId: t.ID, Name: t.Name, SnapshotId: t.SnapshotID, CreatedBy: principalProto(t.CreatedBy), CreateTime: ts(t.CreatedAt)}
}

func (s SnapshotService) CreateSnapshot(ctx context.Context, req *connect.Request[ownsandv1.CreateSnapshotRequest]) (*connect.Response[ownsandv1.CreateSnapshotResponse], error) {
	p, _, err := s.authorize(ctx, req.Msg.GetOrganizationId(), authz.ActionUseSandboxes)
	if err != nil {
		return nil, err
	}
	sb, err := s.Store.GetSandbox(ctx, req.Msg.GetOrganizationId(), req.Msg.GetSandboxId())
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFound("sandbox")
	}
	if err != nil {
		return nil, s.internal(ctx, "get_sandbox", err)
	}
	switch sb.State {
	case ownsandv1.SandboxState_SANDBOX_STATE_RUNNING, ownsandv1.SandboxState_SANDBOX_STATE_PAUSED:
	case ownsandv1.SandboxState_SANDBOX_STATE_UNREACHABLE:
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("the sandbox's device is offline"))
	default:
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("the sandbox is in "+sb.State.String()))
	}
	// A command needs a live agent channel before any record is created.
	if !s.Agents.IsLive(sb.DeviceID) {
		return nil, agent.ErrNoChannel
	}
	sn, err := s.Store.CreateSnapshotRecord(ctx, sb.OrganizationID, sb.ID, actorOf(p))
	if errors.Is(err, store.ErrConflict) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("the sandbox is not running or paused"))
	}
	if err != nil {
		return nil, s.internal(ctx, "create_snapshot", err)
	}
	if _, err := s.Agents.Command(ctx, sb.DeviceID, &ownsandv1.AgentCommand{Command: &ownsandv1.AgentCommand_CreateSnapshot{
		CreateSnapshot: &ownsandv1.CreateSnapshotCommand{SandboxId: sb.ID, SnapshotId: sn.ID},
	}}, 0); err != nil {
		// The record stays CREATING; the reconnection state report drops it
		// when the agent never took the snapshot.
		s.Logger.WarnContext(ctx, "snapshot_command_not_sent")
	}
	return connect.NewResponse(&ownsandv1.CreateSnapshotResponse{Snapshot: SnapshotProto(sn)}), nil
}

func (s SnapshotService) GetSnapshot(ctx context.Context, req *connect.Request[ownsandv1.GetSnapshotRequest]) (*connect.Response[ownsandv1.GetSnapshotResponse], error) {
	if _, _, err := s.authorize(ctx, req.Msg.GetOrganizationId(), authz.ActionUseSandboxes); err != nil {
		return nil, err
	}
	sn, err := s.Store.GetSnapshot(ctx, req.Msg.GetOrganizationId(), req.Msg.GetSnapshotId())
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFound("snapshot")
	}
	if err != nil {
		return nil, s.internal(ctx, "get_snapshot", err)
	}
	return connect.NewResponse(&ownsandv1.GetSnapshotResponse{Snapshot: SnapshotProto(sn)}), nil
}

func (s SnapshotService) ListSnapshots(ctx context.Context, req *connect.Request[ownsandv1.ListSnapshotsRequest]) (*connect.Response[ownsandv1.ListSnapshotsResponse], error) {
	if _, _, err := s.authorize(ctx, req.Msg.GetOrganizationId(), authz.ActionUseSandboxes); err != nil {
		return nil, err
	}
	pg, err := page(req.Msg.GetPageSize(), req.Msg.GetPageToken())
	if err != nil {
		return nil, err
	}
	list, err := s.Store.ListSnapshots(ctx, req.Msg.GetOrganizationId(), req.Msg.GetDeviceId(), pg)
	if err != nil {
		return nil, s.internal(ctx, "list_snapshots", err)
	}
	resp := &ownsandv1.ListSnapshotsResponse{}
	var ids []string
	for _, sn := range list {
		resp.Snapshots = append(resp.Snapshots, SnapshotProto(sn))
		ids = append(ids, sn.ID)
	}
	resp.NextPageToken = store.NextToken(ids, pg.Size)
	return connect.NewResponse(resp), nil
}

func (s SnapshotService) DeleteSnapshot(ctx context.Context, req *connect.Request[ownsandv1.DeleteSnapshotRequest]) (*connect.Response[ownsandv1.DeleteSnapshotResponse], error) {
	if _, _, err := s.authorize(ctx, req.Msg.GetOrganizationId(), authz.ActionUseSandboxes); err != nil {
		return nil, err
	}
	deviceID, err := s.Store.DeleteSnapshot(ctx, req.Msg.GetOrganizationId(), req.Msg.GetSnapshotId())
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, notFound("snapshot")
	case errors.Is(err, store.ErrReferenced):
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("a template references the snapshot"))
	case err != nil:
		return nil, s.internal(ctx, "delete_snapshot", err)
	}
	if deviceID != "" && s.Agents.IsLive(deviceID) {
		_, _ = s.Agents.Command(ctx, deviceID, &ownsandv1.AgentCommand{Command: &ownsandv1.AgentCommand_DeleteSnapshot{
			DeleteSnapshot: &ownsandv1.DeleteSnapshotCommand{SnapshotId: req.Msg.GetSnapshotId()},
		}}, 0)
	}
	return connect.NewResponse(&ownsandv1.DeleteSnapshotResponse{}), nil
}

func (s SnapshotService) ExportSnapshot(ctx context.Context, req *connect.Request[ownsandv1.ExportSnapshotRequest]) (*connect.Response[ownsandv1.ExportSnapshotResponse], error) {
	if _, _, err := s.authorize(ctx, req.Msg.GetOrganizationId(), authz.ActionUseSandboxes); err != nil {
		return nil, err
	}
	if !s.ExportConfigured {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("snapshot export storage is not configured"))
	}
	sn, err := s.Store.GetSnapshot(ctx, req.Msg.GetOrganizationId(), req.Msg.GetSnapshotId())
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFound("snapshot")
	}
	if err != nil {
		return nil, s.internal(ctx, "get_snapshot", err)
	}
	if sn.ExportState == ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTED || sn.ExportState == ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTING {
		return connect.NewResponse(&ownsandv1.ExportSnapshotResponse{Snapshot: SnapshotProto(sn)}), nil
	}
	if sn.State != ownsandv1.SnapshotState_SNAPSHOT_STATE_READY || sn.DeviceID == "" {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("the snapshot is not ready on a device"))
	}
	if !s.Agents.IsLive(sn.DeviceID) {
		return nil, agent.ErrNoChannel
	}
	sn, changed, err := s.Store.StartExport(ctx, req.Msg.GetOrganizationId(), req.Msg.GetSnapshotId())
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, notFound("snapshot")
	case errors.Is(err, store.ErrConflict):
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("the snapshot is not ready on a device"))
	case err != nil:
		return nil, s.internal(ctx, "export_snapshot", err)
	}
	if changed {
		if _, err := s.Agents.Command(ctx, sn.DeviceID, &ownsandv1.AgentCommand{Command: &ownsandv1.AgentCommand_ExportSnapshot{
			ExportSnapshot: &ownsandv1.ExportSnapshotCommand{SnapshotId: sn.ID},
		}}, 0); err != nil {
			s.Logger.WarnContext(ctx, "export_command_not_sent")
		}
	}
	return connect.NewResponse(&ownsandv1.ExportSnapshotResponse{Snapshot: SnapshotProto(sn)}), nil
}

func (s SnapshotService) CreateTemplate(ctx context.Context, req *connect.Request[ownsandv1.CreateTemplateRequest]) (*connect.Response[ownsandv1.CreateTemplateResponse], error) {
	p, _, err := s.authorize(ctx, req.Msg.GetOrganizationId(), authz.ActionUseSandboxes)
	if err != nil {
		return nil, err
	}
	if err := validName("name", req.Msg.GetName(), true); err != nil {
		return nil, err
	}
	t, err := s.Store.CreateTemplate(ctx, req.Msg.GetOrganizationId(), req.Msg.GetSnapshotId(), req.Msg.GetName(), actorOf(p))
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, notFound("snapshot")
	case errors.Is(err, store.ErrConflict):
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("the snapshot is not ready"))
	case errors.Is(err, store.ErrAlreadyExists):
		return nil, connect.NewError(connect.CodeAlreadyExists, errors.New("a template with this name exists"))
	case err != nil:
		return nil, s.internal(ctx, "create_template", err)
	}
	return connect.NewResponse(&ownsandv1.CreateTemplateResponse{Template: templateProto(t)}), nil
}

func (s SnapshotService) ListTemplates(ctx context.Context, req *connect.Request[ownsandv1.ListTemplatesRequest]) (*connect.Response[ownsandv1.ListTemplatesResponse], error) {
	if _, _, err := s.authorize(ctx, req.Msg.GetOrganizationId(), authz.ActionUseSandboxes); err != nil {
		return nil, err
	}
	pg, err := page(req.Msg.GetPageSize(), req.Msg.GetPageToken())
	if err != nil {
		return nil, err
	}
	list, err := s.Store.ListTemplates(ctx, req.Msg.GetOrganizationId(), pg)
	if err != nil {
		return nil, s.internal(ctx, "list_templates", err)
	}
	resp := &ownsandv1.ListTemplatesResponse{}
	var ids []string
	for _, t := range list {
		resp.Templates = append(resp.Templates, templateProto(t))
		ids = append(ids, t.ID)
	}
	resp.NextPageToken = store.NextToken(ids, pg.Size)
	return connect.NewResponse(resp), nil
}

func (s SnapshotService) DeleteTemplate(ctx context.Context, req *connect.Request[ownsandv1.DeleteTemplateRequest]) (*connect.Response[ownsandv1.DeleteTemplateResponse], error) {
	if _, _, err := s.authorize(ctx, req.Msg.GetOrganizationId(), authz.ActionUseSandboxes); err != nil {
		return nil, err
	}
	err := s.Store.DeleteTemplate(ctx, req.Msg.GetOrganizationId(), req.Msg.GetTemplateId())
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFound("template")
	}
	if err != nil {
		return nil, s.internal(ctx, "delete_template", err)
	}
	return connect.NewResponse(&ownsandv1.DeleteTemplateResponse{}), nil
}
