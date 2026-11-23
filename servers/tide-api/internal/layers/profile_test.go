// SPDX-License-Identifier: FSL-1.1-ALv2

package layers_test

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/servers/tide-api/internal/api"
)

func TestRunnerProfiles(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	admin := w.env.User(w.org.Admin)
	base := func() *tidev1.RunnerProfile {
		return &tidev1.RunnerProfile{
			Name: "win-dev", WorkloadKind: tidev1.WorkloadKind_WORKLOAD_KIND_DEV_SESSION, WorkspacePath: `C:\work`,
			GuestOs: tidev1.GuestOs_GUEST_OS_WINDOWS, Source: &tidev1.RunnerProfile_BaseImage{BaseImage: tidev1.GuestOs_GUEST_OS_WINDOWS},
			Resources: &tidev1.Resources{VcpuCount: 4, MemoryMib: 8192, DiskGib: 64}, NetworkMode: tidev1.NetworkMode_NETWORK_MODE_OUTBOUND,
			IdleTtl: durationpb.New(time.Hour), MaxTtl: durationpb.New(8 * time.Hour), MaxQueueWait: durationpb.New(30 * time.Minute), WarmCount: 1,
		}
	}
	resp, err := admin.Profile.CreateRunnerProfile(ctx, connect.NewRequest(&tidev1.CreateRunnerProfileRequest{OrganizationId: w.org.ID, Profile: base()}))
	if err != nil {
		t.Fatal(err)
	}
	p := resp.Msg.GetProfile()
	if p.GetPriority() != tidev1.Priority_PRIORITY_NORMAL || p.GetGithubLabel() != "" || p.GetRunnerProfileId() == "" || p.GetBaseImage() != tidev1.GuestOs_GUEST_OS_WINDOWS {
		t.Fatalf("profile %v", p)
	}
	_, err = admin.Profile.CreateRunnerProfile(ctx, connect.NewRequest(&tidev1.CreateRunnerProfileRequest{OrganizationId: w.org.ID, Profile: base()}))
	wantCode(t, "duplicate name", err, connect.CodeAlreadyExists)
	gh := base()
	gh.Name, gh.WorkloadKind, gh.WorkspacePath = "win-ci", tidev1.WorkloadKind_WORKLOAD_KIND_GITHUB_JOB, ""
	ghResp, err := admin.Profile.CreateRunnerProfile(ctx, connect.NewRequest(&tidev1.CreateRunnerProfileRequest{OrganizationId: w.org.ID, Profile: gh}))
	if err != nil || ghResp.Msg.GetProfile().GetPriority() != tidev1.Priority_PRIORITY_HIGH || ghResp.Msg.GetProfile().GetGithubLabel() != "tide-win-ci" {
		t.Fatalf("github profile %v %v", ghResp, err)
	}
	_, err = w.env.User(w.org.Member).Profile.CreateRunnerProfile(ctx, connect.NewRequest(&tidev1.CreateRunnerProfileRequest{OrganizationId: w.org.ID, Profile: base()}))
	wantCode(t, "member creates", err, connect.CodePermissionDenied)
	agent := base()
	agent.Name, agent.WorkloadKind, agent.WorkspacePath, agent.AgentIntegrationId = "agent", tidev1.WorkloadKind_WORKLOAD_KIND_AGENT_SESSION, "", api.NewID()
	_, err = admin.Profile.CreateRunnerProfile(ctx, connect.NewRequest(&tidev1.CreateRunnerProfileRequest{OrganizationId: w.org.ID, Profile: agent}))
	wantCode(t, "unknown integration", err, connect.CodeNotFound)

	// List with a kind filter; members read.
	list, err := w.env.User(w.org.Member).Profile.ListRunnerProfiles(ctx, connect.NewRequest(&tidev1.ListRunnerProfilesRequest{OrganizationId: w.org.ID, WorkloadKind: tidev1.WorkloadKind_WORKLOAD_KIND_DEV_SESSION}))
	if err != nil || len(list.Msg.GetProfiles()) != 1 {
		t.Fatalf("list %v %v", list, err)
	}

	// Update: kind and integration cannot change.
	id := p.GetRunnerProfileId()
	upd := func(profile *tidev1.RunnerProfile, paths ...string) (*tidev1.RunnerProfile, error) {
		r, err := admin.Profile.UpdateRunnerProfile(ctx, connect.NewRequest(&tidev1.UpdateRunnerProfileRequest{OrganizationId: w.org.ID, RunnerProfileId: id,
			Profile: profile, UpdateMask: &fieldmaskpb.FieldMask{Paths: paths}}))
		if err != nil {
			return nil, err
		}
		return r.Msg.GetProfile(), nil
	}
	_, err = upd(&tidev1.RunnerProfile{WorkloadKind: tidev1.WorkloadKind_WORKLOAD_KIND_GITHUB_JOB}, "workload_kind")
	wantCode(t, "change kind", err, connect.CodeInvalidArgument)
	_, err = upd(&tidev1.RunnerProfile{}, "agent_integration_id")
	wantCode(t, "change integration", err, connect.CodeInvalidArgument)
	_, err = upd(&tidev1.RunnerProfile{GuestOs: tidev1.GuestOs_GUEST_OS_LINUX}, "guest_os")
	wantCode(t, "guest OS without a matching source and path", err, connect.CodeInvalidArgument)
	np, err := upd(&tidev1.RunnerProfile{GuestOs: tidev1.GuestOs_GUEST_OS_LINUX, WorkspacePath: "/work", Source: &tidev1.RunnerProfile_BaseImage{BaseImage: tidev1.GuestOs_GUEST_OS_LINUX},
		Priority: tidev1.Priority_PRIORITY_LOW}, "guest_os", "workspace_path", "source", "priority")
	if err != nil || np.GetGuestOs() != tidev1.GuestOs_GUEST_OS_LINUX || np.GetPriority() != tidev1.Priority_PRIORITY_LOW || np.GetWarmCount() != 1 {
		t.Fatalf("update %v %v", np, err)
	}

	// Template sources: without a link, any template ID is accepted; with a
	// link, OwnSand checks it.
	tmplOK, tmplMac := api.NewID(), api.NewID()
	_, err = upd(&tidev1.RunnerProfile{Source: &tidev1.RunnerProfile_OwnsandTemplateId{OwnsandTemplateId: tmplMac}}, "source")
	if err != nil {
		t.Fatalf("template without link: %v", err)
	}
	osOrg := api.NewID()
	w.fake.AddOrg(osOrg, ownsandKey('a'))
	w.fake.SetTemplate(osOrg, tmplOK, ownsandv1.GuestOs_GUEST_OS_LINUX)
	w.fake.SetTemplate(osOrg, tmplMac, ownsandv1.GuestOs_GUEST_OS_MACOS)
	if _, err := w.setLink(w.org.Admin, osOrg, ownsandKey('a')); err != nil {
		t.Fatal(err)
	}
	_, err = upd(&tidev1.RunnerProfile{Source: &tidev1.RunnerProfile_OwnsandTemplateId{OwnsandTemplateId: api.NewID()}}, "source")
	wantCode(t, "unknown template", err, connect.CodeInvalidArgument)
	_, err = upd(&tidev1.RunnerProfile{GuestOs: tidev1.GuestOs_GUEST_OS_LINUX}, "guest_os")
	wantCode(t, "template of another guest OS", err, connect.CodeInvalidArgument)
	if _, err := upd(&tidev1.RunnerProfile{Source: &tidev1.RunnerProfile_OwnsandTemplateId{OwnsandTemplateId: tmplOK}}, "source"); err != nil {
		t.Fatalf("template of the guest OS: %v", err)
	}

	// Personal API keys are rejected.
	_, secret := w.env.NewAPIKey(w.org.ID, w.org.Owner)
	_, err = w.env.Bearer(secret).Profile.ListRunnerProfiles(ctx, connect.NewRequest(&tidev1.ListRunnerProfilesRequest{OrganizationId: w.org.ID}))
	wantCode(t, "personal API key", err, connect.CodePermissionDenied)
	_, err = w.env.User("outsider").Profile.GetRunnerProfile(ctx, connect.NewRequest(&tidev1.GetRunnerProfileRequest{OrganizationId: w.org.ID, RunnerProfileId: id}))
	wantCode(t, "outsider", err, connect.CodeNotFound)
	other := w.env.NewOrg("bob")
	_, err = w.env.User("bob").Profile.GetRunnerProfile(ctx, connect.NewRequest(&tidev1.GetRunnerProfileRequest{OrganizationId: other.ID, RunnerProfileId: id}))
	wantCode(t, "profile of another organization", err, connect.CodeNotFound)

	if _, err := admin.Profile.DeleteRunnerProfile(ctx, connect.NewRequest(&tidev1.DeleteRunnerProfileRequest{OrganizationId: w.org.ID, RunnerProfileId: id})); err != nil {
		t.Fatal(err)
	}
	_, err = admin.Profile.GetRunnerProfile(ctx, connect.NewRequest(&tidev1.GetRunnerProfileRequest{OrganizationId: w.org.ID, RunnerProfileId: id}))
	wantCode(t, "deleted profile", err, connect.CodeNotFound)
	if countAudit(t, w.env, w.org.ID, tidev1.AuditAction_AUDIT_ACTION_RUNNER_PROFILE_CREATED) != 2 ||
		countAudit(t, w.env, w.org.ID, tidev1.AuditAction_AUDIT_ACTION_RUNNER_PROFILE_UPDATED) != 3 ||
		countAudit(t, w.env, w.org.ID, tidev1.AuditAction_AUDIT_ACTION_RUNNER_PROFILE_DELETED) != 1 {
		t.Fatal("audit events")
	}
}

// Two updates of different masked fields of one profile that overlap both
// stay applied.
func TestRunnerProfileConcurrentUpdates(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	admin := w.env.User(w.org.Admin)
	resp, err := admin.Profile.CreateRunnerProfile(ctx, connect.NewRequest(&tidev1.CreateRunnerProfileRequest{OrganizationId: w.org.ID, Profile: &tidev1.RunnerProfile{
		Name: "dev", WorkloadKind: tidev1.WorkloadKind_WORKLOAD_KIND_DEV_SESSION, WorkspacePath: "/work",
		GuestOs: tidev1.GuestOs_GUEST_OS_LINUX, Source: &tidev1.RunnerProfile_BaseImage{BaseImage: tidev1.GuestOs_GUEST_OS_LINUX},
		Resources: &tidev1.Resources{VcpuCount: 2, MemoryMib: 4096, DiskGib: 32}, NetworkMode: tidev1.NetworkMode_NETWORK_MODE_OUTBOUND,
		IdleTtl: durationpb.New(time.Hour), MaxTtl: durationpb.New(8 * time.Hour), MaxQueueWait: durationpb.New(30 * time.Minute), WarmCount: 1,
	}}))
	if err != nil {
		t.Fatal(err)
	}
	id := resp.Msg.GetProfile().GetRunnerProfileId()
	// The held update stands in for another admin's warm_count update that
	// commits after this name update read the profile.
	commit := holdTx(t, w.env, "UPDATE runner_profiles SET warm_count = 3 WHERE runner_profile_id = $1", id)
	errc := make(chan error, 1)
	go func() {
		_, err := admin.Profile.UpdateRunnerProfile(ctx, connect.NewRequest(&tidev1.UpdateRunnerProfileRequest{OrganizationId: w.org.ID, RunnerProfileId: id,
			Profile: &tidev1.RunnerProfile{Name: "renamed"}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"name"}}}))
		errc <- err
	}()
	waitForLockWaiter(t, w.env)
	commit()
	if err := <-errc; err != nil {
		t.Fatalf("name update: %v", err)
	}
	got, err := admin.Profile.GetRunnerProfile(ctx, connect.NewRequest(&tidev1.GetRunnerProfileRequest{OrganizationId: w.org.ID, RunnerProfileId: id}))
	if err != nil || got.Msg.GetProfile().GetName() != "renamed" || got.Msg.GetProfile().GetWarmCount() != 3 {
		t.Fatalf("profile after both updates: %v %v", got, err)
	}
}

// Durations are stored in milliseconds, so a value with a smaller part is
// rejected instead of being truncated.
func TestRunnerProfileSubMillisecondDurations(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	admin := w.env.User(w.org.Admin)
	base := func() *tidev1.RunnerProfile {
		return &tidev1.RunnerProfile{
			Name: "dev", WorkloadKind: tidev1.WorkloadKind_WORKLOAD_KIND_DEV_SESSION, WorkspacePath: "/work",
			GuestOs: tidev1.GuestOs_GUEST_OS_LINUX, Source: &tidev1.RunnerProfile_BaseImage{BaseImage: tidev1.GuestOs_GUEST_OS_LINUX},
			Resources: &tidev1.Resources{VcpuCount: 2, MemoryMib: 4096, DiskGib: 32}, NetworkMode: tidev1.NetworkMode_NETWORK_MODE_OUTBOUND,
			IdleTtl: durationpb.New(time.Hour), MaxTtl: durationpb.New(8 * time.Hour), MaxQueueWait: durationpb.New(30 * time.Minute),
		}
	}
	for name, change := range map[string]func(*tidev1.RunnerProfile){
		"idle_ttl":       func(p *tidev1.RunnerProfile) { p.IdleTtl = durationpb.New(500 * time.Microsecond) },
		"max_ttl":        func(p *tidev1.RunnerProfile) { p.MaxTtl = durationpb.New(time.Hour + time.Nanosecond) },
		"max_queue_wait": func(p *tidev1.RunnerProfile) { p.MaxQueueWait = durationpb.New(1500 * time.Microsecond) },
	} {
		in := base()
		change(in)
		_, err := admin.Profile.CreateRunnerProfile(ctx, connect.NewRequest(&tidev1.CreateRunnerProfileRequest{OrganizationId: w.org.ID, Profile: in}))
		wantCode(t, "create with sub-millisecond "+name, err, connect.CodeInvalidArgument)
	}
	resp, err := admin.Profile.CreateRunnerProfile(ctx, connect.NewRequest(&tidev1.CreateRunnerProfileRequest{OrganizationId: w.org.ID, Profile: base()}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = admin.Profile.UpdateRunnerProfile(ctx, connect.NewRequest(&tidev1.UpdateRunnerProfileRequest{OrganizationId: w.org.ID, RunnerProfileId: resp.Msg.GetProfile().GetRunnerProfileId(),
		Profile: &tidev1.RunnerProfile{MaxQueueWait: durationpb.New(500 * time.Microsecond)}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"max_queue_wait"}}}))
	wantCode(t, "update with sub-millisecond max_queue_wait", err, connect.CodeInvalidArgument)
}

// TestIntegrationDeleteBlockedByProfile checks that an integration cannot be
// deleted while a runner profile names it.
func TestIntegrationDeleteBlockedByProfile(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	admin := w.env.User(w.org.Admin)
	created, err := admin.Integration.CreateAgentIntegration(ctx, connect.NewRequest(&tidev1.CreateAgentIntegrationRequest{OrganizationId: w.org.ID,
		Vendor: tidev1.AgentVendor_AGENT_VENDOR_CURSOR, Name: "cursor",
		Settings:    &tidev1.CreateAgentIntegrationRequest_Cursor{Cursor: &tidev1.CursorSettings{PoolName: "pool-a"}},
		Credentials: &tidev1.CreateAgentIntegrationRequest_CursorCredentials{CursorCredentials: &tidev1.CursorCredentials{ServiceAccountApiKey: "k"}}}))
	if err != nil {
		t.Fatal(err)
	}
	cursor := created.Msg.GetIntegration()
	// Delete: blocked while a profile names the integration.
	prof, err := admin.Profile.CreateRunnerProfile(ctx, connect.NewRequest(&tidev1.CreateRunnerProfileRequest{OrganizationId: w.org.ID, Profile: &tidev1.RunnerProfile{
		Name: "cursor-linux", WorkloadKind: tidev1.WorkloadKind_WORKLOAD_KIND_AGENT_SESSION, AgentIntegrationId: cursor.GetAgentIntegrationId(),
		GuestOs: tidev1.GuestOs_GUEST_OS_LINUX, Source: &tidev1.RunnerProfile_BaseImage{BaseImage: tidev1.GuestOs_GUEST_OS_LINUX},
		Resources: &tidev1.Resources{VcpuCount: 2, MemoryMib: 2048, DiskGib: 10}, NetworkMode: tidev1.NetworkMode_NETWORK_MODE_OUTBOUND,
		IdleTtl: durationpb.New(time.Hour), MaxTtl: durationpb.New(2 * time.Hour), MaxQueueWait: durationpb.New(time.Hour)}}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = admin.Integration.DeleteAgentIntegration(ctx, connect.NewRequest(&tidev1.DeleteAgentIntegrationRequest{OrganizationId: w.org.ID, AgentIntegrationId: cursor.GetAgentIntegrationId()}))
	wantCode(t, "delete a used integration", err, connect.CodeFailedPrecondition)
	if _, err := admin.Profile.DeleteRunnerProfile(ctx, connect.NewRequest(&tidev1.DeleteRunnerProfileRequest{OrganizationId: w.org.ID, RunnerProfileId: prof.Msg.GetProfile().GetRunnerProfileId()})); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Integration.DeleteAgentIntegration(ctx, connect.NewRequest(&tidev1.DeleteAgentIntegrationRequest{OrganizationId: w.org.ID, AgentIntegrationId: cursor.GetAgentIntegrationId()})); err != nil {
		t.Fatal(err)
	}
	var creds int
	_ = w.env.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM agent_integration_credentials WHERE agent_integration_id = $1", cursor.GetAgentIntegrationId()).Scan(&creds)
	if creds != 0 {
		t.Fatal("credentials stay after the deletion")
	}
	_, err = admin.Integration.GetAgentIntegration(ctx, connect.NewRequest(&tidev1.GetAgentIntegrationRequest{OrganizationId: w.org.ID, AgentIntegrationId: cursor.GetAgentIntegrationId()}))
	wantCode(t, "get deleted", err, connect.CodeNotFound)
}
