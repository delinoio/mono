// SPDX-License-Identifier: Apache-2.0

package sandruntest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/delinoio/mono/go/uuidv7"
	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/protos/delino/sandrun/v1/sandrunv1connect"
)

// AddOrganization adds an organization whose owner is the server's user.
func (s *Server) AddOrganization(name string) *sandrunv1.Organization {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addOrgLocked(name)
}

func (s *Server) addOrgLocked(name string) *sandrunv1.Organization {
	o := &sandrunv1.Organization{
		OrganizationId: s.newID(),
		Name:           name,
		CreateTime:     timestamppb.Now(),
		CallerRole:     sandrunv1.Role_ROLE_OWNER,
		Settings:       &sandrunv1.OrganizationSettings{},
	}
	s.orgs = append(s.orgs, o)
	s.members[o.GetOrganizationId()] = []*sandrunv1.Member{{
		OrganizationId: o.GetOrganizationId(), UserId: s.UserID, Role: sandrunv1.Role_ROLE_OWNER,
		DisplayName: "Test User", JoinTime: timestamppb.Now(),
	}}
	return o
}

// Organizations returns copies of the organizations.
func (s *Server) Organizations() []*sandrunv1.Organization {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*sandrunv1.Organization, 0, len(s.orgs))
	for _, o := range s.orgs {
		out = append(out, proto.CloneOf(o))
	}
	return out
}

// AddDevSessionProfile adds a DEV_SESSION runner profile. workspace is a
// local absolute directory (see the package comment).
func (s *Server) AddDevSessionProfile(orgID, name string, os sandrunv1.GuestOs, workspace string) *sandrunv1.RunnerProfile {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := &sandrunv1.RunnerProfile{
		RunnerProfileId: s.newID(),
		OrganizationId:  orgID,
		Name:            name,
		WorkloadKind:    sandrunv1.WorkloadKind_WORKLOAD_KIND_DEV_SESSION,
		WorkspacePath:   workspace,
		GuestOs:         os,
		Source:          &sandrunv1.RunnerProfile_BaseImage{BaseImage: os},
		Resources:       &sandrunv1.Resources{VcpuCount: 2, MemoryMib: 4096, DiskGib: 32},
		NetworkMode:     sandrunv1.NetworkMode_NETWORK_MODE_OUTBOUND,
		IdleTtl:         durationpb.New(time.Hour),
		MaxTtl:          durationpb.New(24 * time.Hour),
		Priority:        sandrunv1.Priority_PRIORITY_NORMAL,
		MaxQueueWait:    durationpb.New(30 * time.Minute),
		CreateTime:      timestamppb.Now(),
		UpdateTime:      timestamppb.Now(),
	}
	s.profiles = append(s.profiles, p)
	return proto.CloneOf(p)
}

// AddRequest and AddRunner seed the work lists and the queue snapshot.
func (s *Server) AddRequest(r *sandrunv1.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, proto.CloneOf(r))
}

// AddRunner seeds a runner.
func (s *Server) AddRunner(r *sandrunv1.Runner) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runners = append(s.runners, proto.CloneOf(r))
}

// AddDevice seeds the capacity view.
func (s *Server) AddDevice(d *sandrunv1.DeviceCapacity) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.devices = append(s.devices, proto.CloneOf(d))
}

const queueBufferSize = 64

// SendQueueEvent queues a request or runner update for one WatchQueue stream
// of the event's organization. Updates with no organization ID are ignored.
// Other event types can go to any one WatchQueue stream.
func (s *Server) SendQueueEvent(ev *sandrunv1.WatchQueueResponse) {
	var orgID string
	switch event := ev.GetEvent().(type) {
	case *sandrunv1.WatchQueueResponse_Request:
		if event != nil {
			orgID = event.Request.GetOrganizationId()
		}
	case *sandrunv1.WatchQueueResponse_Runner:
		if event != nil {
			orgID = event.Runner.GetOrganizationId()
		}
	default:
		s.unscopedQueue <- ev
		return
	}
	if orgID != "" {
		s.queueForOrganization(orgID) <- proto.CloneOf(ev)
	}
}

func (s *Server) queueForOrganization(orgID string) chan *sandrunv1.WatchQueueResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	queue := s.queueByOrg[orgID]
	if queue == nil {
		queue = make(chan *sandrunv1.WatchQueueResponse, queueBufferSize)
		s.queueByOrg[orgID] = queue
	}
	return queue
}

// OwnSandLink returns the stored link of an organization and the API key
// that SetOwnSandLink received, for credential input tests.
func (s *Server) OwnSandLink(orgID string) (*sandrunv1.OwnSandLink, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.links[orgID]
	if l == nil {
		return nil, ""
	}
	return proto.CloneOf(l), s.linkKeys[orgID]
}

// Integrations returns the agent integrations with the credentials of
// their last create or update request.
func (s *Server) Integrations() ([]*sandrunv1.AgentIntegration, []*sandrunv1.CreateAgentIntegrationRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*sandrunv1.AgentIntegration, 0, len(s.integration))
	for _, i := range s.integration {
		out = append(out, proto.CloneOf(i))
	}
	return out, slices.Clone(s.integrationReqs)
}

type orgService struct {
	sandrunv1connect.UnimplementedOrganizationServiceHandler
	s *Server
}

func (o *orgService) CreateOrganization(_ context.Context, req *connect.Request[sandrunv1.CreateOrganizationRequest]) (*connect.Response[sandrunv1.CreateOrganizationResponse], error) {
	if req.Msg.GetName() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("name is required"))
	}
	o.s.mu.Lock()
	defer o.s.mu.Unlock()
	org := o.s.addOrgLocked(req.Msg.GetName())
	return connect.NewResponse(&sandrunv1.CreateOrganizationResponse{Organization: proto.CloneOf(org)}), nil
}

func (o *orgService) GetOrganization(_ context.Context, req *connect.Request[sandrunv1.GetOrganizationRequest]) (*connect.Response[sandrunv1.GetOrganizationResponse], error) {
	o.s.mu.Lock()
	defer o.s.mu.Unlock()
	org, err := o.s.requireOrg(req.Msg.GetOrganizationId())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&sandrunv1.GetOrganizationResponse{Organization: proto.CloneOf(org)}), nil
}

func (o *orgService) ListOrganizations(_ context.Context, req *connect.Request[sandrunv1.ListOrganizationsRequest]) (*connect.Response[sandrunv1.ListOrganizationsResponse], error) {
	o.s.mu.Lock()
	defer o.s.mu.Unlock()
	items, next, err := page(o.s.orgs, req.Msg.GetPageSize(), req.Msg.GetPageToken())
	if err != nil {
		return nil, err
	}
	res := &sandrunv1.ListOrganizationsResponse{NextPageToken: next}
	for _, it := range items {
		res.Organizations = append(res.Organizations, proto.CloneOf(it))
	}
	return connect.NewResponse(res), nil
}

func (o *orgService) DeleteOrganization(_ context.Context, req *connect.Request[sandrunv1.DeleteOrganizationRequest]) (*connect.Response[sandrunv1.DeleteOrganizationResponse], error) {
	o.s.mu.Lock()
	defer o.s.mu.Unlock()
	if _, err := o.s.requireOrg(req.Msg.GetOrganizationId()); err != nil {
		return nil, err
	}
	o.s.orgs = slices.DeleteFunc(o.s.orgs, func(x *sandrunv1.Organization) bool { return x.GetOrganizationId() == req.Msg.GetOrganizationId() })
	return connect.NewResponse(&sandrunv1.DeleteOrganizationResponse{}), nil
}

func (o *orgService) UpdateOrganizationSettings(_ context.Context, req *connect.Request[sandrunv1.UpdateOrganizationSettingsRequest]) (*connect.Response[sandrunv1.UpdateOrganizationSettingsResponse], error) {
	o.s.mu.Lock()
	defer o.s.mu.Unlock()
	org, err := o.s.requireOrg(req.Msg.GetOrganizationId())
	if err != nil {
		return nil, err
	}
	paths := req.Msg.GetUpdateMask().GetPaths()
	if len(paths) == 0 || slices.ContainsFunc(paths, func(path string) bool {
		return path != "max_dev_sessions_per_user"
	}) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("update_mask must name max_dev_sessions_per_user"))
	}
	if settings := req.Msg.GetSettings(); settings != nil && settings.MaxDevSessionsPerUser != nil {
		v := settings.GetMaxDevSessionsPerUser()
		if v <= 0 {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("max_dev_sessions_per_user must be positive"))
		}
		org.Settings = &sandrunv1.OrganizationSettings{MaxDevSessionsPerUser: &v}
	} else {
		org.Settings = &sandrunv1.OrganizationSettings{}
	}
	return connect.NewResponse(&sandrunv1.UpdateOrganizationSettingsResponse{Organization: proto.CloneOf(org)}), nil
}

type memberService struct {
	sandrunv1connect.UnimplementedMemberServiceHandler
	s *Server
}

func (m *memberService) ListMembers(_ context.Context, req *connect.Request[sandrunv1.ListMembersRequest]) (*connect.Response[sandrunv1.ListMembersResponse], error) {
	m.s.mu.Lock()
	defer m.s.mu.Unlock()
	if _, err := m.s.requireOrg(req.Msg.GetOrganizationId()); err != nil {
		return nil, err
	}
	items, next, err := page(m.s.members[req.Msg.GetOrganizationId()], req.Msg.GetPageSize(), req.Msg.GetPageToken())
	if err != nil {
		return nil, err
	}
	res := &sandrunv1.ListMembersResponse{NextPageToken: next}
	for _, it := range items {
		res.Members = append(res.Members, proto.CloneOf(it))
	}
	return connect.NewResponse(res), nil
}

func (m *memberService) RemoveMember(_ context.Context, req *connect.Request[sandrunv1.RemoveMemberRequest]) (*connect.Response[sandrunv1.RemoveMemberResponse], error) {
	m.s.mu.Lock()
	defer m.s.mu.Unlock()
	org := req.Msg.GetOrganizationId()
	if _, err := m.s.requireOrg(org); err != nil {
		return nil, err
	}
	before := len(m.s.members[org])
	m.s.members[org] = slices.DeleteFunc(m.s.members[org], func(x *sandrunv1.Member) bool { return x.GetUserId() == req.Msg.GetUserId() })
	if len(m.s.members[org]) == before {
		return nil, notFound("member")
	}
	return connect.NewResponse(&sandrunv1.RemoveMemberResponse{}), nil
}

type linkService struct {
	sandrunv1connect.UnimplementedOwnSandLinkServiceHandler
	s *Server
}

func (l *linkService) SetOwnSandLink(_ context.Context, req *connect.Request[sandrunv1.SetOwnSandLinkRequest]) (*connect.Response[sandrunv1.SetOwnSandLinkResponse], error) {
	l.s.mu.Lock()
	defer l.s.mu.Unlock()
	org := req.Msg.GetOrganizationId()
	if _, err := l.s.requireOrg(org); err != nil {
		return nil, err
	}
	if req.Msg.GetOwnsandApiKey() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("ownsand_api_key is required"))
	}
	link := &sandrunv1.OwnSandLink{
		OrganizationId: org, OwnsandOrganizationId: req.Msg.GetOwnsandOrganizationId(),
		State: sandrunv1.OwnSandLinkState_OWN_SAND_LINK_STATE_ACTIVE, CredentialSet: true,
		CredentialSetAt: timestamppb.Now(), CreateTime: timestamppb.Now(),
	}
	l.s.links[org] = link
	if l.s.linkKeys == nil {
		l.s.linkKeys = map[string]string{}
	}
	l.s.linkKeys[org] = req.Msg.GetOwnsandApiKey()
	return connect.NewResponse(&sandrunv1.SetOwnSandLinkResponse{Link: proto.CloneOf(link)}), nil
}

func (l *linkService) GetOwnSandLink(_ context.Context, req *connect.Request[sandrunv1.GetOwnSandLinkRequest]) (*connect.Response[sandrunv1.GetOwnSandLinkResponse], error) {
	l.s.mu.Lock()
	defer l.s.mu.Unlock()
	org := req.Msg.GetOrganizationId()
	if _, err := l.s.requireOrg(org); err != nil {
		return nil, err
	}
	res := &sandrunv1.GetOwnSandLinkResponse{}
	if link := l.s.links[org]; link != nil {
		res.Link = proto.CloneOf(link)
	}
	return connect.NewResponse(res), nil
}

func (l *linkService) RemoveOwnSandLink(_ context.Context, req *connect.Request[sandrunv1.RemoveOwnSandLinkRequest]) (*connect.Response[sandrunv1.RemoveOwnSandLinkResponse], error) {
	l.s.mu.Lock()
	defer l.s.mu.Unlock()
	if l.s.links[req.Msg.GetOrganizationId()] == nil {
		return nil, notFound("link")
	}
	delete(l.s.links, req.Msg.GetOrganizationId())
	return connect.NewResponse(&sandrunv1.RemoveOwnSandLinkResponse{}), nil
}

type gitHubService struct {
	sandrunv1connect.UnimplementedGitHubServiceHandler
	s *Server
}

func (g *gitHubService) CreateGitHubInstallUrl(_ context.Context, req *connect.Request[sandrunv1.CreateGitHubInstallUrlRequest]) (*connect.Response[sandrunv1.CreateGitHubInstallUrlResponse], error) {
	g.s.mu.Lock()
	defer g.s.mu.Unlock()
	if _, err := g.s.requireOrg(req.Msg.GetOrganizationId()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&sandrunv1.CreateGitHubInstallUrlResponse{
		Url:        "https://github.com/apps/sandrun/installations/new?state=test-state",
		ExpireTime: timestamppb.New(time.Now().Add(10 * time.Minute)),
	}), nil
}

type integrationService struct {
	sandrunv1connect.UnimplementedAgentIntegrationServiceHandler
	s *Server
}

func (i *integrationService) CreateAgentIntegration(_ context.Context, req *connect.Request[sandrunv1.CreateAgentIntegrationRequest]) (*connect.Response[sandrunv1.CreateAgentIntegrationResponse], error) {
	i.s.mu.Lock()
	defer i.s.mu.Unlock()
	if _, err := i.s.requireOrg(req.Msg.GetOrganizationId()); err != nil {
		return nil, err
	}
	it := &sandrunv1.AgentIntegration{
		AgentIntegrationId: i.s.newID(), OrganizationId: req.Msg.GetOrganizationId(),
		Vendor: req.Msg.GetVendor(), Name: req.Msg.GetName(),
		CreateTime: timestamppb.Now(), UpdateTime: timestamppb.Now(),
	}
	i.s.integration = append(i.s.integration, it)
	i.s.integrationReqs = append(i.s.integrationReqs, proto.CloneOf(req.Msg))
	return connect.NewResponse(&sandrunv1.CreateAgentIntegrationResponse{Integration: proto.CloneOf(it)}), nil
}

func (i *integrationService) ListAgentIntegrations(_ context.Context, req *connect.Request[sandrunv1.ListAgentIntegrationsRequest]) (*connect.Response[sandrunv1.ListAgentIntegrationsResponse], error) {
	i.s.mu.Lock()
	defer i.s.mu.Unlock()
	var mine []*sandrunv1.AgentIntegration
	for _, it := range i.s.integration {
		if it.GetOrganizationId() == req.Msg.GetOrganizationId() {
			mine = append(mine, it)
		}
	}
	items, next, err := page(mine, req.Msg.GetPageSize(), req.Msg.GetPageToken())
	if err != nil {
		return nil, err
	}
	res := &sandrunv1.ListAgentIntegrationsResponse{NextPageToken: next}
	for _, it := range items {
		res.Integrations = append(res.Integrations, proto.CloneOf(it))
	}
	return connect.NewResponse(res), nil
}

type profileService struct {
	sandrunv1connect.UnimplementedRunnerProfileServiceHandler
	s *Server
}

func (p *profileService) ListRunnerProfiles(_ context.Context, req *connect.Request[sandrunv1.ListRunnerProfilesRequest]) (*connect.Response[sandrunv1.ListRunnerProfilesResponse], error) {
	p.s.mu.Lock()
	defer p.s.mu.Unlock()
	if _, err := p.s.requireOrg(req.Msg.GetOrganizationId()); err != nil {
		return nil, err
	}
	kind := req.Msg.GetWorkloadKind()
	if err := definedListEnum("workload_kind", "WorkloadKind", kind, sandrunv1.WorkloadKind_name); err != nil {
		return nil, err
	}
	var mine []*sandrunv1.RunnerProfile
	for _, it := range p.s.profiles {
		if it.GetOrganizationId() == req.Msg.GetOrganizationId() &&
			(kind == sandrunv1.WorkloadKind_WORKLOAD_KIND_UNSPECIFIED || kind == it.GetWorkloadKind()) {
			mine = append(mine, it)
		}
	}
	items, next, err := page(mine, req.Msg.GetPageSize(), req.Msg.GetPageToken())
	if err != nil {
		return nil, err
	}
	res := &sandrunv1.ListRunnerProfilesResponse{NextPageToken: next}
	for _, it := range items {
		res.Profiles = append(res.Profiles, proto.CloneOf(it))
	}
	return connect.NewResponse(res), nil
}

func (p *profileService) GetRunnerProfile(_ context.Context, req *connect.Request[sandrunv1.GetRunnerProfileRequest]) (*connect.Response[sandrunv1.GetRunnerProfileResponse], error) {
	p.s.mu.Lock()
	defer p.s.mu.Unlock()
	for _, it := range p.s.profiles {
		if it.GetOrganizationId() == req.Msg.GetOrganizationId() && it.GetRunnerProfileId() == req.Msg.GetRunnerProfileId() {
			return connect.NewResponse(&sandrunv1.GetRunnerProfileResponse{Profile: proto.CloneOf(it)}), nil
		}
	}
	return nil, notFound("runner profile")
}

func (p *profileService) CreateRunnerProfile(_ context.Context, req *connect.Request[sandrunv1.CreateRunnerProfileRequest]) (*connect.Response[sandrunv1.CreateRunnerProfileResponse], error) {
	p.s.mu.Lock()
	defer p.s.mu.Unlock()
	if _, err := p.s.requireOrg(req.Msg.GetOrganizationId()); err != nil {
		return nil, err
	}
	in := req.Msg.GetProfile()
	if in.GetName() == "" || in.GetWorkloadKind() == sandrunv1.WorkloadKind_WORKLOAD_KIND_UNSPECIFIED || in.GetGuestOs() == sandrunv1.GuestOs_GUEST_OS_UNSPECIFIED {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("name, workload_kind and guest_os are required"))
	}
	pr := proto.CloneOf(in)
	pr.RunnerProfileId = p.s.newID()
	pr.OrganizationId = req.Msg.GetOrganizationId()
	pr.CreateTime = timestamppb.Now()
	pr.UpdateTime = pr.CreateTime
	p.s.profiles = append(p.s.profiles, pr)
	return connect.NewResponse(&sandrunv1.CreateRunnerProfileResponse{Profile: proto.CloneOf(pr)}), nil
}

func (p *profileService) DeleteRunnerProfile(_ context.Context, req *connect.Request[sandrunv1.DeleteRunnerProfileRequest]) (*connect.Response[sandrunv1.DeleteRunnerProfileResponse], error) {
	p.s.mu.Lock()
	defer p.s.mu.Unlock()
	n := len(p.s.profiles)
	p.s.profiles = slices.DeleteFunc(p.s.profiles, func(x *sandrunv1.RunnerProfile) bool {
		return x.GetOrganizationId() == req.Msg.GetOrganizationId() && x.GetRunnerProfileId() == req.Msg.GetRunnerProfileId()
	})
	if len(p.s.profiles) == n {
		return nil, notFound("runner profile")
	}
	return connect.NewResponse(&sandrunv1.DeleteRunnerProfileResponse{}), nil
}

type workService struct {
	sandrunv1connect.UnimplementedWorkServiceHandler
	s *Server
}

func terminalRunner(st sandrunv1.RunnerState) bool {
	return st == sandrunv1.RunnerState_RUNNER_STATE_DELETED || st == sandrunv1.RunnerState_RUNNER_STATE_FAILED
}

func terminalRequest(st sandrunv1.RequestState) bool {
	switch st {
	case sandrunv1.RequestState_REQUEST_STATE_ENDED, sandrunv1.RequestState_REQUEST_STATE_CANCELED,
		sandrunv1.RequestState_REQUEST_STATE_EXPIRED, sandrunv1.RequestState_REQUEST_STATE_FAILED:
		return true
	}
	return false
}

var openRequestStates = []sandrunv1.RequestState{
	sandrunv1.RequestState_REQUEST_STATE_QUEUED,
	sandrunv1.RequestState_REQUEST_STATE_STARTING,
	sandrunv1.RequestState_REQUEST_STATE_RUNNING,
}

var openRunnerStates = []sandrunv1.RunnerState{
	sandrunv1.RunnerState_RUNNER_STATE_CREATING,
	sandrunv1.RunnerState_RUNNER_STATE_PREPARING,
	sandrunv1.RunnerState_RUNNER_STATE_WARM,
	sandrunv1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK,
	sandrunv1.RunnerState_RUNNER_STATE_BUSY,
	sandrunv1.RunnerState_RUNNER_STATE_DELETING,
}

func definedListEnum[T ~int32](field, enum string, value T, names map[int32]string) error {
	if _, ok := names[int32(value)]; !ok {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid %s: must be a defined %s value", field, enum))
	}
	return nil
}

// listStates validates explicit states and returns the states to match. An
// empty result allows every state.
func listStates[T ~int32](states, open []T, includeEnded bool, enum string, names map[int32]string) ([]T, error) {
	if len(states) > 0 {
		for _, state := range states {
			if err := definedListEnum("states", enum, state, names); err != nil {
				return nil, err
			}
		}
		return states, nil
	}
	if includeEnded {
		return nil, nil
	}
	return open, nil
}

func normalizeListProfileID(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	id, err := uuidv7.Parse(value)
	if err != nil {
		return "", connect.NewError(connect.CodeInvalidArgument, errors.New("invalid runner_profile_id: must be a UUID v7"))
	}
	return id.String(), nil
}

func (w *workService) ListRequests(_ context.Context, req *connect.Request[sandrunv1.ListRequestsRequest]) (*connect.Response[sandrunv1.ListRequestsResponse], error) {
	m := req.Msg
	w.s.mu.Lock()
	defer w.s.mu.Unlock()
	if _, err := w.s.requireOrg(m.GetOrganizationId()); err != nil {
		return nil, err
	}
	profileID, err := normalizeListProfileID(m.GetRunnerProfileId())
	if err != nil {
		return nil, err
	}
	kind := m.GetKind()
	if err := definedListEnum("kind", "WorkloadKind", kind, sandrunv1.WorkloadKind_name); err != nil {
		return nil, err
	}
	states, err := listStates(m.GetStates(), openRequestStates, m.GetIncludeEnded(), "RequestState", sandrunv1.RequestState_name)
	if err != nil {
		return nil, err
	}
	var mine []*sandrunv1.Request
	for _, r := range w.s.requests {
		if r.GetOrganizationId() != m.GetOrganizationId() || (profileID != "" && r.GetRunnerProfileId() != profileID) {
			continue
		}
		if kind != sandrunv1.WorkloadKind_WORKLOAD_KIND_UNSPECIFIED && r.GetKind() != kind {
			continue
		}
		if len(states) > 0 && !slices.Contains(states, r.GetState()) {
			continue
		}
		mine = append(mine, r)
	}
	items, next, err := page(mine, m.GetPageSize(), m.GetPageToken())
	if err != nil {
		return nil, err
	}
	res := &sandrunv1.ListRequestsResponse{NextPageToken: next}
	for _, it := range items {
		res.Requests = append(res.Requests, proto.CloneOf(it))
	}
	return connect.NewResponse(res), nil
}

func (w *workService) GetRequest(_ context.Context, req *connect.Request[sandrunv1.GetRequestRequest]) (*connect.Response[sandrunv1.GetRequestResponse], error) {
	w.s.mu.Lock()
	defer w.s.mu.Unlock()
	for _, r := range w.s.requests {
		if r.GetOrganizationId() == req.Msg.GetOrganizationId() && r.GetRequestId() == req.Msg.GetRequestId() {
			return connect.NewResponse(&sandrunv1.GetRequestResponse{Request: proto.CloneOf(r)}), nil
		}
	}
	return nil, notFound("request")
}

func (w *workService) CancelRequest(_ context.Context, req *connect.Request[sandrunv1.CancelRequestRequest]) (*connect.Response[sandrunv1.CancelRequestResponse], error) {
	w.s.mu.Lock()
	defer w.s.mu.Unlock()
	for _, r := range w.s.requests {
		if r.GetOrganizationId() == req.Msg.GetOrganizationId() && r.GetRequestId() == req.Msg.GetRequestId() {
			if r.GetState() != sandrunv1.RequestState_REQUEST_STATE_QUEUED {
				return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("the request is not queued"))
			}
			r.State = sandrunv1.RequestState_REQUEST_STATE_CANCELED
			r.EndReason = sandrunv1.RequestEndReason_REQUEST_END_REASON_CANCELED_BY_USER
			r.EndedAt = timestamppb.Now()
			return connect.NewResponse(&sandrunv1.CancelRequestResponse{Request: proto.CloneOf(r)}), nil
		}
	}
	return nil, notFound("request")
}

func (w *workService) ListRunners(_ context.Context, req *connect.Request[sandrunv1.ListRunnersRequest]) (*connect.Response[sandrunv1.ListRunnersResponse], error) {
	m := req.Msg
	w.s.mu.Lock()
	defer w.s.mu.Unlock()
	if _, err := w.s.requireOrg(m.GetOrganizationId()); err != nil {
		return nil, err
	}
	profileID, err := normalizeListProfileID(m.GetRunnerProfileId())
	if err != nil {
		return nil, err
	}
	states, err := listStates(m.GetStates(), openRunnerStates, m.GetIncludeEnded(), "RunnerState", sandrunv1.RunnerState_name)
	if err != nil {
		return nil, err
	}
	var mine []*sandrunv1.Runner
	for _, r := range w.s.runners {
		if r.GetOrganizationId() != m.GetOrganizationId() || (profileID != "" && r.GetRunnerProfileId() != profileID) {
			continue
		}
		if len(states) > 0 && !slices.Contains(states, r.GetState()) {
			continue
		}
		mine = append(mine, r)
	}
	items, next, err := page(mine, m.GetPageSize(), m.GetPageToken())
	if err != nil {
		return nil, err
	}
	res := &sandrunv1.ListRunnersResponse{NextPageToken: next}
	for _, it := range items {
		res.Runners = append(res.Runners, proto.CloneOf(it))
	}
	return connect.NewResponse(res), nil
}

func (w *workService) GetRunner(_ context.Context, req *connect.Request[sandrunv1.GetRunnerRequest]) (*connect.Response[sandrunv1.GetRunnerResponse], error) {
	w.s.mu.Lock()
	defer w.s.mu.Unlock()
	for _, r := range w.s.runners {
		if r.GetOrganizationId() == req.Msg.GetOrganizationId() && r.GetRunnerId() == req.Msg.GetRunnerId() {
			return connect.NewResponse(&sandrunv1.GetRunnerResponse{Runner: proto.CloneOf(r)}), nil
		}
	}
	return nil, notFound("runner")
}

func (w *workService) GetCapacity(_ context.Context, req *connect.Request[sandrunv1.GetCapacityRequest]) (*connect.Response[sandrunv1.GetCapacityResponse], error) {
	w.s.mu.Lock()
	defer w.s.mu.Unlock()
	if _, err := w.s.requireOrg(req.Msg.GetOrganizationId()); err != nil {
		return nil, err
	}
	res := &sandrunv1.GetCapacityResponse{}
	for _, d := range w.s.devices {
		res.Devices = append(res.Devices, proto.CloneOf(d))
	}
	return connect.NewResponse(res), nil
}

func (w *workService) WatchQueue(ctx context.Context, req *connect.Request[sandrunv1.WatchQueueRequest], stream *connect.ServerStream[sandrunv1.WatchQueueResponse]) error {
	orgID := req.Msg.GetOrganizationId()
	w.s.mu.Lock()
	if _, err := w.s.requireOrg(orgID); err != nil {
		w.s.mu.Unlock()
		return err
	}
	var snap []*sandrunv1.WatchQueueResponse
	for _, r := range w.s.requests {
		if r.GetOrganizationId() == orgID && !terminalRequest(r.GetState()) {
			snap = append(snap, &sandrunv1.WatchQueueResponse{Event: &sandrunv1.WatchQueueResponse_Request{Request: proto.CloneOf(r)}})
		}
	}
	for _, r := range w.s.runners {
		if r.GetOrganizationId() == orgID && !terminalRunner(r.GetState()) {
			snap = append(snap, &sandrunv1.WatchQueueResponse{Event: &sandrunv1.WatchQueueResponse_Runner{Runner: proto.CloneOf(r)}})
		}
	}
	limit := w.s.WatchEventLimit
	w.s.mu.Unlock()
	snap = append(snap, &sandrunv1.WatchQueueResponse{Event: &sandrunv1.WatchQueueResponse_SnapshotComplete{SnapshotComplete: &sandrunv1.WatchQueueSnapshotComplete{}}})
	sent := 0
	send := func(m *sandrunv1.WatchQueueResponse) (bool, error) {
		if err := stream.Send(m); err != nil {
			return false, err
		}
		sent++
		return limit > 0 && sent >= limit, nil
	}
	for _, m := range snap {
		if done, err := send(m); done || err != nil {
			return err
		}
	}
	queue := w.s.queueForOrganization(orgID)
	for {
		var m *sandrunv1.WatchQueueResponse
		select {
		case <-ctx.Done():
			return nil
		case m = <-queue:
		case m = <-w.s.unscopedQueue:
		}
		if done, err := send(m); done || err != nil {
			return err
		}
	}
}

type apiKeyService struct {
	sandrunv1connect.UnimplementedApiKeyServiceHandler
	s *Server
}

func (a *apiKeyService) CreateApiKey(_ context.Context, req *connect.Request[sandrunv1.CreateApiKeyRequest]) (*connect.Response[sandrunv1.CreateApiKeyResponse], error) {
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	if _, err := a.s.requireOrg(req.Msg.GetOrganizationId()); err != nil {
		return nil, err
	}
	if n := len(req.Msg.GetName()); n == 0 || n > 64 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("name must have 1 to 64 characters"))
	}
	k := &sandrunv1.ApiKey{
		ApiKeyId: a.s.newID(), OrganizationId: req.Msg.GetOrganizationId(), Name: req.Msg.GetName(),
		DisplayPrefix: "sandrun_abcd", State: sandrunv1.ApiKeyState_API_KEY_STATE_ACTIVE,
		CreatedAt: timestamppb.Now(), ExpiresAt: req.Msg.GetExpiresAt(),
	}
	a.s.apiKeys = append(a.s.apiKeys, k)
	return connect.NewResponse(&sandrunv1.CreateApiKeyResponse{ApiKey: proto.CloneOf(k), Secret: issuedAPIKeyPrefix + k.GetApiKeyId()}), nil
}

func (a *apiKeyService) ListApiKeys(_ context.Context, req *connect.Request[sandrunv1.ListApiKeysRequest]) (*connect.Response[sandrunv1.ListApiKeysResponse], error) {
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	var mine []*sandrunv1.ApiKey
	for _, k := range a.s.apiKeys {
		if k.GetOrganizationId() == req.Msg.GetOrganizationId() {
			mine = append(mine, k)
		}
	}
	items, next, err := page(mine, req.Msg.GetPageSize(), req.Msg.GetPageToken())
	if err != nil {
		return nil, err
	}
	res := &sandrunv1.ListApiKeysResponse{NextPageToken: next}
	now := time.Now()
	for _, it := range items {
		key := proto.CloneOf(it)
		if expiresAt := key.GetExpiresAt(); key.GetState() == sandrunv1.ApiKeyState_API_KEY_STATE_ACTIVE &&
			expiresAt != nil && !now.Before(expiresAt.AsTime()) {
			key.State = sandrunv1.ApiKeyState_API_KEY_STATE_EXPIRED
		}
		res.ApiKeys = append(res.ApiKeys, key)
	}
	return connect.NewResponse(res), nil
}

func (a *apiKeyService) RevokeApiKey(_ context.Context, req *connect.Request[sandrunv1.RevokeApiKeyRequest]) (*connect.Response[sandrunv1.RevokeApiKeyResponse], error) {
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	for _, k := range a.s.apiKeys {
		if k.GetOrganizationId() == req.Msg.GetOrganizationId() && k.GetApiKeyId() == req.Msg.GetApiKeyId() {
			if k.GetState() != sandrunv1.ApiKeyState_API_KEY_STATE_REVOKED {
				k.State = sandrunv1.ApiKeyState_API_KEY_STATE_REVOKED
				k.RevokedAt = timestamppb.Now()
			}
			return connect.NewResponse(&sandrunv1.RevokeApiKeyResponse{ApiKey: proto.CloneOf(k)}), nil
		}
	}
	return nil, notFound("api key")
}
