// SPDX-License-Identifier: FSL-1.1-ALv2

// Package profile implements RunnerProfileService: the validation and
// storage of runner profiles. The owning contract is the Runner profiles
// section of docs/servers-tide-api-foundation.md. The work layer reads
// profiles with Profiles.Get and List and registers a deletion hook that ends
// the profile's queued requests and deletes its idle runners.
package profile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/durationpb"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/protos/delino/tide/v1/tidev1connect"
	"github.com/delinoio/mono/servers/tide-api/internal/api"
	"github.com/delinoio/mono/servers/tide-api/internal/app"
	"github.com/delinoio/mono/servers/tide-api/internal/authz"
	"github.com/delinoio/mono/servers/tide-api/internal/db"
	"github.com/delinoio/mono/servers/tide-api/internal/integration"
	"github.com/delinoio/mono/servers/tide-api/internal/link"
)

// Resources are the sandbox resources of a profile.
type Resources struct {
	VCPUCount int32
	MemoryMiB int64
	DiskGiB   int64
}

// Profile is a stored runner profile.
type Profile struct {
	ID                 string
	OrganizationID     string
	Name               string
	Kind               tidev1.WorkloadKind
	AgentIntegrationID string
	WorkspacePath      string
	GuestOS            tidev1.GuestOs
	// Exactly one of BaseImage (a guest OS) and TemplateID is set.
	BaseImage    tidev1.GuestOs
	TemplateID   string
	Resources    Resources
	NetworkMode  tidev1.NetworkMode
	IdleTTL      time.Duration
	MaxTTL       time.Duration
	Priority     tidev1.Priority
	MaxQueueWait time.Duration
	WarmCount    int32
	CreateTime   time.Time
	UpdateTime   time.Time
	WarmRevision string // Changes with every profile update.
}

// GitHubLabel is the `runs-on` label of a GITHUB_JOB profile.
func (p *Profile) GitHubLabel() string {
	if p.Kind != tidev1.WorkloadKind_WORKLOAD_KIND_GITHUB_JOB {
		return ""
	}
	return "tide-" + p.Name
}

// Proto returns the API form.
func (p *Profile) Proto() *tidev1.RunnerProfile {
	out := &tidev1.RunnerProfile{
		RunnerProfileId: p.ID, OrganizationId: p.OrganizationID, Name: p.Name, WorkloadKind: p.Kind,
		AgentIntegrationId: p.AgentIntegrationID, WorkspacePath: p.WorkspacePath, GuestOs: p.GuestOS,
		Resources:   &tidev1.Resources{VcpuCount: p.Resources.VCPUCount, MemoryMib: p.Resources.MemoryMiB, DiskGib: p.Resources.DiskGiB},
		NetworkMode: p.NetworkMode, IdleTtl: durationpb.New(p.IdleTTL), MaxTtl: durationpb.New(p.MaxTTL), Priority: p.Priority,
		MaxQueueWait: durationpb.New(p.MaxQueueWait), WarmCount: p.WarmCount, GithubLabel: p.GitHubLabel(),
		CreateTime: api.Timestamp(p.CreateTime), UpdateTime: api.Timestamp(p.UpdateTime),
	}
	if p.TemplateID != "" {
		out.Source = &tidev1.RunnerProfile_OwnsandTemplateId{OwnsandTemplateId: p.TemplateID}
	} else {
		out.Source = &tidev1.RunnerProfile_BaseImage{BaseImage: p.BaseImage}
	}
	return out
}

// OwnSandBaseImage returns the OwnSand base image of a base image source.
func OwnSandBaseImage(g tidev1.GuestOs) ownsandv1.BaseImage {
	switch g {
	case tidev1.GuestOs_GUEST_OS_LINUX:
		return ownsandv1.BaseImage_BASE_IMAGE_LINUX
	case tidev1.GuestOs_GUEST_OS_MACOS:
		return ownsandv1.BaseImage_BASE_IMAGE_MACOS
	case tidev1.GuestOs_GUEST_OS_WINDOWS:
		return ownsandv1.BaseImage_BASE_IMAGE_WINDOWS
	}
	return ownsandv1.BaseImage_BASE_IMAGE_UNSPECIFIED
}

// OwnSandGuestOS converts a guest OS.
func OwnSandGuestOS(g tidev1.GuestOs) ownsandv1.GuestOs { return ownsandv1.GuestOs(g) }

// DeletionHook lets the work layer clean up when DeleteRunnerProfile runs:
// Prepare may call OwnSand (delete the warm runners and the runners that wait
// for work). InTx runs with the profile locked and can return best-effort
// work to run after commit. Busy runners finish their work.
type DeletionHook struct {
	Name    string
	Prepare func(ctx context.Context, orgID, profileID string) error
	InTx    func(ctx context.Context, tx pgx.Tx, orgID, profileID string) (afterCommit func(context.Context), err error)
}

// Profiles stores runner profiles.
type Profiles struct {
	svc          *api.Service
	links        *link.Links
	integrations *integration.Store
	rules        Rules
	hooks        []DeletionHook
}

// Register mounts RunnerProfileService with the validation rules.
func Register(r *app.Registry, links *link.Links, integrations *integration.Store, rules Rules) *Profiles {
	p := &Profiles{svc: r.Service, links: links, integrations: integrations, rules: rules}
	r.Handle(tidev1connect.NewRunnerProfileServiceHandler(&Handler{p: p}, r.HandlerOptions()...))
	return p
}

// OnDeletion registers a deletion hook.
func (ps *Profiles) OnDeletion(h DeletionHook) { ps.hooks = append(ps.hooks, h) }

const profileColumns = `runner_profile_id::text, organization_id::text, name, workload_kind, COALESCE(agent_integration_id::text, ''), workspace_path,
	guest_os, COALESCE(base_image, 0), COALESCE(ownsand_template_id::text, ''), vcpu_count, memory_mib, disk_gib, network_mode,
	idle_ttl_ms, max_ttl_ms, priority, max_queue_wait_ms, warm_count, create_time, update_time, warm_revision::text`

// Scan reads a row of profileColumns.
func Scan(row pgx.Row) (*Profile, error) {
	var p Profile
	var kind, guest, base, network, priority int16
	var idle, maxTTL, wait int64
	if err := row.Scan(&p.ID, &p.OrganizationID, &p.Name, &kind, &p.AgentIntegrationID, &p.WorkspacePath, &guest, &base, &p.TemplateID,
		&p.Resources.VCPUCount, &p.Resources.MemoryMiB, &p.Resources.DiskGiB, &network, &idle, &maxTTL, &priority, &wait, &p.WarmCount,
		&p.CreateTime, &p.UpdateTime, &p.WarmRevision); err != nil {
		return nil, err
	}
	p.Kind, p.GuestOS, p.BaseImage = tidev1.WorkloadKind(kind), tidev1.GuestOs(guest), tidev1.GuestOs(base)
	p.NetworkMode, p.Priority = tidev1.NetworkMode(network), tidev1.Priority(priority)
	p.IdleTTL, p.MaxTTL, p.MaxQueueWait = time.Duration(idle)*time.Millisecond, time.Duration(maxTTL)*time.Millisecond, time.Duration(wait)*time.Millisecond
	return &p, nil
}

// Columns is the column list that Scan reads, for joins of later layers.
const Columns = profileColumns

// Get loads a profile of the organization (not_found otherwise).
func (ps *Profiles) Get(ctx context.Context, q db.Querier, orgID, id string) (*Profile, error) {
	p, err := Scan(q.QueryRow(ctx, "SELECT "+profileColumns+" FROM runner_profiles WHERE organization_id = $1 AND runner_profile_id = $2", orgID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, api.ErrNotFound("runner profile")
	}
	return p, err
}

// LockShare holds a shared profile row lock until the transaction ends.
// The caller must lock the organization first, as DeleteRunnerProfile does.
// Deletion either finds the committed work or removes the profile before
// the caller can create it.
func LockShare(ctx context.Context, tx pgx.Tx, orgID, id string) error {
	var locked int
	err := tx.QueryRow(ctx, "SELECT 1 FROM runner_profiles WHERE organization_id = $1 AND runner_profile_id = $2 FOR SHARE", orgID, id).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return api.ErrNotFound("runner profile")
	}
	return err
}

// ListAll returns every profile of the organization.
func (ps *Profiles) ListAll(ctx context.Context, q db.Querier, orgID string) ([]*Profile, error) {
	rows, err := q.Query(ctx, "SELECT "+profileColumns+" FROM runner_profiles WHERE organization_id = $1 ORDER BY runner_profile_id", orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Profile
	for rows.Next() {
		p, err := Scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func durationField(field string, d interface {
	CheckValid() error
	AsDuration() time.Duration
}) (time.Duration, error) {
	if d == nil {
		return 0, nil
	}
	if err := d.CheckValid(); err != nil {
		return 0, api.ErrInvalidf("invalid %s: %v", field, err)
	}
	// The durations are stored in milliseconds, so a value with a smaller
	// part would read back as another value.
	if d.AsDuration()%time.Millisecond != 0 {
		return 0, api.ErrInvalidf("invalid %s: must be a whole number of milliseconds", field)
	}
	return d.AsDuration(), nil
}

// applyField copies the field of in that a mask path names into p.
func applyField(p *Profile, in *tidev1.RunnerProfile, path string) error {
	var err error
	switch path {
	case "name":
		p.Name = in.GetName()
	case "workspace_path":
		p.WorkspacePath = in.GetWorkspacePath()
	case "guest_os":
		p.GuestOS = in.GetGuestOs()
	case "source":
		p.BaseImage, p.TemplateID = tidev1.GuestOs_GUEST_OS_UNSPECIFIED, ""
		switch s := in.GetSource().(type) {
		case *tidev1.RunnerProfile_BaseImage:
			p.BaseImage = s.BaseImage
		case *tidev1.RunnerProfile_OwnsandTemplateId:
			p.TemplateID = s.OwnsandTemplateId
		}
	case "resources":
		r := in.GetResources()
		p.Resources = Resources{VCPUCount: r.GetVcpuCount(), MemoryMiB: r.GetMemoryMib(), DiskGiB: r.GetDiskGib()}
	case "resources.vcpu_count":
		p.Resources.VCPUCount = in.GetResources().GetVcpuCount()
	case "resources.memory_mib":
		p.Resources.MemoryMiB = in.GetResources().GetMemoryMib()
	case "resources.disk_gib":
		p.Resources.DiskGiB = in.GetResources().GetDiskGib()
	case "network_mode":
		p.NetworkMode = in.GetNetworkMode()
	case "idle_ttl":
		if in.GetIdleTtl() == nil {
			return api.ErrInvalidf("invalid idle_ttl: is required")
		}
		p.IdleTTL, err = durationField("idle_ttl", in.GetIdleTtl())
	case "max_ttl":
		if in.GetMaxTtl() == nil {
			return api.ErrInvalidf("invalid max_ttl: is required")
		}
		p.MaxTTL, err = durationField("max_ttl", in.GetMaxTtl())
	case "priority":
		p.Priority = in.GetPriority()
	case "max_queue_wait":
		p.MaxQueueWait, err = durationField("max_queue_wait", in.GetMaxQueueWait())
	case "warm_count":
		p.WarmCount = in.GetWarmCount()
	case "workload_kind", "agent_integration_id":
		return api.ErrInvalidf("invalid update_mask: %s cannot change after creation", path)
	default:
		return api.ErrInvalidf("invalid update_mask: unknown path %q", path)
	}
	return err
}

var settablePaths = []string{"name", "workspace_path", "guest_os", "source", "resources", "network_mode", "idle_ttl", "max_ttl", "priority", "max_queue_wait", "warm_count"}

// vendorOf returns the vendor of the profile's integration.
func (ps *Profiles) vendorOf(ctx context.Context, q db.Querier, p *Profile) (tidev1.AgentVendor, error) {
	if p.Kind != tidev1.WorkloadKind_WORKLOAD_KIND_AGENT_SESSION || p.AgentIntegrationID == "" {
		return tidev1.AgentVendor_AGENT_VENDOR_UNSPECIFIED, nil
	}
	id, err := api.NormalizeID("agent_integration_id", p.AgentIntegrationID)
	if err != nil {
		return 0, err
	}
	p.AgentIntegrationID = id
	in, err := ps.integrations.Get(ctx, q, p.OrganizationID, id)
	if err != nil {
		return 0, err
	}
	return in.Vendor, nil
}

// checkTemplate checks the guest OS of a template source with OwnSand when
// the organization has an ACTIVE link. Without one, the template is checked
// when a runner starts: OwnSand then fails the sandbox creation with
// failed_precondition and the request ends with NO_ELIGIBLE_DEVICE or the
// start fails.
func (ps *Profiles) checkTemplate(ctx context.Context, p *Profile) error {
	if p.TemplateID == "" {
		return nil
	}
	client, _, err := ps.links.Client(ctx, p.OrganizationID)
	if err != nil {
		if connect.CodeOf(err) == connect.CodeFailedPrecondition {
			return nil
		}
		return err
	}
	guest, err := client.TemplateGuestOS(ctx, p.TemplateID)
	switch {
	case err == nil:
	case connect.CodeOf(err) == connect.CodeNotFound:
		return api.ErrInvalidf("invalid source.ownsand_template_id: the linked OwnSand organization has no such template")
	case connect.CodeOf(err) == connect.CodeUnavailable || connect.CodeOf(err) == connect.CodeDeadlineExceeded:
		return api.ErrUnavailable("OwnSand is unavailable; try again")
	default:
		return fmt.Errorf("check template: %w", err)
	}
	if guest != OwnSandGuestOS(p.GuestOS) {
		return api.ErrInvalidf("invalid source.ownsand_template_id: the template's guest OS is not guest_os")
	}
	return nil
}

func (ps *Profiles) changed(ctx context.Context, orgID string) {
	ps.svc.WorkChanged(orgID)
}

// Handler serves RunnerProfileService.
type Handler struct{ p *Profiles }

var _ tidev1connect.RunnerProfileServiceHandler = (*Handler)(nil)

func (h *Handler) CreateRunnerProfile(ctx context.Context, req *connect.Request[tidev1.CreateRunnerProfileRequest]) (*connect.Response[tidev1.CreateRunnerProfileResponse], error) {
	ps := h.p
	s := ps.svc
	c, err := s.Authorize(ctx, s.DB.Pool, req.Msg.GetOrganizationId(), authz.ActionManageRunnerProfiles)
	if err != nil {
		return nil, err
	}
	in := req.Msg.GetProfile()
	if in == nil {
		return nil, api.ErrInvalidf("invalid profile: is required")
	}
	p := &Profile{OrganizationID: c.OrganizationID, Kind: in.GetWorkloadKind(), AgentIntegrationID: in.GetAgentIntegrationId()}
	for _, path := range settablePaths {
		if err := applyField(p, in, path); err != nil {
			return nil, err
		}
	}
	if p.Priority == tidev1.Priority_PRIORITY_UNSPECIFIED {
		p.Priority = DefaultPriority(p.Kind)
	}
	vendor, err := ps.vendorOf(ctx, s.DB.Pool, p)
	if err != nil {
		return nil, s.Internal(ctx, "create runner profile", err)
	}
	if err := ps.rules.Validate(p, vendor); err != nil {
		return nil, err
	}
	if p.TemplateID != "" {
		p.TemplateID, _ = api.NormalizeID("ownsand_template_id", p.TemplateID)
	}
	if err := ps.checkTemplate(ctx, p); err != nil {
		return nil, s.Internal(ctx, "create runner profile", err)
	}
	p.ID = api.NewID()
	now := s.NowUTC()
	p.CreateTime, p.UpdateTime = now, now
	err = s.DB.InTx(ctx, func(tx pgx.Tx) error {
		if err := api.LockOrganizationShare(ctx, tx, c.OrganizationID); err != nil {
			return err
		}
		if err := insert(ctx, tx, p); err != nil {
			return err
		}
		return s.WriteAudit(ctx, tx, api.AuditEvent{
			OrganizationID: c.OrganizationID, Action: tidev1.AuditAction_AUDIT_ACTION_RUNNER_PROFILE_CREATED, Actor: c.Principal,
			TargetKind: tidev1.AuditTargetKind_AUDIT_TARGET_KIND_RUNNER_PROFILE, TargetID: p.ID,
		})
	})
	if err != nil {
		return nil, s.Internal(ctx, "create runner profile", err)
	}
	ps.changed(ctx, c.OrganizationID)
	return connect.NewResponse(&tidev1.CreateRunnerProfileResponse{Profile: p.Proto()}), nil
}

func nullable[T comparable](v T) any {
	var zero T
	if v == zero {
		return nil
	}
	return v
}

func insert(ctx context.Context, tx pgx.Tx, p *Profile) error {
	_, err := tx.Exec(ctx, `INSERT INTO runner_profiles (runner_profile_id, organization_id, name, workload_kind, agent_integration_id, workspace_path, guest_os,
		base_image, ownsand_template_id, vcpu_count, memory_mib, disk_gib, network_mode, idle_ttl_ms, max_ttl_ms, priority, max_queue_wait_ms, warm_count, create_time, update_time)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20)`,
		p.ID, p.OrganizationID, p.Name, int16(p.Kind), nullable(p.AgentIntegrationID), p.WorkspacePath, int16(p.GuestOS),
		nullable(int16(p.BaseImage)), nullable(p.TemplateID), p.Resources.VCPUCount, p.Resources.MemoryMiB, p.Resources.DiskGiB, int16(p.NetworkMode),
		p.IdleTTL.Milliseconds(), p.MaxTTL.Milliseconds(), int16(p.Priority), p.MaxQueueWait.Milliseconds(), p.WarmCount, p.CreateTime, p.UpdateTime)
	if db.IsUniqueViolation(err) {
		return api.ErrAlreadyExists("a runner profile with this name exists")
	}
	return err
}

func (h *Handler) GetRunnerProfile(ctx context.Context, req *connect.Request[tidev1.GetRunnerProfileRequest]) (*connect.Response[tidev1.GetRunnerProfileResponse], error) {
	s := h.p.svc
	c, err := s.Authorize(ctx, s.DB.Pool, req.Msg.GetOrganizationId(), authz.ActionRead)
	if err != nil {
		return nil, err
	}
	id, err := api.NormalizeID("runner_profile_id", req.Msg.GetRunnerProfileId())
	if err != nil {
		return nil, err
	}
	p, err := h.p.Get(ctx, s.DB.Pool, c.OrganizationID, id)
	if err != nil {
		return nil, s.Internal(ctx, "get runner profile", err)
	}
	return connect.NewResponse(&tidev1.GetRunnerProfileResponse{Profile: p.Proto()}), nil
}

func (h *Handler) ListRunnerProfiles(ctx context.Context, req *connect.Request[tidev1.ListRunnerProfilesRequest]) (*connect.Response[tidev1.ListRunnerProfilesResponse], error) {
	s := h.p.svc
	c, err := s.Authorize(ctx, s.DB.Pool, req.Msg.GetOrganizationId(), authz.ActionRead)
	if err != nil {
		return nil, err
	}
	limit, after, err := api.PageArgs(req.Msg.GetPageSize(), req.Msg.GetPageToken())
	if err != nil {
		return nil, err
	}
	kind := req.Msg.GetWorkloadKind()
	if err := api.DefinedEnum("workload_kind", "WorkloadKind", kind, tidev1.WorkloadKind_name); err != nil {
		return nil, err
	}
	rows, err := s.DB.Pool.Query(ctx, "SELECT "+profileColumns+` FROM runner_profiles
		WHERE organization_id = $1 AND runner_profile_id > $2 AND ($3 = 0 OR workload_kind = $3) ORDER BY runner_profile_id LIMIT $4`,
		c.OrganizationID, api.AfterKey(after), int16(kind), limit)
	if err != nil {
		return nil, s.Internal(ctx, "list runner profiles", err)
	}
	defer rows.Close()
	resp := &tidev1.ListRunnerProfilesResponse{}
	last := ""
	for rows.Next() {
		p, err := Scan(rows)
		if err != nil {
			return nil, s.Internal(ctx, "list runner profiles", err)
		}
		resp.Profiles = append(resp.Profiles, p.Proto())
		last = p.ID
	}
	if err := rows.Err(); err != nil {
		return nil, s.Internal(ctx, "list runner profiles", err)
	}
	resp.NextPageToken = api.NextPageToken(len(resp.Profiles), limit, last)
	return connect.NewResponse(resp), nil
}

// UpdateRunnerProfile applies a field mask and validates the whole result. A
// change applies to runners that start after it.
func (h *Handler) UpdateRunnerProfile(ctx context.Context, req *connect.Request[tidev1.UpdateRunnerProfileRequest]) (*connect.Response[tidev1.UpdateRunnerProfileResponse], error) {
	ps := h.p
	s := ps.svc
	c, err := s.Authorize(ctx, s.DB.Pool, req.Msg.GetOrganizationId(), authz.ActionManageRunnerProfiles)
	if err != nil {
		return nil, err
	}
	id, err := api.NormalizeID("runner_profile_id", req.Msg.GetRunnerProfileId())
	if err != nil {
		return nil, err
	}
	paths := req.Msg.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		return nil, api.ErrInvalidf("invalid update_mask: must name at least one path")
	}
	in := req.Msg.GetProfile()
	if in == nil {
		in = &tidev1.RunnerProfile{}
	}
	// The mask is applied to a snapshot outside the transaction, because the
	// template check may call OwnSand. The transaction locks the row and
	// writes only when the row still equals the snapshot; otherwise a
	// concurrent update changed it, and the mask is applied again to the
	// new row, so the update does not undo the other one.
	var p *Profile
	for attempt := 1; ; attempt++ {
		p, err = ps.applyUpdate(ctx, c, id, in, paths)
		if !errors.Is(err, errProfileChanged) {
			break
		}
		if attempt == maxUpdateAttempts {
			return nil, api.ErrPrecondition("the runner profile changed meanwhile; try again")
		}
	}
	if err != nil {
		return nil, s.Internal(ctx, "update runner profile", err)
	}
	ps.changed(ctx, c.OrganizationID)
	return connect.NewResponse(&tidev1.UpdateRunnerProfileResponse{Profile: p.Proto()}), nil
}

// maxUpdateAttempts bounds the retries of UpdateRunnerProfile after
// concurrent updates of the same profile.
const maxUpdateAttempts = 3

// errProfileChanged tells UpdateRunnerProfile that the row changed after it
// read its snapshot.
var errProfileChanged = errors.New("the runner profile changed meanwhile")

// sameRow reports whether two reads of a profile row are equal.
func sameRow(a, b *Profile) bool {
	x, y := *a, *b
	if !x.CreateTime.Equal(y.CreateTime) || !x.UpdateTime.Equal(y.UpdateTime) {
		return false
	}
	x.CreateTime, x.UpdateTime, y.CreateTime, y.UpdateTime = time.Time{}, time.Time{}, time.Time{}, time.Time{}
	return x == y
}

// applyUpdate reads the profile, applies the mask, validates the result and
// writes it. It returns errProfileChanged when the row changed between the
// read and the write.
func (ps *Profiles) applyUpdate(ctx context.Context, c api.Caller, id string, in *tidev1.RunnerProfile, paths []string) (*Profile, error) {
	s := ps.svc
	p, err := ps.Get(ctx, s.DB.Pool, c.OrganizationID, id)
	if err != nil {
		return nil, err
	}
	snapshot := *p
	oldTemplate := p.TemplateID
	for _, path := range paths {
		if err := applyField(p, in, path); err != nil {
			return nil, err
		}
	}
	vendor, err := ps.vendorOf(ctx, s.DB.Pool, p)
	if err != nil {
		return nil, err
	}
	if err := ps.rules.Validate(p, vendor); err != nil {
		return nil, err
	}
	if p.TemplateID != "" {
		p.TemplateID, _ = api.NormalizeID("ownsand_template_id", p.TemplateID)
	}
	if p.TemplateID != oldTemplate || contains(paths, "guest_os") {
		if err := ps.checkTemplate(ctx, p); err != nil {
			return nil, err
		}
	}
	p.UpdateTime = s.NowUTC()
	err = s.DB.InTx(ctx, func(tx pgx.Tx) error {
		if err := api.LockOrganizationShare(ctx, tx, c.OrganizationID); err != nil {
			return err
		}
		current, err := Scan(tx.QueryRow(ctx, "SELECT "+profileColumns+" FROM runner_profiles WHERE organization_id = $1 AND runner_profile_id = $2 FOR UPDATE", c.OrganizationID, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return api.ErrNotFound("runner profile")
		}
		if err != nil {
			return err
		}
		if !sameRow(current, &snapshot) {
			return errProfileChanged
		}
		_, err = tx.Exec(ctx, `UPDATE runner_profiles SET name = $3, workspace_path = $4, guest_os = $5, base_image = $6, ownsand_template_id = $7,
			vcpu_count = $8, memory_mib = $9, disk_gib = $10, network_mode = $11, idle_ttl_ms = $12, max_ttl_ms = $13, priority = $14,
			max_queue_wait_ms = $15, warm_count = $16, update_time = $17, warm_revision = gen_random_uuid(),
			warm_retry_link_revision = NULL, warm_failures = 0, warm_create_rejections = 0, warm_retry_after = NULL
			WHERE organization_id = $1 AND runner_profile_id = $2`,
			c.OrganizationID, id, p.Name, p.WorkspacePath, int16(p.GuestOS), nullable(int16(p.BaseImage)), nullable(p.TemplateID),
			p.Resources.VCPUCount, p.Resources.MemoryMiB, p.Resources.DiskGiB, int16(p.NetworkMode), p.IdleTTL.Milliseconds(), p.MaxTTL.Milliseconds(),
			int16(p.Priority), p.MaxQueueWait.Milliseconds(), p.WarmCount, p.UpdateTime)
		if db.IsUniqueViolation(err) {
			return api.ErrAlreadyExists("a runner profile with this name exists")
		}
		if err != nil {
			return err
		}
		return s.WriteAudit(ctx, tx, api.AuditEvent{
			OrganizationID: c.OrganizationID, Action: tidev1.AuditAction_AUDIT_ACTION_RUNNER_PROFILE_UPDATED, Actor: c.Principal,
			TargetKind: tidev1.AuditTargetKind_AUDIT_TARGET_KIND_RUNNER_PROFILE, TargetID: id,
		})
	})
	if err != nil {
		return nil, err
	}
	return p, nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// DeleteRunnerProfile runs the deletion hooks of the work layer and deletes
// the profile.
func (h *Handler) DeleteRunnerProfile(ctx context.Context, req *connect.Request[tidev1.DeleteRunnerProfileRequest]) (*connect.Response[tidev1.DeleteRunnerProfileResponse], error) {
	ps := h.p
	s := ps.svc
	c, err := s.Authorize(ctx, s.DB.Pool, req.Msg.GetOrganizationId(), authz.ActionManageRunnerProfiles)
	if err != nil {
		return nil, err
	}
	id, err := api.NormalizeID("runner_profile_id", req.Msg.GetRunnerProfileId())
	if err != nil {
		return nil, err
	}
	if _, err := ps.Get(ctx, s.DB.Pool, c.OrganizationID, id); err != nil {
		return nil, s.Internal(ctx, "delete runner profile", err)
	}
	for _, hook := range ps.hooks {
		if hook.Prepare == nil {
			continue
		}
		if err := hook.Prepare(ctx, c.OrganizationID, id); err != nil {
			s.Logger.WarnContext(ctx, "runner_profile_deletion_prepare_failed", slog.String("hook", hook.Name), slog.String("runner_profile_id", id), slog.Any("error", err))
			return nil, s.Internal(ctx, "prepare runner profile deletion", err)
		}
	}
	var afterCommit []func(context.Context)
	err = s.DB.InTx(ctx, func(tx pgx.Tx) error {
		if err := api.LockOrganizationShare(ctx, tx, c.OrganizationID); err != nil {
			return err
		}
		var locked int
		if err := tx.QueryRow(ctx, "SELECT 1 FROM runner_profiles WHERE organization_id = $1 AND runner_profile_id = $2 FOR UPDATE", c.OrganizationID, id).Scan(&locked); errors.Is(err, pgx.ErrNoRows) {
			return api.ErrNotFound("runner profile")
		} else if err != nil {
			return err
		}
		for _, hook := range ps.hooks {
			if hook.InTx == nil {
				continue
			}
			after, err := hook.InTx(ctx, tx, c.OrganizationID, id)
			if err != nil {
				return fmt.Errorf("runner profile deletion hook %s: %w", hook.Name, err)
			}
			if after != nil {
				afterCommit = append(afterCommit, after)
			}
		}
		if _, err := tx.Exec(ctx, "DELETE FROM runner_profiles WHERE runner_profile_id = $1", id); err != nil {
			return err
		}
		return s.WriteAudit(ctx, tx, api.AuditEvent{
			OrganizationID: c.OrganizationID, Action: tidev1.AuditAction_AUDIT_ACTION_RUNNER_PROFILE_DELETED, Actor: c.Principal,
			TargetKind: tidev1.AuditTargetKind_AUDIT_TARGET_KIND_RUNNER_PROFILE, TargetID: id,
		})
	})
	if err != nil {
		return nil, s.Internal(ctx, "delete runner profile", err)
	}
	for _, after := range afterCommit {
		after(context.WithoutCancel(ctx))
	}
	ps.changed(ctx, c.OrganizationID)
	return connect.NewResponse(&tidev1.DeleteRunnerProfileResponse{}), nil
}
