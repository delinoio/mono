// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/accounts/accountstest"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// principalCase names a caller of the matrix test.
type principalCase int

const (
	asOwner principalCase = iota
	asAdmin
	asMember
	asAPIKey
	asOutsider       // a user without a membership
	asOtherOrgAPIKey // an API key of another organization
	asDevice         // a device session credential of the organization
)

func (p principalCase) String() string {
	return [...]string{"owner", "admin", "member", "api_key", "outsider", "other_org_api_key", "device"}[p]
}

// rpcCase is one organization-scoped RPC with the expected outcome per
// caller. A zero code means success.
type rpcCase struct {
	name string
	call func(ctx context.Context, c *testenv.Clients, orgID string) error
	// want maps owner, admin, member and api_key to the expected code.
	// Outsiders and other organizations' API keys always get not_found and
	// devices permission_denied.
	owner, admin, member, apiKey connect.Code
}

const (
	ok   connect.Code = 0
	deny              = connect.CodePermissionDenied
	// skip marks a caller that the matrix test does not run.
	skip connect.Code = 999
)

func matrixCases(env *testenv.Env, org *testenv.Org) []rpcCase {
	nonexistent := "01900000-0000-7000-8000-000000000000"
	return []rpcCase{
		{"GetOrganization", func(ctx context.Context, c *testenv.Clients, o string) error {
			_, err := c.Organization.GetOrganization(ctx, connect.NewRequest(&ownsandv1.GetOrganizationRequest{OrganizationId: o}))
			return err
		}, ok, ok, ok, deny},
		{"TransferOwnership", func(ctx context.Context, c *testenv.Clients, o string) error {
			// A nonexistent target user keeps the organization unchanged:
			// allowed callers get not_found for the member.
			_, err := c.Organization.TransferOwnership(ctx, connect.NewRequest(&ownsandv1.TransferOwnershipRequest{OrganizationId: o, UserId: "nobody"}))
			return err
		}, connect.CodeNotFound, deny, deny, deny},
		{"DeleteOrganization", func(ctx context.Context, c *testenv.Clients, o string) error {
			// Checked with a separate organization in TestDeleteOrganization;
			// here only callers that must be rejected run it.
			_, err := c.Organization.DeleteOrganization(ctx, connect.NewRequest(&ownsandv1.DeleteOrganizationRequest{OrganizationId: o}))
			return err
		}, skip, deny, deny, deny},
		{"CreateInvitation", func(ctx context.Context, c *testenv.Clients, o string) error {
			_, err := c.Member.CreateInvitation(ctx, connect.NewRequest(&ownsandv1.CreateInvitationRequest{OrganizationId: o, Role: ownsandv1.Role_ROLE_MEMBER}))
			return err
		}, ok, ok, deny, deny},
		{"ListInvitations", func(ctx context.Context, c *testenv.Clients, o string) error {
			_, err := c.Member.ListInvitations(ctx, connect.NewRequest(&ownsandv1.ListInvitationsRequest{OrganizationId: o}))
			return err
		}, ok, ok, deny, deny},
		{"RevokeInvitation", func(ctx context.Context, c *testenv.Clients, o string) error {
			_, err := c.Member.RevokeInvitation(ctx, connect.NewRequest(&ownsandv1.RevokeInvitationRequest{OrganizationId: o, InvitationId: nonexistent}))
			return err
		}, connect.CodeNotFound, connect.CodeNotFound, deny, deny},
		{"ListMembers", func(ctx context.Context, c *testenv.Clients, o string) error {
			_, err := c.Member.ListMembers(ctx, connect.NewRequest(&ownsandv1.ListMembersRequest{OrganizationId: o}))
			return err
		}, ok, ok, ok, deny},
		{"UpdateMemberRole", func(ctx context.Context, c *testenv.Clients, o string) error {
			_, err := c.Member.UpdateMemberRole(ctx, connect.NewRequest(&ownsandv1.UpdateMemberRoleRequest{OrganizationId: o, UserId: org.Member, Role: ownsandv1.Role_ROLE_MEMBER}))
			return err
		}, ok, ok, deny, deny},
		{"RemoveMember", func(ctx context.Context, c *testenv.Clients, o string) error {
			_, err := c.Member.RemoveMember(ctx, connect.NewRequest(&ownsandv1.RemoveMemberRequest{OrganizationId: o, UserId: "nobody"}))
			return err
		}, connect.CodeNotFound, connect.CodeNotFound, deny, deny},
		{"CreateApiKey", func(ctx context.Context, c *testenv.Clients, o string) error {
			_, err := c.APIKey.CreateApiKey(ctx, connect.NewRequest(&ownsandv1.CreateApiKeyRequest{OrganizationId: o, Name: "k"}))
			return err
		}, ok, ok, deny, deny},
		{"ListApiKeys", func(ctx context.Context, c *testenv.Clients, o string) error {
			_, err := c.APIKey.ListApiKeys(ctx, connect.NewRequest(&ownsandv1.ListApiKeysRequest{OrganizationId: o}))
			return err
		}, ok, ok, deny, deny},
		{"RevokeApiKey", func(ctx context.Context, c *testenv.Clients, o string) error {
			_, err := c.APIKey.RevokeApiKey(ctx, connect.NewRequest(&ownsandv1.RevokeApiKeyRequest{OrganizationId: o, ApiKeyId: nonexistent}))
			return err
		}, connect.CodeNotFound, connect.CodeNotFound, deny, deny},
		{"CreateEnrollmentToken", func(ctx context.Context, c *testenv.Clients, o string) error {
			_, err := c.Device.CreateEnrollmentToken(ctx, connect.NewRequest(&ownsandv1.CreateEnrollmentTokenRequest{OrganizationId: o, Ttl: durationpb.New(time.Hour)}))
			return err
		}, ok, ok, deny, deny},
		{"ListEnrollmentTokens", func(ctx context.Context, c *testenv.Clients, o string) error {
			_, err := c.Device.ListEnrollmentTokens(ctx, connect.NewRequest(&ownsandv1.ListEnrollmentTokensRequest{OrganizationId: o}))
			return err
		}, ok, ok, deny, deny},
		{"RevokeEnrollmentToken", func(ctx context.Context, c *testenv.Clients, o string) error {
			_, err := c.Device.RevokeEnrollmentToken(ctx, connect.NewRequest(&ownsandv1.RevokeEnrollmentTokenRequest{OrganizationId: o, EnrollmentTokenId: nonexistent}))
			return err
		}, connect.CodeNotFound, connect.CodeNotFound, deny, deny},
		{"ListDevices", func(ctx context.Context, c *testenv.Clients, o string) error {
			_, err := c.Device.ListDevices(ctx, connect.NewRequest(&ownsandv1.ListDevicesRequest{OrganizationId: o}))
			return err
		}, ok, ok, ok, ok},
		{"GetDevice", func(ctx context.Context, c *testenv.Clients, o string) error {
			_, err := c.Device.GetDevice(ctx, connect.NewRequest(&ownsandv1.GetDeviceRequest{OrganizationId: o, DeviceId: nonexistent}))
			return err
		}, connect.CodeNotFound, connect.CodeNotFound, connect.CodeNotFound, connect.CodeNotFound},
		{"UpdateDeviceLimits", func(ctx context.Context, c *testenv.Clients, o string) error {
			_, err := c.Device.UpdateDeviceLimits(ctx, connect.NewRequest(&ownsandv1.UpdateDeviceLimitsRequest{OrganizationId: o, DeviceId: nonexistent,
				Limits: &ownsandv1.DeviceLimits{VcpuCount: 1, MemoryMib: 1, DiskGib: 1, VcpuOvercommitPercent: 100}}))
			return err
		}, connect.CodeNotFound, connect.CodeNotFound, deny, deny},
		{"ReplaceDeviceLimits", func(ctx context.Context, c *testenv.Clients, o string) error {
			_, err := c.Device.ReplaceDeviceLimits(ctx, connect.NewRequest(&ownsandv1.ReplaceDeviceLimitsRequest{OrganizationId: o, DeviceId: nonexistent,
				Limits: &ownsandv1.DeviceLimitsInput{VcpuCount: i32(1), MemoryMib: i64(1), DiskGib: i64(1), VcpuOvercommitPercent: i32(100)}}))
			return err
		}, connect.CodeNotFound, connect.CodeNotFound, deny, deny},
		{"RemoveDevice", func(ctx context.Context, c *testenv.Clients, o string) error {
			_, err := c.Device.RemoveDevice(ctx, connect.NewRequest(&ownsandv1.RemoveDeviceRequest{OrganizationId: o, DeviceId: nonexistent}))
			return err
		}, connect.CodeNotFound, connect.CodeNotFound, deny, deny},
		{"GetAgentVersionPolicy", func(ctx context.Context, c *testenv.Clients, o string) error {
			_, err := c.Device.GetAgentVersionPolicy(ctx, connect.NewRequest(&ownsandv1.GetAgentVersionPolicyRequest{OrganizationId: o}))
			return err
		}, ok, ok, ok, ok},
		{"UpdateAgentVersionPolicy", func(ctx context.Context, c *testenv.Clients, o string) error {
			_, err := c.Device.UpdateAgentVersionPolicy(ctx, connect.NewRequest(&ownsandv1.UpdateAgentVersionPolicyRequest{OrganizationId: o, Mode: ownsandv1.AgentVersionPolicyMode_AGENT_VERSION_POLICY_MODE_HOLD}))
			return err
		}, ok, ok, deny, deny},
		// BillingService. The organization is linked to matrixBillingAccount
		// in the fake delino-accounts, where its owner is the billing owner.
		{"GetBillingStatus", func(ctx context.Context, c *testenv.Clients, o string) error {
			_, err := c.Billing.GetBillingStatus(ctx, connect.NewRequest(&ownsandv1.GetBillingStatusRequest{OrganizationId: o}))
			return err
		}, ok, ok, ok, ok},
		{"GetBillingUsage", func(ctx context.Context, c *testenv.Clients, o string) error {
			_, err := c.Billing.GetBillingUsage(ctx, connect.NewRequest(&ownsandv1.GetBillingUsageRequest{OrganizationId: o}))
			return err
		}, ok, ok, deny, deny},
		{"ListLinkableBillingAccounts", func(ctx context.Context, c *testenv.Clients, o string) error {
			_, err := c.Billing.ListLinkableBillingAccounts(ctx, connect.NewRequest(&ownsandv1.ListLinkableBillingAccountsRequest{OrganizationId: o}))
			return err
		}, ok, deny, deny, deny},
		{"LinkBillingAccount", func(ctx context.Context, c *testenv.Clients, o string) error {
			// Linking the linked billing account again changes nothing.
			_, err := c.Billing.LinkBillingAccount(ctx, connect.NewRequest(&ownsandv1.LinkBillingAccountRequest{OrganizationId: o, BillingAccountId: matrixBillingAccount}))
			return err
		}, ok, deny, deny, deny},
		// Last: the owner's unlink blocks the organization.
		{"UnlinkBillingAccount", func(ctx context.Context, c *testenv.Clients, o string) error {
			_, err := c.Billing.UnlinkBillingAccount(ctx, connect.NewRequest(&ownsandv1.UnlinkBillingAccountRequest{OrganizationId: o}))
			return err
		}, ok, deny, deny, deny},
	}
}

// matrixBillingAccount is the billing account of the matrix organization.
const matrixBillingAccount = "01900000-0000-7000-8000-0000000000a1"

// linkMatrixBilling links the organization to matrixBillingAccount through
// the fake delino-accounts, with the organization owner as billing owner.
func linkMatrixBilling(t *testing.T, fake *accountstest.Server, org *testenv.Org) {
	t.Helper()
	ctx := context.Background()
	fake.AddAccount(matrixBillingAccount, "Matrix", map[string]accountsv1.BillingAccountRole{org.Owner: accountsv1.BillingAccountRole_BILLING_ACCOUNT_ROLE_OWNER})
	if _, err := org.OwnerClient.Billing.UnlinkBillingAccount(ctx, connect.NewRequest(&ownsandv1.UnlinkBillingAccountRequest{OrganizationId: org.ID})); err != nil {
		t.Fatalf("unlink the seeded billing link: %v", err)
	}
	if _, err := org.OwnerClient.Billing.LinkBillingAccount(ctx, connect.NewRequest(&ownsandv1.LinkBillingAccountRequest{OrganizationId: org.ID, BillingAccountId: matrixBillingAccount})); err != nil {
		t.Fatalf("link billing: %v", err)
	}
}

// TestAuthorizationMatrix checks every organization-scoped RPC of the
// organization, member, API key, device and billing services with each
// principal kind, including cross-organization access.
func TestAuthorizationMatrix(t *testing.T) {
	fake := accountstest.New(t, nil)
	env := testenv.New(t, testenv.WithAccounts(fake))
	org := env.NewOrg("alice")
	linkMatrixBilling(t, fake, org)
	other := env.NewOrg("mallory")
	dev := env.EnrollDevice(org.ID, org.OwnerClient, ownsandv1.HostOs_HOST_OS_LINUX, ownsandv1.Arch_ARCH_AMD64)
	deviceCred := env.SessionCredential(dev)

	callers := map[principalCase]*testenv.Clients{
		asOwner:          env.User(org.Owner),
		asAdmin:          env.User(org.Admin),
		asMember:         env.User(org.Member),
		asAPIKey:         env.Clients(org.APIKey),
		asOutsider:       env.User(other.Owner),
		asOtherOrgAPIKey: env.Clients(other.APIKey),
		asDevice:         env.Clients(deviceCred),
	}
	ctx := context.Background()
	for _, rc := range matrixCases(env, org) {
		for _, who := range []principalCase{asOwner, asAdmin, asMember, asAPIKey, asOutsider, asOtherOrgAPIKey, asDevice} {
			var want connect.Code
			switch who {
			case asOwner:
				want = rc.owner
			case asAdmin:
				want = rc.admin
			case asMember:
				want = rc.member
			case asAPIKey:
				want = rc.apiKey
			case asOutsider, asOtherOrgAPIKey:
				want = connect.CodeNotFound
			case asDevice:
				want = deny
			}
			if want == skip {
				continue
			}
			t.Run(rc.name+"/"+who.String(), func(t *testing.T) {
				err := rc.call(ctx, callers[who], org.ID)
				if got := testenv.Code(err); got != want {
					t.Fatalf("code = %v, want %v (err %v)", got, want, err)
				}
			})
		}
	}
}

// TestRPCsWithoutOrganization checks the three RPCs that accept only Logto
// users.
func TestRPCsWithoutOrganization(t *testing.T) {
	env := testenv.New(t)
	org := env.NewOrg("alice")
	ctx := context.Background()
	key := env.Clients(org.APIKey)
	if _, err := key.Organization.CreateOrganization(ctx, connect.NewRequest(&ownsandv1.CreateOrganizationRequest{Name: "x"})); testenv.Code(err) != deny {
		t.Fatalf("CreateOrganization with API key: %v", err)
	}
	if _, err := key.Organization.ListOrganizations(ctx, connect.NewRequest(&ownsandv1.ListOrganizationsRequest{})); testenv.Code(err) != deny {
		t.Fatalf("ListOrganizations with API key: %v", err)
	}
	if _, err := key.Member.AcceptInvitation(ctx, connect.NewRequest(&ownsandv1.AcceptInvitationRequest{InvitationSecret: "osi_x"})); testenv.Code(err) != deny {
		t.Fatalf("AcceptInvitation with API key: %v", err)
	}
	if _, err := env.Clients("").Organization.ListOrganizations(ctx, connect.NewRequest(&ownsandv1.ListOrganizationsRequest{})); testenv.Code(err) != connect.CodeUnauthenticated {
		t.Fatalf("ListOrganizations without credential: %v", err)
	}
	if _, err := env.Clients("garbage").Organization.ListOrganizations(ctx, connect.NewRequest(&ownsandv1.ListOrganizationsRequest{})); testenv.Code(err) != connect.CodeUnauthenticated {
		t.Fatalf("ListOrganizations with an invalid token: %v", err)
	}
}
