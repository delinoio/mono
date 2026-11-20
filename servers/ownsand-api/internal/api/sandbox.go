// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/delinoio/mono/packages/ownsand-sdk-go/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
	"github.com/delinoio/mono/servers/ownsand-api/internal/agent"
	"github.com/delinoio/mono/servers/ownsand-api/internal/authz"
	"github.com/delinoio/mono/servers/ownsand-api/internal/scheduler"
	"github.com/delinoio/mono/servers/ownsand-api/internal/store"
)

// Limits of the create request.
const (
	MaxTTL          = 7 * 24 * time.Hour
	MaxLabels       = 32
	MaxLabelKey     = 63
	MaxLabelValue   = 255
	ResultWait      = 60 * time.Second
	statePollPeriod = time.Second
)

// SandboxService implements ownsandv1connect.SandboxServiceHandler.
type SandboxService struct {
	*Server
	Agents *agent.Hub
}

var _ ownsandv1connect.SandboxServiceHandler = SandboxService{}

// SandboxProto converts a sandbox row.
func SandboxProto(sb store.Sandbox) *ownsandv1.Sandbox {
	src := &ownsandv1.SandboxSource{}
	switch {
	case sb.SourceTemplate != "":
		src.Source = &ownsandv1.SandboxSource_TemplateId{TemplateId: sb.SourceTemplate}
	case sb.SourceSnapshot != "":
		src.Source = &ownsandv1.SandboxSource_SnapshotId{SnapshotId: sb.SourceSnapshot}
	default:
		src.Source = &ownsandv1.SandboxSource_BaseImage{BaseImage: sb.BaseImage}
	}
	return &ownsandv1.Sandbox{
		OrganizationId: sb.OrganizationID, SandboxId: sb.ID, DeviceId: sb.DeviceID, State: sb.State, LastKnownState: sb.LastKnownState,
		EndReason: sb.EndReason, Source: src, GuestOs: sb.GuestOS, Arch: sb.Arch,
		Resources: &ownsandv1.Resources{VcpuCount: sb.Resources.VCPUCount, MemoryMib: sb.Resources.MemoryMiB, DiskGib: sb.Resources.DiskGiB},
		Network:   &ownsandv1.NetworkPolicy{Mode: sb.NetworkMode},
		IdleTtl:   durationpb.New(sb.IdleTTL), MaxTtl: durationpb.New(sb.MaxTTL),
		IdleDeadline: timestamppb.New(sb.IdleDeadline), MaxDeadline: timestamppb.New(sb.MaxDeadline),
		Labels: sb.Labels, CreatedBy: principalProto(sb.CreatedBy), CreateTime: ts(sb.CreatedAt), EndTime: tsPtr(sb.EndedAt), RestorePath: sb.RestorePath,
	}
}

// ValidateCreate checks a create request and returns the store parameters
// (without organization, actor and request ID).
func ValidateCreate(req *ownsandv1.CreateSandboxRequest) (store.CreateSandboxParams, error) {
	var p store.CreateSandboxParams
	if _, err := datapath.ParseUUIDv7(req.GetIdempotencyKey()); err != nil {
		return p, invalid("idempotency_key must be a UUID v7")
	}
	p.IdempotencyKey = req.GetIdempotencyKey()
	switch src := req.GetSource().GetSource().(type) {
	case *ownsandv1.SandboxSource_BaseImage:
		if src.BaseImage == ownsandv1.BaseImage_BASE_IMAGE_UNSPECIFIED {
			return p, invalid("source.base_image is required")
		}
		p.BaseImage = src.BaseImage
	case *ownsandv1.SandboxSource_SnapshotId:
		if !store.ValidID(src.SnapshotId) {
			return p, invalid("source.snapshot_id must be a UUID")
		}
		p.SnapshotID = src.SnapshotId
	case *ownsandv1.SandboxSource_TemplateId:
		if !store.ValidID(src.TemplateId) {
			return p, invalid("source.template_id must be a UUID")
		}
		p.TemplateID = src.TemplateId
	default:
		return p, invalid("source is required")
	}
	r := req.GetResources()
	if r.GetVcpuCount() <= 0 || r.GetMemoryMib() <= 0 || r.GetDiskGib() <= 0 {
		return p, invalid("resources.vcpu_count, memory_mib and disk_gib are required and must be greater than zero")
	}
	p.Resources = store.Limits{VCPUCount: r.GetVcpuCount(), MemoryMiB: r.GetMemoryMib(), DiskGiB: r.GetDiskGib()}
	idle, max, err := ValidateTTL(req.GetIdleTtl(), req.GetMaxTtl())
	if err != nil {
		return p, err
	}
	p.IdleTTL, p.MaxTTL = idle, max
	p.NetworkMode = ownsandv1.NetworkMode_NETWORK_MODE_OUTBOUND
	if req.GetNetwork() != nil {
		switch req.GetNetwork().GetMode() {
		case ownsandv1.NetworkMode_NETWORK_MODE_OUTBOUND, ownsandv1.NetworkMode_NETWORK_MODE_NONE:
			p.NetworkMode = req.GetNetwork().GetMode()
		default:
			return p, invalid("network.mode is required when network is present")
		}
	}
	if pl := req.GetPlacement(); pl != nil {
		if pl.DeviceId != nil {
			if !store.ValidID(pl.GetDeviceId()) {
				return p, invalid("placement.device_id must be a UUID")
			}
			p.DeviceID = pl.GetDeviceId()
		}
		if pl.HostOs != nil {
			if pl.GetHostOs() == ownsandv1.HostOs_HOST_OS_UNSPECIFIED {
				return p, invalid("placement.host_os must not be unspecified")
			}
			v := pl.GetHostOs()
			p.HostOS = &v
		}
		if pl.Arch != nil {
			if pl.GetArch() == ownsandv1.Arch_ARCH_UNSPECIFIED {
				return p, invalid("placement.arch must not be unspecified")
			}
			v := pl.GetArch()
			p.Arch = &v
		}
	}
	if len(req.GetLabels()) > MaxLabels {
		return p, invalid("labels has more than 32 entries")
	}
	for k, v := range req.GetLabels() {
		if k == "" || len(k) > MaxLabelKey || len(v) > MaxLabelValue || !utf8.ValidString(k) || !utf8.ValidString(v) {
			return p, invalid("label keys must have 1 to 63 bytes and values at most 255 bytes")
		}
	}
	p.Labels = req.GetLabels()
	if p.Labels == nil {
		p.Labels = map[string]string{}
	}
	// The idempotency hash covers every parameter except the key itself.
	clone := proto.Clone(req).(*ownsandv1.CreateSandboxRequest)
	clone.IdempotencyKey = ""
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(clone)
	if err != nil {
		return p, invalid("request cannot be encoded")
	}
	sum := sha256.Sum256(b)
	p.IdempotencyHash = sum[:]
	return p, nil
}

// ValidateTTL applies the TTL rules: both required and greater than zero,
// idle_ttl not greater than max_ttl, and max_ttl at most 7 days.
func ValidateTTL(idle, max *durationpb.Duration) (time.Duration, time.Duration, error) {
	if idle == nil || max == nil {
		return 0, 0, invalid("idle_ttl and max_ttl are required")
	}
	if idle.CheckValid() != nil || max.CheckValid() != nil {
		return 0, 0, invalid("idle_ttl and max_ttl must be valid durations")
	}
	i, m := idle.AsDuration(), max.AsDuration()
	switch {
	case i <= 0 || m <= 0:
		return 0, 0, invalid("idle_ttl and max_ttl must be greater than zero")
	case i > m:
		return 0, 0, invalid("idle_ttl must not be greater than max_ttl")
	case m > MaxTTL:
		return 0, 0, invalid("max_ttl must not be greater than 7 days")
	}
	return i, m, nil
}

func (s SandboxService) CreateSandbox(ctx context.Context, req *connect.Request[ownsandv1.CreateSandboxRequest]) (*connect.Response[ownsandv1.CreateSandboxResponse], error) {
	p, _, err := s.authorize(ctx, req.Msg.GetOrganizationId(), authz.ActionUseSandboxes)
	if err != nil {
		return nil, err
	}
	params, err := ValidateCreate(req.Msg)
	if err != nil {
		return nil, err
	}
	params.OrganizationID = req.Msg.GetOrganizationId()
	params.Actor = actorOf(p)
	params.RequestID = requestID()
	// The idempotency key is looked up right after authorization, before
	// placement or the live-channel check.
	if existing, err := s.lookupIdempotent(ctx, params); err != nil || existing != nil {
		if err != nil {
			return nil, err
		}
		return connect.NewResponse(&ownsandv1.CreateSandboxResponse{Sandbox: SandboxProto(*existing)}), nil
	}
	res, err := s.Store.CreateSandbox(ctx, params, s.Agents.IsLive)
	switch {
	case errors.Is(err, store.ErrConflict):
		// A concurrent create with the same key won.
		existing, err := s.lookupIdempotent(ctx, params)
		if err != nil {
			return nil, err
		}
		if existing == nil {
			return nil, connect.NewError(connect.CodeAborted, errors.New("concurrent create; retry"))
		}
		return connect.NewResponse(&ownsandv1.CreateSandboxResponse{Sandbox: SandboxProto(*existing)}), nil
	case errors.Is(err, scheduler.ErrUnavailable), errors.Is(err, scheduler.ErrFailedPrecondition):
		return nil, err
	case errors.Is(err, store.ErrNotFound):
		return nil, notFound("source or device")
	case errors.Is(err, store.ErrSourceNotReady):
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("the source snapshot is not ready"))
	case errors.Is(err, store.ErrSnapshotTooLarge):
		return nil, invalid("resources.disk_gib must be at least the snapshot disk size")
	case err != nil:
		return nil, s.internal(ctx, "create_sandbox", err)
	}
	sb := res.Sandbox
	cmd := &ownsandv1.CreateSandboxCommand{
		SandboxId: sb.ID, Resources: &ownsandv1.Resources{VcpuCount: sb.Resources.VCPUCount, MemoryMib: sb.Resources.MemoryMiB, DiskGib: sb.Resources.DiskGiB},
		NetworkMode: sb.NetworkMode, IdleDeadline: timestamppb.New(sb.IdleDeadline), MaxDeadline: timestamppb.New(sb.MaxDeadline),
		IdleTtl: durationpb.New(sb.IdleTTL), Limits: &ownsandv1.DeviceLimits{VcpuCount: res.Limits.VCPUCount, MemoryMib: res.Limits.MemoryMiB, DiskGib: res.Limits.DiskGiB},
		MacosMachineIdentifier: sb.MachineID, TryMemoryRestore: res.MemoryRestore,
	}
	if res.SnapshotID != "" {
		cmd.Source = &ownsandv1.SandboxSource{Source: &ownsandv1.SandboxSource_SnapshotId{SnapshotId: res.SnapshotID}}
	} else {
		cmd.Source = &ownsandv1.SandboxSource{Source: &ownsandv1.SandboxSource_BaseImage{BaseImage: params.BaseImage}}
	}
	if im := res.Import; im != nil {
		cmd.Import = &ownsandv1.ImportSnapshotCommand{SnapshotId: im.ID, PartSha256: im.PartSHA256, SizeBytes: im.SizeBytes, GuestOs: im.GuestOS, MacosHardwareModel: im.MacOSHardwareModel}
	}
	if _, err := s.Agents.Command(ctx, sb.DeviceID, &ownsandv1.AgentCommand{Command: &ownsandv1.AgentCommand_CreateSandbox{CreateSandbox: cmd}}, 0); err != nil {
		// The channel closed after the reservation: the sandbox stays
		// CREATING, and the reconnection state report ends it as
		// LOST_ON_DEVICE when the agent never received the command.
		s.Logger.WarnContext(ctx, "create_command_not_sent", slog.String("sandbox_id", sb.ID), slog.String("device_id", sb.DeviceID))
	}
	return connect.NewResponse(&ownsandv1.CreateSandboxResponse{Sandbox: SandboxProto(sb)}), nil
}

func (s SandboxService) lookupIdempotent(ctx context.Context, p store.CreateSandboxParams) (*store.Sandbox, error) {
	sb, err := s.Store.LookupIdempotency(ctx, p.OrganizationID, p.IdempotencyKey, p.IdempotencyHash)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, nil
	case errors.Is(err, store.ErrIdempotencyMismatch):
		return nil, connect.NewError(connect.CodeAlreadyExists, errors.New("idempotency_key was used with different parameters"))
	case err != nil:
		return nil, s.internal(ctx, "lookup_idempotency", err)
	}
	return &sb, nil
}

func (s SandboxService) get(ctx context.Context, org, id string) (store.Sandbox, error) {
	sb, err := s.Store.GetSandbox(ctx, org, id)
	if errors.Is(err, store.ErrNotFound) {
		return sb, notFound("sandbox")
	}
	if err != nil {
		return sb, s.internal(ctx, "get_sandbox", err)
	}
	return sb, nil
}

func (s SandboxService) GetSandbox(ctx context.Context, req *connect.Request[ownsandv1.GetSandboxRequest]) (*connect.Response[ownsandv1.GetSandboxResponse], error) {
	if _, _, err := s.authorize(ctx, req.Msg.GetOrganizationId(), authz.ActionUseSandboxes); err != nil {
		return nil, err
	}
	sb, err := s.get(ctx, req.Msg.GetOrganizationId(), req.Msg.GetSandboxId())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&ownsandv1.GetSandboxResponse{Sandbox: SandboxProto(sb)}), nil
}

func (s SandboxService) ListSandboxes(ctx context.Context, req *connect.Request[ownsandv1.ListSandboxesRequest]) (*connect.Response[ownsandv1.ListSandboxesResponse], error) {
	if _, _, err := s.authorize(ctx, req.Msg.GetOrganizationId(), authz.ActionUseSandboxes); err != nil {
		return nil, err
	}
	pg, err := page(req.Msg.GetPageSize(), req.Msg.GetPageToken())
	if err != nil {
		return nil, err
	}
	for _, st := range req.Msg.GetStates() {
		if st == ownsandv1.SandboxState_SANDBOX_STATE_UNSPECIFIED {
			return nil, invalid("states must not contain SANDBOX_STATE_UNSPECIFIED")
		}
	}
	list, err := s.Store.ListSandboxes(ctx, req.Msg.GetOrganizationId(), req.Msg.GetDeviceId(), req.Msg.GetStates(), pg)
	if err != nil {
		return nil, s.internal(ctx, "list_sandboxes", err)
	}
	resp := &ownsandv1.ListSandboxesResponse{}
	var ids []string
	for _, sb := range list {
		resp.Sandboxes = append(resp.Sandboxes, SandboxProto(sb))
		ids = append(ids, sb.ID)
	}
	resp.NextPageToken = store.NextToken(ids, pg.Size)
	return connect.NewResponse(resp), nil
}

// waitFor waits up to ResultWait for done to hold and returns the current
// sandbox.
func (s SandboxService) waitFor(ctx context.Context, org, id string, done func(store.Sandbox) bool) (store.Sandbox, error) {
	deadline := time.NewTimer(ResultWait)
	defer deadline.Stop()
	for {
		changes := s.Agents.Changes()
		sb, err := s.get(ctx, org, id)
		if err != nil || done(sb) {
			return sb, err
		}
		select {
		case <-changes:
		case <-time.After(statePollPeriod):
		case <-deadline.C:
			return s.get(ctx, org, id)
		case <-ctx.Done():
			return sb, ctx.Err()
		}
	}
}

func (s SandboxService) DeleteSandbox(ctx context.Context, req *connect.Request[ownsandv1.DeleteSandboxRequest]) (*connect.Response[ownsandv1.DeleteSandboxResponse], error) {
	p, _, err := s.authorize(ctx, req.Msg.GetOrganizationId(), authz.ActionUseSandboxes)
	if err != nil {
		return nil, err
	}
	sb, err := s.Store.RequestDelete(ctx, req.Msg.GetOrganizationId(), req.Msg.GetSandboxId(), actorOf(p), requestID())
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFound("sandbox")
	}
	if err != nil {
		return nil, s.internal(ctx, "delete_sandbox", err)
	}
	if !store.Terminal(sb.State) && s.Agents.IsLive(sb.DeviceID) {
		sb, err = s.waitFor(ctx, sb.OrganizationID, sb.ID, func(x store.Sandbox) bool { return store.Terminal(x.State) })
		if err != nil {
			return nil, err
		}
	}
	return connect.NewResponse(&ownsandv1.DeleteSandboxResponse{Sandbox: SandboxProto(sb)}), nil
}

// stateCheck applies the pause and resume state rules.
func stateCheck(sb store.Sandbox, want, done ownsandv1.SandboxState) (bool, error) {
	switch sb.State {
	case done:
		return true, nil
	case want:
		return false, nil
	case ownsandv1.SandboxState_SANDBOX_STATE_UNREACHABLE:
		return false, connect.NewError(connect.CodeUnavailable, errors.New("the sandbox's device is offline"))
	default:
		return false, connect.NewError(connect.CodeFailedPrecondition, errors.New("the sandbox is in "+sb.State.String()))
	}
}

func agentError(res *ownsandv1.AgentCommandResult) error {
	if res == nil || res.GetErrorCode() == "" {
		return nil
	}
	var code connect.Code
	if err := code.UnmarshalText([]byte(res.GetErrorCode())); err != nil {
		code = connect.CodeUnknown
	}
	return connect.NewError(code, errors.New("the device agent rejected the command"))
}

func (s SandboxService) pauseResume(ctx context.Context, org, id string, pause bool) (store.Sandbox, error) {
	if _, _, err := s.authorize(ctx, org, authz.ActionUseSandboxes); err != nil {
		return store.Sandbox{}, err
	}
	sb, err := s.get(ctx, org, id)
	if err != nil {
		return sb, err
	}
	want, done := ownsandv1.SandboxState_SANDBOX_STATE_RUNNING, ownsandv1.SandboxState_SANDBOX_STATE_PAUSED
	cmd := &ownsandv1.AgentCommand{Command: &ownsandv1.AgentCommand_PauseSandbox{PauseSandbox: &ownsandv1.PauseSandboxCommand{SandboxId: id}}}
	if !pause {
		want, done = done, want
		cmd = &ownsandv1.AgentCommand{Command: &ownsandv1.AgentCommand_ResumeSandbox{ResumeSandbox: &ownsandv1.ResumeSandboxCommand{SandboxId: id}}}
	}
	finished, err := stateCheck(sb, want, done)
	if err != nil || finished {
		return sb, err
	}
	res, err := s.Agents.Command(ctx, sb.DeviceID, cmd, ResultWait)
	if err != nil {
		return sb, err
	}
	if err := agentError(res); err != nil {
		return sb, err
	}
	if res == nil {
		return s.get(ctx, org, id)
	}
	// The result carries the new state; otherwise wait for the update.
	return s.waitFor(ctx, org, id, func(x store.Sandbox) bool { return x.State != want })
}

func (s SandboxService) PauseSandbox(ctx context.Context, req *connect.Request[ownsandv1.PauseSandboxRequest]) (*connect.Response[ownsandv1.PauseSandboxResponse], error) {
	sb, err := s.pauseResume(ctx, req.Msg.GetOrganizationId(), req.Msg.GetSandboxId(), true)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&ownsandv1.PauseSandboxResponse{Sandbox: SandboxProto(sb)}), nil
}

func (s SandboxService) ResumeSandbox(ctx context.Context, req *connect.Request[ownsandv1.ResumeSandboxRequest]) (*connect.Response[ownsandv1.ResumeSandboxResponse], error) {
	sb, err := s.pauseResume(ctx, req.Msg.GetOrganizationId(), req.Msg.GetSandboxId(), false)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&ownsandv1.ResumeSandboxResponse{Sandbox: SandboxProto(sb)}), nil
}
