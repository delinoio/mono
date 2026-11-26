// SPDX-License-Identifier: FSL-1.1-ALv2

package layers_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/servers/sandrun-api/internal/api"
	"github.com/delinoio/mono/servers/sandrun-api/internal/layers"
	"github.com/delinoio/mono/servers/sandrun-api/internal/ownsandclient"
	"github.com/delinoio/mono/servers/sandrun-api/internal/ownsandclient/ownsandfake"
	"github.com/delinoio/mono/servers/sandrun-api/internal/testenv"
)

const templateKeyRejection = "OwnSand rejected the organization's OwnSand API key"

func linuxDevProfile() *sandrunv1.RunnerProfile {
	return &sandrunv1.RunnerProfile{
		Name: "dev", WorkloadKind: sandrunv1.WorkloadKind_WORKLOAD_KIND_DEV_SESSION, WorkspacePath: "/work",
		GuestOs: sandrunv1.GuestOs_GUEST_OS_LINUX, Source: &sandrunv1.RunnerProfile_BaseImage{BaseImage: sandrunv1.GuestOs_GUEST_OS_LINUX},
		Resources: &sandrunv1.Resources{VcpuCount: 2, MemoryMib: 4096, DiskGib: 32}, NetworkMode: sandrunv1.NetworkMode_NETWORK_MODE_OUTBOUND,
		IdleTtl: durationpb.New(time.Hour), MaxTtl: durationpb.New(8 * time.Hour), MaxQueueWait: durationpb.New(30 * time.Minute),
	}
}

func wantTemplateError(t *testing.T, err error, code connect.Code, message string) {
	t.Helper()
	var got *connect.Error
	if !errors.As(err, &got) || got.Code() != code || got.Message() != message {
		t.Fatalf("template check: got %v, want %s: %s", err, code, message)
	}
}

func wantProfileLinkState(t *testing.T, admin *testenv.Clients, orgID string, state sandrunv1.OwnSandLinkState) {
	t.Helper()
	got, err := admin.OwnSandLink.GetOwnSandLink(context.Background(), connect.NewRequest(&sandrunv1.GetOwnSandLinkRequest{OrganizationId: orgID}))
	if err != nil || got.Msg.GetLink().GetState() != state {
		t.Fatalf("OwnSand link: got %v (%v), want %s", got, err, state)
	}
}

func TestRunnerProfileTemplateErrorsRevokedKey(t *testing.T) {
	for _, action := range []string{"create", "source", "guest_os"} {
		t.Run(action, func(t *testing.T) {
			w := newWorld(t)
			ctx := context.Background()
			admin := w.env.User(w.org.Admin)
			osOrg, templateID := api.NewID(), api.NewID()
			source := &sandrunv1.RunnerProfile_OwnsandTemplateId{OwnsandTemplateId: templateID}
			key := ownsandKey('a')
			w.fake.AddOrg(osOrg, key)
			w.fake.SetTemplate(osOrg, templateID, ownsandv1.GuestOs_GUEST_OS_LINUX)
			if _, err := w.setLink(w.org.Admin, osOrg, key); err != nil {
				t.Fatal(err)
			}
			wantProfileLinkState(t, admin, w.org.ID, sandrunv1.OwnSandLinkState_OWN_SAND_LINK_STATE_ACTIVE)

			input := linuxDevProfile()
			create := func() (*connect.Response[sandrunv1.CreateRunnerProfileResponse], error) {
				return admin.Profile.CreateRunnerProfile(ctx, connect.NewRequest(&sandrunv1.CreateRunnerProfileRequest{OrganizationId: w.org.ID, Profile: input}))
			}
			if action == "create" {
				w.fake.RevokeKey(osOrg)
				input.Source = source
				_, err := create()
				wantTemplateError(t, err, connect.CodeFailedPrecondition, templateKeyRejection)
				wantProfileLinkState(t, admin, w.org.ID, sandrunv1.OwnSandLinkState_OWN_SAND_LINK_STATE_INVALID)
				list, err := admin.Profile.ListRunnerProfiles(ctx, connect.NewRequest(&sandrunv1.ListRunnerProfilesRequest{OrganizationId: w.org.ID}))
				if err != nil || len(list.Msg.GetProfiles()) != 0 {
					t.Fatalf("profiles after failed create: %v (%v)", list, err)
				}
				// An invalid link skips the check, even for an unknown template.
				input.Source = &sandrunv1.RunnerProfile_OwnsandTemplateId{OwnsandTemplateId: api.NewID()}
				if _, err := create(); err != nil {
					t.Fatalf("template with invalid link: %v", err)
				}
				return
			}

			update := &sandrunv1.RunnerProfile{Source: source}
			if action == "guest_os" {
				// A guest_os-only update checks OwnSand when the stored source is a template.
				input.Source = source
				update = &sandrunv1.RunnerProfile{GuestOs: sandrunv1.GuestOs_GUEST_OS_MACOS}
			}
			created, err := create()
			if err != nil {
				t.Fatal(err)
			}
			before := created.Msg.GetProfile()
			id := before.GetRunnerProfileId()
			w.fake.RevokeKey(osOrg)
			_, err = admin.Profile.UpdateRunnerProfile(ctx, connect.NewRequest(&sandrunv1.UpdateRunnerProfileRequest{
				OrganizationId: w.org.ID, RunnerProfileId: id, Profile: update, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{action}},
			}))
			wantTemplateError(t, err, connect.CodeFailedPrecondition, templateKeyRejection)
			wantProfileLinkState(t, admin, w.org.ID, sandrunv1.OwnSandLinkState_OWN_SAND_LINK_STATE_INVALID)
			got, err := admin.Profile.GetRunnerProfile(ctx, connect.NewRequest(&sandrunv1.GetRunnerProfileRequest{OrganizationId: w.org.ID, RunnerProfileId: id}))
			if err != nil || !proto.Equal(got.Msg.GetProfile(), before) {
				t.Fatalf("profile after failed update: %v (%v), want %v", got, err, before)
			}
		})
	}
}

type templateErrorFactory struct {
	ownsandclient.Factory
	failures map[string]error
}

func (f templateErrorFactory) New(org, key string) (ownsandclient.Client, error) {
	client, err := f.Factory.New(org, key)
	if err != nil {
		return nil, err
	}
	return templateErrorClient{Client: client, failures: f.failures}, nil
}

type templateErrorClient struct {
	ownsandclient.Client
	failures map[string]error
}

func (c templateErrorClient) TemplateGuestOS(ctx context.Context, id string) (ownsandv1.GuestOs, error) {
	if err := c.failures[id]; err != nil {
		return 0, err
	}
	return c.Client.TemplateGuestOS(ctx, id)
}

func TestRunnerProfileTemplateErrorsOwnSandCodes(t *testing.T) {
	const detail = "scripted OwnSand detail"
	cases := []struct {
		upstream connect.Code
		want     connect.Code
		message  string
	}{
		{connect.CodePermissionDenied, connect.CodeFailedPrecondition, templateKeyRejection},
		{connect.CodeResourceExhausted, connect.CodeResourceExhausted, "OwnSand: " + detail},
		{connect.CodeInvalidArgument, connect.CodeInternal, "internal error"},
		{connect.CodeFailedPrecondition, connect.CodeInternal, "internal error"},
		{connect.CodeAborted, connect.CodeInternal, "internal error"},
		{connect.CodeInternal, connect.CodeInternal, "internal error"},
		{connect.CodeUnknown, connect.CodeInternal, "internal error"},
		{connect.CodeNotFound, connect.CodeInvalidArgument, "invalid source.ownsand_template_id: the linked OwnSand organization has no such template"},
		{connect.CodeUnavailable, connect.CodeUnavailable, "OwnSand is unavailable; try again"},
		{connect.CodeDeadlineExceeded, connect.CodeUnavailable, "OwnSand is unavailable; try again"},
	}
	fake := ownsandfake.New()
	factory := templateErrorFactory{Factory: fake, failures: make(map[string]error)}
	ids := make(map[connect.Code]string)
	for _, tc := range cases {
		id := api.NewID()
		ids[tc.upstream] = id
		factory.failures[id] = connect.NewWireError(tc.upstream, errors.New(detail))
	}
	env := testenv.New(t, testenv.WithLayerOptions(layers.Options{OwnSand: factory}, &layers.Layers{}))
	w := &world{env: env, org: env.NewOrg("alice")}
	ctx := context.Background()
	admin := env.User(w.org.Admin)
	osOrg, key := api.NewID(), ownsandKey('a')
	fake.AddOrg(osOrg, key)
	if _, err := w.setLink(w.org.Admin, osOrg, key); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.upstream.String(), func(t *testing.T) {
			input := linuxDevProfile()
			input.Source = &sandrunv1.RunnerProfile_OwnsandTemplateId{OwnsandTemplateId: ids[tc.upstream]}
			_, err := admin.Profile.CreateRunnerProfile(ctx, connect.NewRequest(&sandrunv1.CreateRunnerProfileRequest{OrganizationId: w.org.ID, Profile: input}))
			wantTemplateError(t, err, tc.want, tc.message)
			wantProfileLinkState(t, admin, w.org.ID, sandrunv1.OwnSandLinkState_OWN_SAND_LINK_STATE_ACTIVE)
		})
	}
}

func TestRunnerProfiles(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	admin := w.env.User(w.org.Admin)
	base := func() *sandrunv1.RunnerProfile {
		return &sandrunv1.RunnerProfile{
			Name: "win-dev", WorkloadKind: sandrunv1.WorkloadKind_WORKLOAD_KIND_DEV_SESSION, WorkspacePath: `C:\work`,
			GuestOs: sandrunv1.GuestOs_GUEST_OS_WINDOWS, Source: &sandrunv1.RunnerProfile_BaseImage{BaseImage: sandrunv1.GuestOs_GUEST_OS_WINDOWS},
			Resources: &sandrunv1.Resources{VcpuCount: 4, MemoryMib: 8192, DiskGib: 64}, NetworkMode: sandrunv1.NetworkMode_NETWORK_MODE_OUTBOUND,
			IdleTtl: durationpb.New(time.Hour), MaxTtl: durationpb.New(8 * time.Hour), MaxQueueWait: durationpb.New(30 * time.Minute), WarmCount: 1,
		}
	}
	resp, err := admin.Profile.CreateRunnerProfile(ctx, connect.NewRequest(&sandrunv1.CreateRunnerProfileRequest{OrganizationId: w.org.ID, Profile: base()}))
	if err != nil {
		t.Fatal(err)
	}
	p := resp.Msg.GetProfile()
	if p.GetPriority() != sandrunv1.Priority_PRIORITY_NORMAL || p.GetGithubLabel() != "" || p.GetRunnerProfileId() == "" || p.GetBaseImage() != sandrunv1.GuestOs_GUEST_OS_WINDOWS {
		t.Fatalf("profile %v", p)
	}
	_, err = admin.Profile.CreateRunnerProfile(ctx, connect.NewRequest(&sandrunv1.CreateRunnerProfileRequest{OrganizationId: w.org.ID, Profile: base()}))
	wantCode(t, "duplicate name", err, connect.CodeAlreadyExists)
	gh := base()
	gh.Name, gh.WorkloadKind, gh.WorkspacePath = "win-ci", sandrunv1.WorkloadKind_WORKLOAD_KIND_GITHUB_JOB, ""
	ghResp, err := admin.Profile.CreateRunnerProfile(ctx, connect.NewRequest(&sandrunv1.CreateRunnerProfileRequest{OrganizationId: w.org.ID, Profile: gh}))
	if err != nil || ghResp.Msg.GetProfile().GetPriority() != sandrunv1.Priority_PRIORITY_HIGH || ghResp.Msg.GetProfile().GetGithubLabel() != "sandrun-win-ci" {
		t.Fatalf("github profile %v %v", ghResp, err)
	}
	_, err = w.env.User(w.org.Member).Profile.CreateRunnerProfile(ctx, connect.NewRequest(&sandrunv1.CreateRunnerProfileRequest{OrganizationId: w.org.ID, Profile: base()}))
	wantCode(t, "member creates", err, connect.CodePermissionDenied)
	agent := base()
	agent.Name, agent.WorkloadKind, agent.WorkspacePath, agent.AgentIntegrationId = "agent", sandrunv1.WorkloadKind_WORKLOAD_KIND_AGENT_SESSION, "", api.NewID()
	_, err = admin.Profile.CreateRunnerProfile(ctx, connect.NewRequest(&sandrunv1.CreateRunnerProfileRequest{OrganizationId: w.org.ID, Profile: agent}))
	wantCode(t, "unknown integration", err, connect.CodeNotFound)

	// List with a kind filter; members read.
	list, err := w.env.User(w.org.Member).Profile.ListRunnerProfiles(ctx, connect.NewRequest(&sandrunv1.ListRunnerProfilesRequest{OrganizationId: w.org.ID, WorkloadKind: sandrunv1.WorkloadKind_WORKLOAD_KIND_DEV_SESSION}))
	if err != nil || len(list.Msg.GetProfiles()) != 1 || list.Msg.GetProfiles()[0].GetRunnerProfileId() != p.GetRunnerProfileId() {
		t.Fatalf("list %v %v", list, err)
	}
	all, err := w.env.User(w.org.Member).Profile.ListRunnerProfiles(ctx, connect.NewRequest(&sandrunv1.ListRunnerProfilesRequest{OrganizationId: w.org.ID,
		WorkloadKind: sandrunv1.WorkloadKind_WORKLOAD_KIND_UNSPECIFIED}))
	if err != nil || len(all.Msg.GetProfiles()) != 2 {
		t.Fatalf("unfiltered profiles %v %v", all, err)
	}

	// Update: kind and integration cannot change.
	id := p.GetRunnerProfileId()
	upd := func(profile *sandrunv1.RunnerProfile, paths ...string) (*sandrunv1.RunnerProfile, error) {
		r, err := admin.Profile.UpdateRunnerProfile(ctx, connect.NewRequest(&sandrunv1.UpdateRunnerProfileRequest{OrganizationId: w.org.ID, RunnerProfileId: id,
			Profile: profile, UpdateMask: &fieldmaskpb.FieldMask{Paths: paths}}))
		if err != nil {
			return nil, err
		}
		return r.Msg.GetProfile(), nil
	}
	_, err = upd(&sandrunv1.RunnerProfile{WorkloadKind: sandrunv1.WorkloadKind_WORKLOAD_KIND_GITHUB_JOB}, "workload_kind")
	wantCode(t, "change kind", err, connect.CodeInvalidArgument)
	_, err = upd(&sandrunv1.RunnerProfile{}, "agent_integration_id")
	wantCode(t, "change integration", err, connect.CodeInvalidArgument)
	_, err = upd(&sandrunv1.RunnerProfile{GuestOs: sandrunv1.GuestOs_GUEST_OS_LINUX}, "guest_os")
	wantCode(t, "guest OS without a matching source and path", err, connect.CodeInvalidArgument)
	np, err := upd(&sandrunv1.RunnerProfile{GuestOs: sandrunv1.GuestOs_GUEST_OS_LINUX, WorkspacePath: "/work", Source: &sandrunv1.RunnerProfile_BaseImage{BaseImage: sandrunv1.GuestOs_GUEST_OS_LINUX},
		Priority: sandrunv1.Priority_PRIORITY_LOW}, "guest_os", "workspace_path", "source", "priority")
	if err != nil || np.GetGuestOs() != sandrunv1.GuestOs_GUEST_OS_LINUX || np.GetPriority() != sandrunv1.Priority_PRIORITY_LOW || np.GetWarmCount() != 1 {
		t.Fatalf("update %v %v", np, err)
	}

	// Template sources: without a link, any template ID is accepted; with a
	// link, OwnSand checks it.
	tmplOK, tmplMac := api.NewID(), api.NewID()
	_, err = upd(&sandrunv1.RunnerProfile{Source: &sandrunv1.RunnerProfile_OwnsandTemplateId{OwnsandTemplateId: tmplMac}}, "source")
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
	_, err = upd(&sandrunv1.RunnerProfile{Source: &sandrunv1.RunnerProfile_OwnsandTemplateId{OwnsandTemplateId: api.NewID()}}, "source")
	wantCode(t, "unknown template", err, connect.CodeInvalidArgument)
	_, err = upd(&sandrunv1.RunnerProfile{GuestOs: sandrunv1.GuestOs_GUEST_OS_LINUX}, "guest_os")
	wantCode(t, "template of another guest OS", err, connect.CodeInvalidArgument)
	if _, err := upd(&sandrunv1.RunnerProfile{Source: &sandrunv1.RunnerProfile_OwnsandTemplateId{OwnsandTemplateId: tmplOK}}, "source"); err != nil {
		t.Fatalf("template of the guest OS: %v", err)
	}

	// Personal API keys are rejected.
	_, secret := w.env.NewAPIKey(w.org.ID, w.org.Owner)
	_, err = w.env.Bearer(secret).Profile.ListRunnerProfiles(ctx, connect.NewRequest(&sandrunv1.ListRunnerProfilesRequest{OrganizationId: w.org.ID}))
	wantCode(t, "personal API key", err, connect.CodePermissionDenied)
	_, err = w.env.User("outsider").Profile.GetRunnerProfile(ctx, connect.NewRequest(&sandrunv1.GetRunnerProfileRequest{OrganizationId: w.org.ID, RunnerProfileId: id}))
	wantCode(t, "outsider", err, connect.CodeNotFound)
	other := w.env.NewOrg("bob")
	_, err = w.env.User("bob").Profile.GetRunnerProfile(ctx, connect.NewRequest(&sandrunv1.GetRunnerProfileRequest{OrganizationId: other.ID, RunnerProfileId: id}))
	wantCode(t, "profile of another organization", err, connect.CodeNotFound)

	if _, err := admin.Profile.DeleteRunnerProfile(ctx, connect.NewRequest(&sandrunv1.DeleteRunnerProfileRequest{OrganizationId: w.org.ID, RunnerProfileId: id})); err != nil {
		t.Fatal(err)
	}
	_, err = admin.Profile.GetRunnerProfile(ctx, connect.NewRequest(&sandrunv1.GetRunnerProfileRequest{OrganizationId: w.org.ID, RunnerProfileId: id}))
	wantCode(t, "deleted profile", err, connect.CodeNotFound)
	if countAudit(t, w.env, w.org.ID, sandrunv1.AuditAction_AUDIT_ACTION_RUNNER_PROFILE_CREATED) != 2 ||
		countAudit(t, w.env, w.org.ID, sandrunv1.AuditAction_AUDIT_ACTION_RUNNER_PROFILE_UPDATED) != 3 ||
		countAudit(t, w.env, w.org.ID, sandrunv1.AuditAction_AUDIT_ACTION_RUNNER_PROFILE_DELETED) != 1 {
		t.Fatal("audit events")
	}
}

// Two updates of different masked fields of one profile that overlap both
// stay applied.
func TestRunnerProfileWindowsWorkspacePath(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	admin := w.env.User(w.org.Admin).Profile
	profile := linuxDevProfile()
	profile.GuestOs = sandrunv1.GuestOs_GUEST_OS_WINDOWS
	profile.Source = &sandrunv1.RunnerProfile_BaseImage{BaseImage: sandrunv1.GuestOs_GUEST_OS_WINDOWS}
	profile.WorkspacePath = `C:\work\NUL`

	_, err := admin.CreateRunnerProfile(ctx, connect.NewRequest(&sandrunv1.CreateRunnerProfileRequest{
		OrganizationId: w.org.ID, Profile: profile,
	}))
	wantCode(t, "create with reserved device name", err, connect.CodeInvalidArgument)
	if !strings.Contains(err.Error(), "workspace_path") {
		t.Fatalf("create error does not name workspace_path: %v", err)
	}

	// Creating a profile with the same name confirms the rejected create did not save it.
	profile.WorkspacePath = `C:\work`
	created, err := admin.CreateRunnerProfile(ctx, connect.NewRequest(&sandrunv1.CreateRunnerProfileRequest{
		OrganizationId: w.org.ID, Profile: profile,
	}))
	if err != nil {
		t.Fatal(err)
	}
	id := created.Msg.GetProfile().GetRunnerProfileId()
	update := func(path string) (*connect.Response[sandrunv1.UpdateRunnerProfileResponse], error) {
		return admin.UpdateRunnerProfile(ctx, connect.NewRequest(&sandrunv1.UpdateRunnerProfileRequest{
			OrganizationId: w.org.ID, RunnerProfileId: id,
			Profile:    &sandrunv1.RunnerProfile{WorkspacePath: path},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"workspace_path"}},
		}))
	}
	assertStoredPath := func(want string) {
		t.Helper()
		got, err := admin.GetRunnerProfile(ctx, connect.NewRequest(&sandrunv1.GetRunnerProfileRequest{
			OrganizationId: w.org.ID, RunnerProfileId: id,
		}))
		if err != nil {
			t.Fatal(err)
		}
		if path := got.Msg.GetProfile().GetWorkspacePath(); path != want {
			t.Fatalf("stored workspace path = %q, want %q", path, want)
		}
	}

	_, err = update(`C:\work\LPT9.log`)
	wantCode(t, "update with reserved device name", err, connect.CodeInvalidArgument)
	if !strings.Contains(err.Error(), "workspace_path") {
		t.Fatalf("update error does not name workspace_path: %v", err)
	}
	assertStoredPath(`C:\work`)

	// Simulate a profile saved before reserved device names were rejected.
	if _, err := w.env.DB.Pool.Exec(ctx, "UPDATE runner_profiles SET workspace_path = $2 WHERE runner_profile_id = $1", id, `C:\work\NUL`); err != nil {
		t.Fatal(err)
	}
	repaired, err := update(`C:\work\project`)
	if err != nil || repaired.Msg.GetProfile().GetWorkspacePath() != `C:\work\project` {
		t.Fatalf("repair workspace path: %v (%v)", repaired, err)
	}
	assertStoredPath(`C:\work\project`)
}

func TestRunnerProfileConcurrentUpdates(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	admin := w.env.User(w.org.Admin)
	resp, err := admin.Profile.CreateRunnerProfile(ctx, connect.NewRequest(&sandrunv1.CreateRunnerProfileRequest{OrganizationId: w.org.ID, Profile: &sandrunv1.RunnerProfile{
		Name: "dev", WorkloadKind: sandrunv1.WorkloadKind_WORKLOAD_KIND_DEV_SESSION, WorkspacePath: "/work",
		GuestOs: sandrunv1.GuestOs_GUEST_OS_LINUX, Source: &sandrunv1.RunnerProfile_BaseImage{BaseImage: sandrunv1.GuestOs_GUEST_OS_LINUX},
		Resources: &sandrunv1.Resources{VcpuCount: 2, MemoryMib: 4096, DiskGib: 32}, NetworkMode: sandrunv1.NetworkMode_NETWORK_MODE_OUTBOUND,
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
		_, err := admin.Profile.UpdateRunnerProfile(ctx, connect.NewRequest(&sandrunv1.UpdateRunnerProfileRequest{OrganizationId: w.org.ID, RunnerProfileId: id,
			Profile: &sandrunv1.RunnerProfile{Name: "renamed"}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"name"}}}))
		errc <- err
	}()
	waitForLockWaiter(t, w.env)
	commit()
	if err := <-errc; err != nil {
		t.Fatalf("name update: %v", err)
	}
	got, err := admin.Profile.GetRunnerProfile(ctx, connect.NewRequest(&sandrunv1.GetRunnerProfileRequest{OrganizationId: w.org.ID, RunnerProfileId: id}))
	if err != nil || got.Msg.GetProfile().GetName() != "renamed" || got.Msg.GetProfile().GetWarmCount() != 3 {
		t.Fatalf("profile after both updates: %v %v", got, err)
	}
}

func TestRunnerProfileResourceUpdates(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	admin := w.env.User(w.org.Admin)
	created, err := admin.Profile.CreateRunnerProfile(ctx, connect.NewRequest(&sandrunv1.CreateRunnerProfileRequest{OrganizationId: w.org.ID, Profile: &sandrunv1.RunnerProfile{
		Name: "dev", WorkloadKind: sandrunv1.WorkloadKind_WORKLOAD_KIND_DEV_SESSION, WorkspacePath: "/work",
		GuestOs: sandrunv1.GuestOs_GUEST_OS_LINUX, Source: &sandrunv1.RunnerProfile_BaseImage{BaseImage: sandrunv1.GuestOs_GUEST_OS_LINUX},
		Resources: &sandrunv1.Resources{VcpuCount: 4, MemoryMib: 8192, DiskGib: 64}, NetworkMode: sandrunv1.NetworkMode_NETWORK_MODE_OUTBOUND,
		IdleTtl: durationpb.New(time.Hour), MaxTtl: durationpb.New(8 * time.Hour), MaxQueueWait: durationpb.New(30 * time.Minute),
	}}))
	if err != nil {
		t.Fatal(err)
	}
	id := created.Msg.GetProfile().GetRunnerProfileId()
	update := func(resources *sandrunv1.Resources, paths ...string) (*connect.Response[sandrunv1.UpdateRunnerProfileResponse], error) {
		return admin.Profile.UpdateRunnerProfile(ctx, connect.NewRequest(&sandrunv1.UpdateRunnerProfileRequest{
			OrganizationId: w.org.ID, RunnerProfileId: id,
			Profile: &sandrunv1.RunnerProfile{Resources: resources}, UpdateMask: &fieldmaskpb.FieldMask{Paths: paths},
		}))
	}
	assertStored := func(want *sandrunv1.Resources) {
		t.Helper()
		got, err := admin.Profile.GetRunnerProfile(ctx, connect.NewRequest(&sandrunv1.GetRunnerProfileRequest{OrganizationId: w.org.ID, RunnerProfileId: id}))
		if err != nil {
			t.Fatal(err)
		}
		if resources := got.Msg.GetProfile().GetResources(); !proto.Equal(resources, want) {
			t.Fatalf("stored resources = %v, want %v", resources, want)
		}
	}

	// Commit the memory change after the vCPU update tries to lock the row.
	commit := holdTx(t, w.env, "UPDATE runner_profiles SET memory_mib = 16384 WHERE runner_profile_id = $1", id)
	type result struct {
		resp *connect.Response[sandrunv1.UpdateRunnerProfileResponse]
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := update(&sandrunv1.Resources{VcpuCount: 8}, "resources.vcpu_count")
		done <- result{resp: resp, err: err}
	}()
	waitForLockWaiter(t, w.env)
	commit()
	concurrent := <-done
	if concurrent.err != nil {
		t.Fatal(concurrent.err)
	}
	want := &sandrunv1.Resources{VcpuCount: 8, MemoryMib: 16384, DiskGib: 64}
	if got := concurrent.resp.Msg.GetProfile().GetResources(); !proto.Equal(got, want) {
		t.Fatalf("resources after retry = %v, want %v", got, want)
	}
	assertStored(want)

	if _, err := update(&sandrunv1.Resources{MemoryMib: 12288, DiskGib: 96}, "resources.memory_mib", "resources.disk_gib"); err != nil {
		t.Fatal(err)
	}
	assertStored(&sandrunv1.Resources{VcpuCount: 8, MemoryMib: 12288, DiskGib: 96})

	replacement := &sandrunv1.Resources{VcpuCount: 2, MemoryMib: 4096, DiskGib: 32}
	if _, err := update(replacement, "resources"); err != nil {
		t.Fatal(err)
	}
	assertStored(replacement)

	_, err = update(&sandrunv1.Resources{}, "resources.memory_mib")
	wantCode(t, "zero memory", err, connect.CodeInvalidArgument)
	assertStored(replacement)
}

func TestRunnerProfileResourceLimitErrors(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	admin := w.env.User(w.org.Admin)
	create := func(name string, resources *sandrunv1.Resources) (*connect.Response[sandrunv1.CreateRunnerProfileResponse], error) {
		return admin.Profile.CreateRunnerProfile(ctx, connect.NewRequest(&sandrunv1.CreateRunnerProfileRequest{
			OrganizationId: w.org.ID,
			Profile: &sandrunv1.RunnerProfile{
				Name: name, WorkloadKind: sandrunv1.WorkloadKind_WORKLOAD_KIND_DEV_SESSION, WorkspacePath: "/work",
				GuestOs: sandrunv1.GuestOs_GUEST_OS_LINUX, Source: &sandrunv1.RunnerProfile_BaseImage{BaseImage: sandrunv1.GuestOs_GUEST_OS_LINUX},
				Resources: resources, NetworkMode: sandrunv1.NetworkMode_NETWORK_MODE_OUTBOUND,
				IdleTtl: durationpb.New(time.Hour), MaxTtl: durationpb.New(8 * time.Hour), MaxQueueWait: durationpb.New(30 * time.Minute),
			},
		}))
	}
	limits := &sandrunv1.Resources{VcpuCount: 1024, MemoryMib: 16777216, DiskGib: 1048576}
	created, err := create("at-limits", limits)
	if err != nil {
		t.Fatal(err)
	}
	if got := created.Msg.GetProfile().GetResources(); !proto.Equal(got, limits) {
		t.Fatalf("created resources = %v, want %v", got, limits)
	}
	update := func(resources *sandrunv1.Resources) (*connect.Response[sandrunv1.UpdateRunnerProfileResponse], error) {
		return admin.Profile.UpdateRunnerProfile(ctx, connect.NewRequest(&sandrunv1.UpdateRunnerProfileRequest{
			OrganizationId: w.org.ID, RunnerProfileId: created.Msg.GetProfile().GetRunnerProfileId(),
			Profile: &sandrunv1.RunnerProfile{Resources: resources}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"resources"}},
		}))
	}
	for _, tc := range []struct {
		name      string
		resources *sandrunv1.Resources
		want      string
	}{
		{"zero-vcpu", &sandrunv1.Resources{VcpuCount: 0, MemoryMib: 4096, DiskGib: 32}, "invalid resources.vcpu_count: must be greater than zero"},
		{"over-vcpu-limit", &sandrunv1.Resources{VcpuCount: 1025, MemoryMib: 4096, DiskGib: 32}, "invalid resources.vcpu_count: must be at most 1024"},
		{"zero-memory", &sandrunv1.Resources{VcpuCount: 2, MemoryMib: 0, DiskGib: 32}, "invalid resources.memory_mib: must be greater than zero"},
		{"over-memory-limit", &sandrunv1.Resources{VcpuCount: 2, MemoryMib: 16777217, DiskGib: 32}, "invalid resources.memory_mib: must be at most 16777216"},
		{"zero-disk", &sandrunv1.Resources{VcpuCount: 2, MemoryMib: 4096, DiskGib: 0}, "invalid resources.disk_gib: must be greater than zero"},
		{"over-disk-limit", &sandrunv1.Resources{VcpuCount: 2, MemoryMib: 4096, DiskGib: 1048577}, "invalid resources.disk_gib: must be at most 1048576"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, createErr := create(tc.name, tc.resources)
			_, updateErr := update(tc.resources)
			wantCode(t, "create", createErr, connect.CodeInvalidArgument)
			wantCode(t, "update", updateErr, connect.CodeInvalidArgument)
			want := "invalid_argument: " + tc.want
			if createErr.Error() != want || updateErr.Error() != want {
				t.Fatalf("create = %v, update = %v, want both to return %q", createErr, updateErr, want)
			}
		})
	}
	updated, err := update(limits)
	if err != nil {
		t.Fatal(err)
	}
	if got := updated.Msg.GetProfile().GetResources(); !proto.Equal(got, limits) {
		t.Fatalf("updated resources = %v, want %v", got, limits)
	}
}

// Durations are stored in milliseconds, so a value with a smaller part is
// rejected instead of being truncated.
func TestRunnerProfileSubMillisecondDurations(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	admin := w.env.User(w.org.Admin)
	base := func() *sandrunv1.RunnerProfile {
		return &sandrunv1.RunnerProfile{
			Name: "dev", WorkloadKind: sandrunv1.WorkloadKind_WORKLOAD_KIND_DEV_SESSION, WorkspacePath: "/work",
			GuestOs: sandrunv1.GuestOs_GUEST_OS_LINUX, Source: &sandrunv1.RunnerProfile_BaseImage{BaseImage: sandrunv1.GuestOs_GUEST_OS_LINUX},
			Resources: &sandrunv1.Resources{VcpuCount: 2, MemoryMib: 4096, DiskGib: 32}, NetworkMode: sandrunv1.NetworkMode_NETWORK_MODE_OUTBOUND,
			IdleTtl: durationpb.New(time.Hour), MaxTtl: durationpb.New(8 * time.Hour), MaxQueueWait: durationpb.New(30 * time.Minute),
		}
	}
	for name, change := range map[string]func(*sandrunv1.RunnerProfile){
		"idle_ttl":       func(p *sandrunv1.RunnerProfile) { p.IdleTtl = durationpb.New(500 * time.Microsecond) },
		"max_ttl":        func(p *sandrunv1.RunnerProfile) { p.MaxTtl = durationpb.New(time.Hour + time.Nanosecond) },
		"max_queue_wait": func(p *sandrunv1.RunnerProfile) { p.MaxQueueWait = durationpb.New(1500 * time.Microsecond) },
	} {
		in := base()
		change(in)
		_, err := admin.Profile.CreateRunnerProfile(ctx, connect.NewRequest(&sandrunv1.CreateRunnerProfileRequest{OrganizationId: w.org.ID, Profile: in}))
		wantCode(t, "create with sub-millisecond "+name, err, connect.CodeInvalidArgument)
	}
	resp, err := admin.Profile.CreateRunnerProfile(ctx, connect.NewRequest(&sandrunv1.CreateRunnerProfileRequest{OrganizationId: w.org.ID, Profile: base()}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = admin.Profile.UpdateRunnerProfile(ctx, connect.NewRequest(&sandrunv1.UpdateRunnerProfileRequest{OrganizationId: w.org.ID, RunnerProfileId: resp.Msg.GetProfile().GetRunnerProfileId(),
		Profile: &sandrunv1.RunnerProfile{MaxQueueWait: durationpb.New(500 * time.Microsecond)}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"max_queue_wait"}}}))
	wantCode(t, "update with sub-millisecond max_queue_wait", err, connect.CodeInvalidArgument)
}

// TestIntegrationDeleteBlockedByProfile checks that an integration cannot be
// deleted while a runner profile names it.
func TestIntegrationDeleteBlockedByProfile(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	admin := w.env.User(w.org.Admin)
	created, err := admin.Integration.CreateAgentIntegration(ctx, connect.NewRequest(&sandrunv1.CreateAgentIntegrationRequest{OrganizationId: w.org.ID,
		Vendor: sandrunv1.AgentVendor_AGENT_VENDOR_CURSOR, Name: "cursor",
		Settings:    &sandrunv1.CreateAgentIntegrationRequest_Cursor{Cursor: &sandrunv1.CursorSettings{PoolName: "pool-a"}},
		Credentials: &sandrunv1.CreateAgentIntegrationRequest_CursorCredentials{CursorCredentials: &sandrunv1.CursorCredentials{ServiceAccountApiKey: "k"}}}))
	if err != nil {
		t.Fatal(err)
	}
	cursor := created.Msg.GetIntegration()
	// Delete: blocked while a profile names the integration.
	prof, err := admin.Profile.CreateRunnerProfile(ctx, connect.NewRequest(&sandrunv1.CreateRunnerProfileRequest{OrganizationId: w.org.ID, Profile: &sandrunv1.RunnerProfile{
		Name: "cursor-linux", WorkloadKind: sandrunv1.WorkloadKind_WORKLOAD_KIND_AGENT_SESSION, AgentIntegrationId: cursor.GetAgentIntegrationId(),
		GuestOs: sandrunv1.GuestOs_GUEST_OS_LINUX, Source: &sandrunv1.RunnerProfile_BaseImage{BaseImage: sandrunv1.GuestOs_GUEST_OS_LINUX},
		Resources: &sandrunv1.Resources{VcpuCount: 2, MemoryMib: 2048, DiskGib: 10}, NetworkMode: sandrunv1.NetworkMode_NETWORK_MODE_OUTBOUND,
		IdleTtl: durationpb.New(time.Hour), MaxTtl: durationpb.New(2 * time.Hour), MaxQueueWait: durationpb.New(time.Hour)}}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = admin.Integration.DeleteAgentIntegration(ctx, connect.NewRequest(&sandrunv1.DeleteAgentIntegrationRequest{OrganizationId: w.org.ID, AgentIntegrationId: cursor.GetAgentIntegrationId()}))
	wantCode(t, "delete a used integration", err, connect.CodeFailedPrecondition)
	if _, err := admin.Profile.DeleteRunnerProfile(ctx, connect.NewRequest(&sandrunv1.DeleteRunnerProfileRequest{OrganizationId: w.org.ID, RunnerProfileId: prof.Msg.GetProfile().GetRunnerProfileId()})); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Integration.DeleteAgentIntegration(ctx, connect.NewRequest(&sandrunv1.DeleteAgentIntegrationRequest{OrganizationId: w.org.ID, AgentIntegrationId: cursor.GetAgentIntegrationId()})); err != nil {
		t.Fatal(err)
	}
	var creds int
	_ = w.env.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM agent_integration_credentials WHERE agent_integration_id = $1", cursor.GetAgentIntegrationId()).Scan(&creds)
	if creds != 0 {
		t.Fatal("credentials stay after the deletion")
	}
	_, err = admin.Integration.GetAgentIntegration(ctx, connect.NewRequest(&sandrunv1.GetAgentIntegrationRequest{OrganizationId: w.org.ID, AgentIntegrationId: cursor.GetAgentIntegrationId()}))
	wantCode(t, "get deleted", err, connect.CodeNotFound)
}
