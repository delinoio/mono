// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/servers/tide-api/internal/api"
	"github.com/delinoio/mono/servers/tide-api/internal/ratelimit"
	"github.com/delinoio/mono/servers/tide-api/internal/testenv"
)

const (
	ok   connect.Code = 0
	deny              = connect.CodePermissionDenied
	nf                = connect.CodeNotFound
	// skip marks a caller that the matrix does not run for a case.
	skip connect.Code = 999
)

// matrixCase is one organization-scoped RPC with the expected code per
// caller. Outsiders and personal API keys of another organization always
// get not_found on DevSessionService and permission_denied (keys) or
// not_found (outsiders) elsewhere.
type matrixCase struct {
	name                         string
	call                         func(ctx context.Context, c *testenv.Clients, orgID string) error
	owner, admin, member, apiKey connect.Code
}

func matrixCases(org testenv.Org) []matrixCase {
	nonexistent := "01900000-0000-7000-8000-000000000000"
	mask := &fieldmaskpb.FieldMask{Paths: []string{"max_dev_sessions_per_user"}}
	return []matrixCase{
		{"GetOrganization", func(ctx context.Context, c *testenv.Clients, o string) error {
			_, err := c.Organization.GetOrganization(ctx, connect.NewRequest(&tidev1.GetOrganizationRequest{OrganizationId: o}))
			return err
		}, ok, ok, ok, deny},
		{"UpdateOrganizationSettings", func(ctx context.Context, c *testenv.Clients, o string) error {
			_, err := c.Organization.UpdateOrganizationSettings(ctx, connect.NewRequest(&tidev1.UpdateOrganizationSettingsRequest{
				OrganizationId: o, Settings: &tidev1.OrganizationSettings{}, UpdateMask: mask}))
			return err
		}, ok, ok, deny, deny},
		{"TransferOwnership", func(ctx context.Context, c *testenv.Clients, o string) error {
			_, err := c.Organization.TransferOwnership(ctx, connect.NewRequest(&tidev1.TransferOwnershipRequest{OrganizationId: o, UserId: "nobody"}))
			return err
		}, nf, deny, deny, deny},
		{"DeleteOrganization", func(ctx context.Context, c *testenv.Clients, o string) error {
			// The owner case is TestDeleteOrganizationHooks.
			_, err := c.Organization.DeleteOrganization(ctx, connect.NewRequest(&tidev1.DeleteOrganizationRequest{OrganizationId: o}))
			return err
		}, skip, deny, deny, deny},
		{"CreateInvitation", func(ctx context.Context, c *testenv.Clients, o string) error {
			_, err := c.Member.CreateInvitation(ctx, connect.NewRequest(&tidev1.CreateInvitationRequest{OrganizationId: o, Role: tidev1.Role_ROLE_MEMBER}))
			return err
		}, ok, ok, deny, deny},
		{"ListInvitations", func(ctx context.Context, c *testenv.Clients, o string) error {
			_, err := c.Member.ListInvitations(ctx, connect.NewRequest(&tidev1.ListInvitationsRequest{OrganizationId: o}))
			return err
		}, ok, ok, deny, deny},
		{"RevokeInvitation", func(ctx context.Context, c *testenv.Clients, o string) error {
			_, err := c.Member.RevokeInvitation(ctx, connect.NewRequest(&tidev1.RevokeInvitationRequest{OrganizationId: o, InvitationId: nonexistent}))
			return err
		}, nf, nf, deny, deny},
		{"ListMembers", func(ctx context.Context, c *testenv.Clients, o string) error {
			_, err := c.Member.ListMembers(ctx, connect.NewRequest(&tidev1.ListMembersRequest{OrganizationId: o}))
			return err
		}, ok, ok, ok, deny},
		{"UpdateMemberRole", func(ctx context.Context, c *testenv.Clients, o string) error {
			_, err := c.Member.UpdateMemberRole(ctx, connect.NewRequest(&tidev1.UpdateMemberRoleRequest{OrganizationId: o, UserId: org.Member, Role: tidev1.Role_ROLE_MEMBER}))
			return err
		}, ok, ok, deny, deny},
		{"RemoveMember", func(ctx context.Context, c *testenv.Clients, o string) error {
			_, err := c.Member.RemoveMember(ctx, connect.NewRequest(&tidev1.RemoveMemberRequest{OrganizationId: o, UserId: "nobody"}))
			return err
		}, nf, nf, deny, deny},
		{"CreateApiKey", func(ctx context.Context, c *testenv.Clients, o string) error {
			_, err := c.APIKey.CreateApiKey(ctx, connect.NewRequest(&tidev1.CreateApiKeyRequest{OrganizationId: o, Name: "k"}))
			return err
		}, ok, ok, ok, deny},
		{"ListApiKeys", func(ctx context.Context, c *testenv.Clients, o string) error {
			_, err := c.APIKey.ListApiKeys(ctx, connect.NewRequest(&tidev1.ListApiKeysRequest{OrganizationId: o}))
			return err
		}, ok, ok, ok, deny},
		{"RevokeApiKey", func(ctx context.Context, c *testenv.Clients, o string) error {
			_, err := c.APIKey.RevokeApiKey(ctx, connect.NewRequest(&tidev1.RevokeApiKeyRequest{OrganizationId: o, ApiKeyId: nonexistent}))
			return err
		}, nf, nf, nf, deny},
		{"ListAuditEvents", func(ctx context.Context, c *testenv.Clients, o string) error {
			_, err := c.Audit.ListAuditEvents(ctx, connect.NewRequest(&tidev1.ListAuditEventsRequest{OrganizationId: o}))
			return err
		}, ok, ok, ok, deny},
		{"DevSessionService (fake layer)", func(ctx context.Context, c *testenv.Clients, o string) error {
			return listProfiles(c, o)
		}, ok, ok, ok, ok},
	}
}

func TestAuthorizationMatrix(t *testing.T) {
	env := testenv.New(t, withFakeDevSessions())
	ctx := context.Background()
	org := env.NewOrg("alice")
	other := env.NewOrg("bob")
	_, memberKey := env.NewAPIKey(org.ID, org.Member)
	_, otherKey := env.NewAPIKey(other.ID, other.Owner)
	callers := []struct {
		name string
		c    *testenv.Clients
	}{
		{"owner", env.User(org.Owner)}, {"admin", env.User(org.Admin)}, {"member", env.User(org.Member)},
		{"api_key", env.Bearer(memberKey)}, {"outsider", env.User("outsider")}, {"other_org_api_key", env.Bearer(otherKey)},
	}
	for _, tc := range matrixCases(org) {
		for i, caller := range callers {
			want := [...]connect.Code{tc.owner, tc.admin, tc.member, tc.apiKey, nf, tc.apiKey}[i]
			if i == 5 && tc.apiKey == ok {
				// A key of another organization gets not_found where keys
				// are accepted.
				want = nf
			}
			if want == skip {
				continue
			}
			err := tc.call(ctx, caller.c, org.ID)
			if got := testenv.Code(err); got != want {
				t.Errorf("%s as %s: %v (%v), want %v", tc.name, caller.name, got, err, want)
			}
		}
	}
	// The three RPCs without organization_id reject personal API keys.
	key := env.Bearer(memberKey)
	_, err := key.Organization.CreateOrganization(ctx, connect.NewRequest(&tidev1.CreateOrganizationRequest{Name: "x"}))
	wantCode(t, "CreateOrganization with key", err, deny)
	_, err = key.Organization.ListOrganizations(ctx, connect.NewRequest(&tidev1.ListOrganizationsRequest{}))
	wantCode(t, "ListOrganizations with key", err, deny)
	_, err = key.Member.AcceptInvitation(ctx, connect.NewRequest(&tidev1.AcceptInvitationRequest{InvitationSecret: "tdi_x"}))
	wantCode(t, "AcceptInvitation with key", err, deny)
	// An invitation secret is not a bearer credential.
	inv, _ := env.User(org.Owner).Member.CreateInvitation(ctx, connect.NewRequest(&tidev1.CreateInvitationRequest{OrganizationId: org.ID, Role: tidev1.Role_ROLE_MEMBER}))
	_, err = env.Bearer(inv.Msg.GetInvitationSecret()).Organization.ListOrganizations(ctx, connect.NewRequest(&tidev1.ListOrganizationsRequest{}))
	wantCode(t, "invitation secret as bearer", err, connect.CodeUnauthenticated)
}

func tightLimits(now func() time.Time) api.Limits {
	return api.Limits{
		PerPrefix: ratelimit.New(10.0/60, 10, now),
		PerUser:   ratelimit.New(10.0/60, 10, now),
		PerClient: ratelimit.New(1000, 1000, now),
	}
}

func TestAcceptInvitationRateLimits(t *testing.T) {
	var env *testenv.Env
	clock := func() time.Time { return env.Clock.Now() }
	env = testenv.New(t, testenv.WithLimits(tightLimits(clock)))
	ctx := context.Background()
	// Per user: the 11th call in a minute is rejected, whatever the secret.
	u := env.User("u")
	for i := 0; i < 10; i++ {
		_, err := u.Member.AcceptInvitation(ctx, connect.NewRequest(&tidev1.AcceptInvitationRequest{InvitationSecret: "tdi_" + string(rune('a'+i)) + "xyz"}))
		wantCode(t, "unknown invitation", err, nf)
	}
	_, err := u.Member.AcceptInvitation(ctx, connect.NewRequest(&tidev1.AcceptInvitationRequest{InvitationSecret: "tdi_zzzz"}))
	wantCode(t, "11th call of one user", err, connect.CodeResourceExhausted)
	// Per invitation prefix: 10 users guessing one prefix exhaust it.
	for i := 0; i < 10; i++ {
		_, err := env.User(string(rune('a'+i))).Member.AcceptInvitation(ctx, connect.NewRequest(&tidev1.AcceptInvitationRequest{InvitationSecret: "tdi_samepfx"}))
		wantCode(t, "guess", err, nf)
	}
	_, err = env.User("fresh").Member.AcceptInvitation(ctx, connect.NewRequest(&tidev1.AcceptInvitationRequest{InvitationSecret: "tdi_samepfx2"}))
	wantCode(t, "11th call of one prefix", err, connect.CodeResourceExhausted)
	// Tokens refill over time.
	env.Clock.Advance(time.Minute)
	_, err = u.Member.AcceptInvitation(ctx, connect.NewRequest(&tidev1.AcceptInvitationRequest{InvitationSecret: "tdi_zzzz"}))
	wantCode(t, "after a minute", err, nf)
}

func TestAPIKeyFailureRateLimit(t *testing.T) {
	var env *testenv.Env
	clock := func() time.Time { return env.Clock.Now() }
	env = testenv.New(t, withFakeDevSessions(), testenv.WithLimits(tightLimits(clock)))
	org := env.NewOrg("alice")
	_, secret := env.NewAPIKey(org.ID, org.Member)
	// Successes take no tokens.
	for i := 0; i < 20; i++ {
		if err := listProfiles(env.Bearer(secret), org.ID); err != nil {
			t.Fatal(err)
		}
	}
	// Failures with the same display prefix take tokens; the 11th is
	// rejected before validation, even with the valid key.
	bad := secret[:9] + "AAAA"
	for i := 0; i < 10; i++ {
		wantCode(t, "bad key", listProfiles(env.Bearer(bad), org.ID), connect.CodeUnauthenticated)
	}
	wantCode(t, "11th failure", listProfiles(env.Bearer(bad), org.ID), connect.CodeResourceExhausted)
	wantCode(t, "valid key with an exhausted prefix", listProfiles(env.Bearer(secret), org.ID), connect.CodeResourceExhausted)
	env.Clock.Advance(6 * time.Second)
	if err := listProfiles(env.Bearer(secret), org.ID); err != nil {
		t.Fatalf("after refill: %v", err)
	}
}

func TestClientAddressLimit(t *testing.T) {
	var env *testenv.Env
	clock := func() time.Time { return env.Clock.Now() }
	limits := tightLimits(clock)
	limits.PerUser = ratelimit.New(1000, 1000, clock)
	limits.PerPrefix = ratelimit.New(1000, 1000, clock)
	limits.PerClient = ratelimit.New(2, 60, clock)
	env = testenv.New(t, testenv.WithLimits(limits))
	ctx := context.Background()
	// The shared client bucket: 60 at once from one address.
	for i := 0; i < 60; i++ {
		_, err := env.User("u").Member.AcceptInvitation(ctx, connect.NewRequest(&tidev1.AcceptInvitationRequest{InvitationSecret: "tdi_x"}))
		wantCode(t, "call", err, nf)
	}
	_, err := env.User("v").Member.AcceptInvitation(ctx, connect.NewRequest(&tidev1.AcceptInvitationRequest{InvitationSecret: "tdi_y"}))
	wantCode(t, "61st call of one address", err, connect.CodeResourceExhausted)
	// Outside an RPC there is no client address, so only the per-user
	// bucket of the OwnSand link validation applies.
	if err := env.App.Service.AllowOwnSandLinkValidation(ctx, "u"); err != nil {
		t.Fatalf("validation without a client address: %v", err)
	}
}
