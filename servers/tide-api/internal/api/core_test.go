// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/servers/tide-api/internal/api"
	"github.com/delinoio/mono/servers/tide-api/internal/testenv"
)

func TestOrganizationLifecycleAndPagination(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	alice := env.User("alice")
	var ids []string
	for i := 0; i < 5; i++ {
		resp, err := alice.Organization.CreateOrganization(ctx, connect.NewRequest(&tidev1.CreateOrganizationRequest{Name: "org"}))
		if err != nil {
			t.Fatal(err)
		}
		o := resp.Msg.GetOrganization()
		if o.GetCallerRole() != tidev1.Role_ROLE_OWNER || o.GetSettings() == nil || o.GetSettings().MaxDevSessionsPerUser != nil {
			t.Fatalf("created %+v", o)
		}
		ids = append(ids, o.GetOrganizationId())
	}
	var listed []string
	token := ""
	for {
		resp, err := alice.Organization.ListOrganizations(ctx, connect.NewRequest(&tidev1.ListOrganizationsRequest{PageSize: 2, PageToken: token}))
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range resp.Msg.GetOrganizations() {
			listed = append(listed, o.GetOrganizationId())
		}
		if token = resp.Msg.GetNextPageToken(); token == "" {
			break
		}
	}
	if strings.Join(listed, ",") != strings.Join(ids, ",") {
		t.Fatalf("listed %v, want %v", listed, ids)
	}
	_, err := alice.Organization.ListOrganizations(ctx, connect.NewRequest(&tidev1.ListOrganizationsRequest{PageToken: "bogus"}))
	wantCode(t, "bogus page token", err, connect.CodeInvalidArgument)
	for _, name := range []string{"", " ", strings.Repeat("x", 101), "a\nb"} {
		_, err := alice.Organization.CreateOrganization(ctx, connect.NewRequest(&tidev1.CreateOrganizationRequest{Name: name}))
		wantCode(t, "name "+name, err, connect.CodeInvalidArgument)
	}
	if _, err := alice.Organization.CreateOrganization(ctx, connect.NewRequest(&tidev1.CreateOrganizationRequest{Name: strings.Repeat("가", 100)})); err != nil {
		t.Fatalf("100 characters: %v", err)
	}
	// Another user sees none of them; a get of one is not_found.
	bob := env.User("bob")
	resp, err := bob.Organization.ListOrganizations(ctx, connect.NewRequest(&tidev1.ListOrganizationsRequest{}))
	if err != nil || len(resp.Msg.GetOrganizations()) != 0 {
		t.Fatalf("bob sees %v, %v", resp, err)
	}
	_, err = bob.Organization.GetOrganization(ctx, connect.NewRequest(&tidev1.GetOrganizationRequest{OrganizationId: ids[0]}))
	wantCode(t, "outsider get", err, connect.CodeNotFound)
	// Without a credential or with a bad token: unauthenticated.
	_, err = env.Bearer("").Organization.ListOrganizations(ctx, connect.NewRequest(&tidev1.ListOrganizationsRequest{}))
	wantCode(t, "no credential", err, connect.CodeUnauthenticated)
	_, err = env.Bearer("garbage").Organization.ListOrganizations(ctx, connect.NewRequest(&tidev1.ListOrganizationsRequest{}))
	wantCode(t, "bad token", err, connect.CodeUnauthenticated)
	if got := countAction(auditActions(t, env, ids[0]), tidev1.AuditAction_AUDIT_ACTION_ORGANIZATION_CREATED); got != 1 {
		t.Fatalf("ORGANIZATION_CREATED events = %d", got)
	}
}

func TestUpdateOrganizationSettings(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	mask := &fieldmaskpb.FieldMask{Paths: []string{"max_dev_sessions_per_user"}}
	update := func(user string, settings *tidev1.OrganizationSettings, m *fieldmaskpb.FieldMask) (*tidev1.Organization, error) {
		resp, err := env.User(user).Organization.UpdateOrganizationSettings(ctx, connect.NewRequest(&tidev1.UpdateOrganizationSettingsRequest{
			OrganizationId: org.ID, Settings: settings, UpdateMask: m}))
		if err != nil {
			return nil, err
		}
		return resp.Msg.GetOrganization(), nil
	}
	o, err := update(org.Admin, &tidev1.OrganizationSettings{MaxDevSessionsPerUser: proto.Int32(3)}, mask)
	if err != nil || o.GetSettings().GetMaxDevSessionsPerUser() != 3 || o.GetCallerRole() != tidev1.Role_ROLE_ADMIN {
		t.Fatalf("admin update: %v %v", o, err)
	}
	got, err := env.User(org.Member).Organization.GetOrganization(ctx, connect.NewRequest(&tidev1.GetOrganizationRequest{OrganizationId: org.ID}))
	if err != nil || got.Msg.GetOrganization().GetSettings().GetMaxDevSessionsPerUser() != 3 {
		t.Fatalf("member reads settings: %v %v", got, err)
	}
	_, err = update(org.Member, &tidev1.OrganizationSettings{MaxDevSessionsPerUser: proto.Int32(1)}, mask)
	wantCode(t, "member update", err, connect.CodePermissionDenied)
	for name, tc := range map[string]struct {
		s *tidev1.OrganizationSettings
		m *fieldmaskpb.FieldMask
	}{
		"zero":         {&tidev1.OrganizationSettings{MaxDevSessionsPerUser: proto.Int32(0)}, mask},
		"negative":     {&tidev1.OrganizationSettings{MaxDevSessionsPerUser: proto.Int32(-1)}, mask},
		"empty mask":   {&tidev1.OrganizationSettings{MaxDevSessionsPerUser: proto.Int32(1)}, nil},
		"unknown path": {&tidev1.OrganizationSettings{}, &fieldmaskpb.FieldMask{Paths: []string{"name"}}},
	} {
		_, err := update(org.Owner, tc.s, tc.m)
		wantCode(t, name, err, connect.CodeInvalidArgument)
	}
	// A masked path without a value clears the limit; a repeated clear
	// records no second event.
	for i := 0; i < 2; i++ {
		o, err = update(org.Owner, &tidev1.OrganizationSettings{}, mask)
		if err != nil || o.GetSettings().MaxDevSessionsPerUser != nil {
			t.Fatalf("clear: %v %v", o, err)
		}
	}
	// An omitted settings message also clears the limit.
	if _, err := update(org.Owner, &tidev1.OrganizationSettings{MaxDevSessionsPerUser: proto.Int32(2)}, mask); err != nil {
		t.Fatal(err)
	}
	o, err = update(org.Owner, nil, mask)
	if err != nil || o.GetSettings().MaxDevSessionsPerUser != nil {
		t.Fatalf("clear without settings: %v %v", o, err)
	}
	if n := countAction(auditActions(t, env, org.ID), tidev1.AuditAction_AUDIT_ACTION_ORGANIZATION_SETTINGS_CHANGED); n != 4 {
		t.Fatalf("settings events = %d, want 4", n)
	}
}

func TestInvitationsAndMembers(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	owner := env.User(org.Owner)
	inv, err := owner.Member.CreateInvitation(ctx, connect.NewRequest(&tidev1.CreateInvitationRequest{OrganizationId: org.ID, Role: tidev1.Role_ROLE_MEMBER}))
	if err != nil {
		t.Fatal(err)
	}
	secret := inv.Msg.GetInvitationSecret()
	i := inv.Msg.GetInvitation()
	if !strings.HasPrefix(secret, "tdi_") || len(secret) != 4+43 || i.GetSecretPrefix() != secret[:8] || strings.Contains(i.String(), secret) {
		t.Fatalf("invitation %v / secret of %d bytes", i, len(secret))
	}
	if i.GetExpireTime().AsTime().Sub(i.GetCreateTime().AsTime()) != 7*24*time.Hour || i.GetState() != tidev1.InvitationState_INVITATION_STATE_PENDING ||
		i.GetCreatedByUserId() != org.Owner {
		t.Fatalf("invitation %v", i)
	}
	// The database holds only the hash.
	var stored int
	if err := env.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM invitations WHERE secret_prefix = $1 AND position($2::bytea in secret_sha256) = 0", i.GetSecretPrefix(), []byte(secret)).Scan(&stored); err != nil || stored != 1 {
		t.Fatalf("stored %d, %v", stored, err)
	}
	_, err = owner.Member.CreateInvitation(ctx, connect.NewRequest(&tidev1.CreateInvitationRequest{OrganizationId: org.ID, Role: tidev1.Role_ROLE_OWNER}))
	wantCode(t, "owner invitation", err, connect.CodeInvalidArgument)
	list, err := owner.Member.ListInvitations(ctx, connect.NewRequest(&tidev1.ListInvitationsRequest{OrganizationId: org.ID}))
	if err != nil || len(list.Msg.GetInvitations()) != 1 {
		t.Fatalf("pending invitations: %v %v", list, err)
	}

	carol := env.User("carol")
	_, err = carol.Member.AcceptInvitation(ctx, connect.NewRequest(&tidev1.AcceptInvitationRequest{InvitationSecret: "tdi_bad"}))
	wantCode(t, "malformed secret", err, connect.CodeNotFound)
	acc, err := carol.Member.AcceptInvitation(ctx, connect.NewRequest(&tidev1.AcceptInvitationRequest{InvitationSecret: secret}))
	if err != nil || acc.Msg.GetMember().GetRole() != tidev1.Role_ROLE_MEMBER || acc.Msg.GetOrganization().GetOrganizationId() != org.ID {
		t.Fatalf("accept: %v %v", acc, err)
	}
	_, err = env.User("dave").Member.AcceptInvitation(ctx, connect.NewRequest(&tidev1.AcceptInvitationRequest{InvitationSecret: secret}))
	wantCode(t, "used invitation", err, connect.CodeNotFound)
	// A second invitation for an existing member.
	inv2, _ := owner.Member.CreateInvitation(ctx, connect.NewRequest(&tidev1.CreateInvitationRequest{OrganizationId: org.ID, Role: tidev1.Role_ROLE_ADMIN}))
	_, err = carol.Member.AcceptInvitation(ctx, connect.NewRequest(&tidev1.AcceptInvitationRequest{InvitationSecret: inv2.Msg.GetInvitationSecret()}))
	wantCode(t, "existing member", err, connect.CodeAlreadyExists)
	// Revocation and expiry.
	rev, err := owner.Member.RevokeInvitation(ctx, connect.NewRequest(&tidev1.RevokeInvitationRequest{OrganizationId: org.ID, InvitationId: inv2.Msg.GetInvitation().GetInvitationId()}))
	if err != nil || rev.Msg.GetInvitation().GetState() != tidev1.InvitationState_INVITATION_STATE_REVOKED {
		t.Fatalf("revoke: %v %v", rev, err)
	}
	_, err = owner.Member.RevokeInvitation(ctx, connect.NewRequest(&tidev1.RevokeInvitationRequest{OrganizationId: org.ID, InvitationId: inv2.Msg.GetInvitation().GetInvitationId()}))
	wantCode(t, "revoke twice", err, connect.CodeFailedPrecondition)
	inv3, _ := owner.Member.CreateInvitation(ctx, connect.NewRequest(&tidev1.CreateInvitationRequest{OrganizationId: org.ID, Role: tidev1.Role_ROLE_MEMBER}))
	env.Clock.Advance(7 * 24 * time.Hour)
	_, err = env.User("erin").Member.AcceptInvitation(ctx, connect.NewRequest(&tidev1.AcceptInvitationRequest{InvitationSecret: inv3.Msg.GetInvitationSecret()}))
	wantCode(t, "expired invitation", err, connect.CodeNotFound)

	// Members, roles and display names (none are wired).
	members, err := env.User(org.Member).Member.ListMembers(ctx, connect.NewRequest(&tidev1.ListMembersRequest{OrganizationId: org.ID}))
	if err != nil || len(members.Msg.GetMembers()) != 4 {
		t.Fatalf("members: %v %v", members, err)
	}
	up, err := env.User(org.Admin).Member.UpdateMemberRole(ctx, connect.NewRequest(&tidev1.UpdateMemberRoleRequest{OrganizationId: org.ID, UserId: "carol", Role: tidev1.Role_ROLE_ADMIN}))
	if err != nil || up.Msg.GetMember().GetRole() != tidev1.Role_ROLE_ADMIN {
		t.Fatalf("update role: %v %v", up, err)
	}
	_, err = env.User(org.Admin).Member.UpdateMemberRole(ctx, connect.NewRequest(&tidev1.UpdateMemberRoleRequest{OrganizationId: org.ID, UserId: org.Owner, Role: tidev1.Role_ROLE_MEMBER}))
	wantCode(t, "owner role change", err, connect.CodePermissionDenied)
	_, err = env.User(org.Admin).Member.UpdateMemberRole(ctx, connect.NewRequest(&tidev1.UpdateMemberRoleRequest{OrganizationId: org.ID, UserId: "carol", Role: tidev1.Role_ROLE_OWNER}))
	wantCode(t, "role owner", err, connect.CodeInvalidArgument)

	// Ownership transfer: the old owner becomes admin.
	_, err = env.User(org.Admin).Organization.TransferOwnership(ctx, connect.NewRequest(&tidev1.TransferOwnershipRequest{OrganizationId: org.ID, UserId: "carol"}))
	wantCode(t, "admin transfer", err, connect.CodePermissionDenied)
	_, err = owner.Organization.TransferOwnership(ctx, connect.NewRequest(&tidev1.TransferOwnershipRequest{OrganizationId: org.ID, UserId: "nobody"}))
	wantCode(t, "transfer to a non-member", err, connect.CodeNotFound)
	_, err = owner.Member.RemoveMember(ctx, connect.NewRequest(&tidev1.RemoveMemberRequest{OrganizationId: org.ID, UserId: org.Owner}))
	wantCode(t, "owner leaves", err, connect.CodeFailedPrecondition)
	if _, err := owner.Organization.TransferOwnership(ctx, connect.NewRequest(&tidev1.TransferOwnershipRequest{OrganizationId: org.ID, UserId: "carol"})); err != nil {
		t.Fatal(err)
	}
	got, _ := owner.Organization.GetOrganization(ctx, connect.NewRequest(&tidev1.GetOrganizationRequest{OrganizationId: org.ID}))
	if got.Msg.GetOrganization().GetCallerRole() != tidev1.Role_ROLE_ADMIN {
		t.Fatalf("old owner role %v", got.Msg.GetOrganization().GetCallerRole())
	}
	// The old owner can leave now; the member cannot remove others.
	_, err = env.User(org.Member).Member.RemoveMember(ctx, connect.NewRequest(&tidev1.RemoveMemberRequest{OrganizationId: org.ID, UserId: org.Admin}))
	wantCode(t, "member removes admin", err, connect.CodePermissionDenied)
	_, err = env.User(org.Admin).Member.RemoveMember(ctx, connect.NewRequest(&tidev1.RemoveMemberRequest{OrganizationId: org.ID, UserId: "carol"}))
	wantCode(t, "remove the new owner", err, connect.CodePermissionDenied)
	if _, err := owner.Member.RemoveMember(ctx, connect.NewRequest(&tidev1.RemoveMemberRequest{OrganizationId: org.ID, UserId: org.Owner})); err != nil {
		t.Fatalf("old owner leaves: %v", err)
	}
	_, err = owner.Organization.GetOrganization(ctx, connect.NewRequest(&tidev1.GetOrganizationRequest{OrganizationId: org.ID}))
	wantCode(t, "after leaving", err, connect.CodeNotFound)

	actions := auditActions(t, env, org.ID)
	for a, want := range map[tidev1.AuditAction]int{
		tidev1.AuditAction_AUDIT_ACTION_INVITATION_CREATED:    5,
		tidev1.AuditAction_AUDIT_ACTION_INVITATION_REVOKED:    1,
		tidev1.AuditAction_AUDIT_ACTION_MEMBER_JOINED:         3,
		tidev1.AuditAction_AUDIT_ACTION_MEMBER_ROLE_CHANGED:   1,
		tidev1.AuditAction_AUDIT_ACTION_OWNERSHIP_TRANSFERRED: 1,
		tidev1.AuditAction_AUDIT_ACTION_MEMBER_REMOVED:        1,
	} {
		if got := countAction(actions, a); got != want {
			t.Errorf("%v events = %d, want %d", a, got, want)
		}
	}
}

func TestAuditEventsNewestFirst(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	keyID, _ := env.NewAPIKey(org.ID, org.Member)
	var events []*tidev1.AuditEvent
	token := ""
	for {
		resp, err := env.User(org.Member).Audit.ListAuditEvents(ctx, connect.NewRequest(&tidev1.ListAuditEventsRequest{OrganizationId: org.ID, PageSize: 2, PageToken: token}))
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, resp.Msg.GetEvents()...)
		if token = resp.Msg.GetNextPageToken(); token == "" {
			break
		}
	}
	// Created, 2 invitations, 2 joins, 1 key.
	if len(events) != 6 {
		t.Fatalf("%d events", len(events))
	}
	first := events[0]
	if first.GetAction() != tidev1.AuditAction_AUDIT_ACTION_API_KEY_CREATED || first.GetTargetId() != keyID || first.GetActorUserId() != org.Member ||
		first.GetSubjectUserId() != org.Member || first.GetTargetKind() != tidev1.AuditTargetKind_AUDIT_TARGET_KIND_API_KEY {
		t.Fatalf("newest event %v", first)
	}
	if last := events[len(events)-1]; last.GetAction() != tidev1.AuditAction_AUDIT_ACTION_ORGANIZATION_CREATED {
		t.Fatalf("oldest event %v", last)
	}
	for i := 1; i < len(events); i++ {
		if events[i-1].GetAuditEventId() <= events[i].GetAuditEventId() {
			t.Fatal("events are not newest first")
		}
	}
	_, err := env.User("outsider").Audit.ListAuditEvents(ctx, connect.NewRequest(&tidev1.ListAuditEventsRequest{OrganizationId: org.ID}))
	wantCode(t, "outsider", err, connect.CodeNotFound)
	_, err = env.User(org.Member).Audit.ListAuditEvents(ctx, connect.NewRequest(&tidev1.ListAuditEventsRequest{OrganizationId: "not-a-uuid"}))
	wantCode(t, "bad organization id", err, connect.CodeInvalidArgument)
}

// TestRequestSizeLimit checks that an oversized message is rejected.
func TestRequestSizeLimit(t *testing.T) {
	env := testenv.New(t)
	_, err := env.User("alice").Organization.CreateOrganization(context.Background(), connect.NewRequest(&tidev1.CreateOrganizationRequest{Name: strings.Repeat("x", api.MaxRequestBytes+1)}))
	wantCode(t, "oversized", err, connect.CodeResourceExhausted)
}

// An admin's change of its own role that overlaps its demotion must not
// restore the admin role.
func TestSelfRoleUpdateAfterConcurrentDemotion(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	// The held update stands in for the owner's demotion that commits while
	// the admin's request runs.
	commit := holdTx(t, env, "UPDATE memberships SET role = $3 WHERE organization_id = $1 AND user_id = $2", org.ID, org.Admin, int16(tidev1.Role_ROLE_MEMBER))
	errc := make(chan error, 1)
	go func() {
		_, err := env.User(org.Admin).Member.UpdateMemberRole(ctx, connect.NewRequest(&tidev1.UpdateMemberRoleRequest{OrganizationId: org.ID, UserId: org.Admin, Role: tidev1.Role_ROLE_ADMIN}))
		errc <- err
	}()
	waitForLockWaiter(t, env)
	commit()
	wantCode(t, "self update after demotion", <-errc, connect.CodePermissionDenied)
	var role int16
	if err := env.DB.Pool.QueryRow(ctx, "SELECT role FROM memberships WHERE organization_id = $1 AND user_id = $2", org.ID, org.Admin).Scan(&role); err != nil || tidev1.Role(role) != tidev1.Role_ROLE_MEMBER {
		t.Fatalf("role = %v, %v; want ROLE_MEMBER", tidev1.Role(role), err)
	}
}
