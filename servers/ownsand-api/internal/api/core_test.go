// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/api"
	"github.com/delinoio/mono/servers/ownsand-api/internal/app"
	"github.com/delinoio/mono/servers/ownsand-api/internal/auth"
	"github.com/delinoio/mono/servers/ownsand-api/internal/ratelimit"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

func TestOrganizationLifecycleAndPagination(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	alice := env.User("alice")
	var ids []string
	for i := 0; i < 5; i++ {
		resp, err := alice.Organization.CreateOrganization(ctx, connect.NewRequest(&ownsandv1.CreateOrganizationRequest{Name: "org"}))
		if err != nil {
			t.Fatal(err)
		}
		if resp.Msg.GetOrganization().GetCallerRole() != ownsandv1.Role_ROLE_OWNER {
			t.Fatalf("caller role = %v", resp.Msg.GetOrganization().GetCallerRole())
		}
		ids = append(ids, resp.Msg.GetOrganization().GetOrganizationId())
	}
	var listed []string
	token := ""
	for {
		resp, err := alice.Organization.ListOrganizations(ctx, connect.NewRequest(&ownsandv1.ListOrganizationsRequest{PageSize: 2, PageToken: token}))
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range resp.Msg.GetOrganizations() {
			listed = append(listed, o.GetOrganizationId())
		}
		token = resp.Msg.GetNextPageToken()
		if token == "" {
			break
		}
	}
	if strings.Join(listed, ",") != strings.Join(ids, ",") {
		t.Fatalf("listed %v, want %v", listed, ids)
	}
	if _, err := alice.Organization.ListOrganizations(ctx, connect.NewRequest(&ownsandv1.ListOrganizationsRequest{PageToken: "bogus"})); testenv.Code(err) != connect.CodeInvalidArgument {
		t.Fatalf("bogus page token: %v", err)
	}
	if _, err := alice.Organization.CreateOrganization(ctx, connect.NewRequest(&ownsandv1.CreateOrganizationRequest{Name: " "})); testenv.Code(err) != connect.CodeInvalidArgument {
		t.Fatalf("blank name: %v", err)
	}
	// Another user sees none of them.
	resp, err := env.User("bob").Organization.ListOrganizations(ctx, connect.NewRequest(&ownsandv1.ListOrganizationsRequest{}))
	if err != nil || len(resp.Msg.GetOrganizations()) != 0 {
		t.Fatalf("bob sees %v, %v", resp, err)
	}
}

func TestTransferOwnershipAndLeave(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	owner := org.OwnerClient
	// The owner cannot leave.
	if _, err := owner.Member.RemoveMember(ctx, connect.NewRequest(&ownsandv1.RemoveMemberRequest{OrganizationId: org.ID, UserId: org.Owner})); testenv.Code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("owner leave: %v", err)
	}
	// An admin cannot remove or demote the owner.
	admin := env.User(org.Admin)
	if _, err := admin.Member.RemoveMember(ctx, connect.NewRequest(&ownsandv1.RemoveMemberRequest{OrganizationId: org.ID, UserId: org.Owner})); testenv.Code(err) != connect.CodePermissionDenied {
		t.Fatalf("admin removes owner: %v", err)
	}
	if _, err := admin.Member.UpdateMemberRole(ctx, connect.NewRequest(&ownsandv1.UpdateMemberRoleRequest{OrganizationId: org.ID, UserId: org.Owner, Role: ownsandv1.Role_ROLE_MEMBER})); testenv.Code(err) != connect.CodePermissionDenied {
		t.Fatalf("admin demotes owner: %v", err)
	}
	if _, err := admin.Member.UpdateMemberRole(ctx, connect.NewRequest(&ownsandv1.UpdateMemberRoleRequest{OrganizationId: org.ID, UserId: org.Member, Role: ownsandv1.Role_ROLE_OWNER})); testenv.Code(err) != connect.CodeInvalidArgument {
		t.Fatalf("admin grants owner: %v", err)
	}
	if _, err := owner.Organization.TransferOwnership(ctx, connect.NewRequest(&ownsandv1.TransferOwnershipRequest{OrganizationId: org.ID, UserId: org.Member})); err != nil {
		t.Fatal(err)
	}
	members, err := owner.Member.ListMembers(ctx, connect.NewRequest(&ownsandv1.ListMembersRequest{OrganizationId: org.ID}))
	if err != nil {
		t.Fatal(err)
	}
	roles := map[string]ownsandv1.Role{}
	for _, m := range members.Msg.GetMembers() {
		roles[m.GetUserId()] = m.GetRole()
	}
	if roles[org.Owner] != ownsandv1.Role_ROLE_ADMIN || roles[org.Member] != ownsandv1.Role_ROLE_OWNER {
		t.Fatalf("roles after transfer: %v", roles)
	}
	// The previous owner, now an admin, can leave.
	if _, err := owner.Member.RemoveMember(ctx, connect.NewRequest(&ownsandv1.RemoveMemberRequest{OrganizationId: org.ID, UserId: org.Owner})); err != nil {
		t.Fatalf("former owner leaves: %v", err)
	}
	if _, err := owner.Organization.GetOrganization(ctx, connect.NewRequest(&ownsandv1.GetOrganizationRequest{OrganizationId: org.ID})); testenv.Code(err) != connect.CodeNotFound {
		t.Fatalf("after leaving: %v", err)
	}
	var revocations int
	if err := env.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM control_events WHERE kind = 2 AND user_id = $1", org.Owner).Scan(&revocations); err != nil || revocations != 1 {
		t.Fatalf("membership revocation events = %d, %v", revocations, err)
	}
}

func TestInvitations(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	owner := org.OwnerClient
	inv, err := owner.Member.CreateInvitation(ctx, connect.NewRequest(&ownsandv1.CreateInvitationRequest{OrganizationId: org.ID, Role: ownsandv1.Role_ROLE_MEMBER}))
	if err != nil {
		t.Fatal(err)
	}
	secret := inv.Msg.GetInvitationSecret()
	if !strings.HasPrefix(secret, "osi_") || !strings.HasPrefix(secret, inv.Msg.GetInvitation().GetSecretPrefix()) {
		t.Fatalf("secret shape: prefix %q", inv.Msg.GetInvitation().GetSecretPrefix())
	}
	// An existing member gets already_exists and the invitation stays unused.
	if _, err := env.User(org.Member).Member.AcceptInvitation(ctx, connect.NewRequest(&ownsandv1.AcceptInvitationRequest{InvitationSecret: secret})); testenv.Code(err) != connect.CodeAlreadyExists {
		t.Fatalf("existing member accept: %v", err)
	}
	if _, err := env.User("carol").Member.AcceptInvitation(ctx, connect.NewRequest(&ownsandv1.AcceptInvitationRequest{InvitationSecret: secret})); err != nil {
		t.Fatalf("accept: %v", err)
	}
	// A used invitation fails with not_found.
	if _, err := env.User("dave").Member.AcceptInvitation(ctx, connect.NewRequest(&ownsandv1.AcceptInvitationRequest{InvitationSecret: secret})); testenv.Code(err) != connect.CodeNotFound {
		t.Fatalf("used invitation: %v", err)
	}
	// Revoked.
	inv2, _ := owner.Member.CreateInvitation(ctx, connect.NewRequest(&ownsandv1.CreateInvitationRequest{OrganizationId: org.ID, Role: ownsandv1.Role_ROLE_ADMIN}))
	if _, err := owner.Member.RevokeInvitation(ctx, connect.NewRequest(&ownsandv1.RevokeInvitationRequest{OrganizationId: org.ID, InvitationId: inv2.Msg.GetInvitation().GetInvitationId()})); err != nil {
		t.Fatal(err)
	}
	if _, err := env.User("dave").Member.AcceptInvitation(ctx, connect.NewRequest(&ownsandv1.AcceptInvitationRequest{InvitationSecret: inv2.Msg.GetInvitationSecret()})); testenv.Code(err) != connect.CodeNotFound {
		t.Fatalf("revoked invitation: %v", err)
	}
	// Expired after 7 days.
	inv3, _ := owner.Member.CreateInvitation(ctx, connect.NewRequest(&ownsandv1.CreateInvitationRequest{OrganizationId: org.ID, Role: ownsandv1.Role_ROLE_MEMBER}))
	env.Clock.Advance(7*24*time.Hour + time.Second)
	if _, err := env.User("dave").Member.AcceptInvitation(ctx, connect.NewRequest(&ownsandv1.AcceptInvitationRequest{InvitationSecret: inv3.Msg.GetInvitationSecret()})); testenv.Code(err) != connect.CodeNotFound {
		t.Fatalf("expired invitation: %v", err)
	}
	list, err := owner.Member.ListInvitations(ctx, connect.NewRequest(&ownsandv1.ListInvitationsRequest{OrganizationId: org.ID}))
	if err != nil || len(list.Msg.GetInvitations()) != 0 {
		t.Fatalf("pending invitations after expiry: %v, %v", list, err)
	}
	if _, err := owner.Member.CreateInvitation(ctx, connect.NewRequest(&ownsandv1.CreateInvitationRequest{OrganizationId: org.ID, Role: ownsandv1.Role_ROLE_OWNER})); testenv.Code(err) != connect.CodeInvalidArgument {
		t.Fatalf("owner invitation: %v", err)
	}
}

// countJoined returns the number of MEMBER_JOINED audit events of an organization.
func countJoined(t *testing.T, env *testenv.Env, orgID string) int {
	t.Helper()
	var n int
	if err := env.DB.Pool.QueryRow(context.Background(), "SELECT count(*) FROM audit_events WHERE organization_id = $1 AND action = $2",
		orgID, int16(ownsandv1.AuditAction_AUDIT_ACTION_MEMBER_JOINED)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// pendingInvitation reports whether the invitation still has no accept time.
func pendingInvitation(t *testing.T, env *testenv.Env, invitationID string) bool {
	t.Helper()
	var pending bool
	if err := env.DB.Pool.QueryRow(context.Background(), "SELECT accept_time IS NULL FROM invitations WHERE invitation_id = $1", invitationID).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	return pending
}

// A concurrent acceptance by the same user, which loses the race on the
// membership unique index, gets already_exists and leaves its invitation
// unused and unaudited.
func TestInvitationsConcurrentAcceptByMember(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	invB, err := org.OwnerClient.Member.CreateInvitation(ctx, connect.NewRequest(&ownsandv1.CreateInvitationRequest{OrganizationId: org.ID, Role: ownsandv1.Role_ROLE_MEMBER}))
	if err != nil {
		t.Fatal(err)
	}
	joinedBefore := countJoined(t, env, org.ID)
	// This transaction plays the first acceptance: its membership insert is
	// not committed yet, so the second acceptance passes its existence check
	// and then waits on the unique index.
	membershipID, err := uuidv7.NewString()
	if err != nil {
		t.Fatal(err)
	}
	tx, err := env.DB.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "INSERT INTO memberships (membership_id, organization_id, user_id, role, join_time) VALUES ($1, $2, 'erin', $3, now())",
		membershipID, org.ID, int16(ownsandv1.Role_ROLE_MEMBER)); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := env.User("erin").Member.AcceptInvitation(ctx, connect.NewRequest(&ownsandv1.AcceptInvitationRequest{InvitationSecret: invB.Msg.GetInvitationSecret()}))
		result <- err
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var blocked bool
		if err := env.DB.Pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query LIKE 'INSERT INTO memberships%')").Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the second acceptance never blocked on the membership insert")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-result; testenv.Code(err) != connect.CodeAlreadyExists {
		t.Fatalf("losing concurrent accept: %v", err)
	}
	var memberships int
	if err := env.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM memberships WHERE organization_id = $1 AND user_id = 'erin'", org.ID).Scan(&memberships); err != nil || memberships != 1 {
		t.Fatalf("memberships of erin = %d, %v", memberships, err)
	}
	if got := countJoined(t, env, org.ID); got != joinedBefore {
		t.Fatalf("MEMBER_JOINED events = %d, want %d", got, joinedBefore)
	}
	if !pendingInvitation(t, env, invB.Msg.GetInvitation().GetInvitationId()) {
		t.Fatal("the losing acceptance used the invitation")
	}
	if _, err := env.User("frank").Member.AcceptInvitation(ctx, connect.NewRequest(&ownsandv1.AcceptInvitationRequest{InvitationSecret: invB.Msg.GetInvitationSecret()})); err != nil {
		t.Fatalf("another user accepts the unused invitation: %v", err)
	}
}

// A unique violation other than the (organization, user) membership key
// still fails as internal and leaves the invitation pending.
func TestInvitationsUnrelatedUniqueViolation(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	other := env.NewOrg("bob")
	// gina already belongs to another organization. The test-only index
	// then makes a second membership of gina violate a unique key that is
	// not the (organization, user) key.
	env.Join(other.ID, other.OwnerClient, "gina", ownsandv1.Role_ROLE_MEMBER)
	if _, err := env.DB.Pool.Exec(ctx, "CREATE UNIQUE INDEX test_memberships_gina_only ON memberships (user_id) WHERE user_id = 'gina'"); err != nil {
		t.Fatal(err)
	}
	inv, err := org.OwnerClient.Member.CreateInvitation(ctx, connect.NewRequest(&ownsandv1.CreateInvitationRequest{OrganizationId: org.ID, Role: ownsandv1.Role_ROLE_MEMBER}))
	if err != nil {
		t.Fatal(err)
	}
	joinedBefore := countJoined(t, env, org.ID)
	if _, err := env.User("gina").Member.AcceptInvitation(ctx, connect.NewRequest(&ownsandv1.AcceptInvitationRequest{InvitationSecret: inv.Msg.GetInvitationSecret()})); testenv.Code(err) != connect.CodeInternal {
		t.Fatalf("unrelated unique violation: %v", err)
	}
	var memberships int
	if err := env.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM memberships WHERE organization_id = $1 AND user_id = 'gina'", org.ID).Scan(&memberships); err != nil || memberships != 0 {
		t.Fatalf("memberships of gina = %d, %v", memberships, err)
	}
	if got := countJoined(t, env, org.ID); got != joinedBefore {
		t.Fatalf("MEMBER_JOINED events = %d, want %d", got, joinedBefore)
	}
	if !pendingInvitation(t, env, inv.Msg.GetInvitation().GetInvitationId()) {
		t.Fatal("the failed acceptance used the invitation")
	}
}

func TestAPIKeyLifecycle(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	key := env.Clients(org.APIKey)
	if _, err := key.Device.ListDevices(ctx, connect.NewRequest(&ownsandv1.ListDevicesRequest{OrganizationId: org.ID})); err != nil {
		t.Fatalf("api key list devices: %v", err)
	}
	if _, err := org.OwnerClient.APIKey.RevokeApiKey(ctx, connect.NewRequest(&ownsandv1.RevokeApiKeyRequest{OrganizationId: org.ID, ApiKeyId: org.APIKeyID})); err != nil {
		t.Fatal(err)
	}
	if _, err := key.Device.ListDevices(ctx, connect.NewRequest(&ownsandv1.ListDevicesRequest{OrganizationId: org.ID})); testenv.Code(err) != connect.CodeUnauthenticated {
		t.Fatalf("revoked key: %v", err)
	}
	// A well-formed but unknown key and a malformed key are rejected.
	if _, err := env.Clients("osk_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA").Device.ListDevices(ctx, connect.NewRequest(&ownsandv1.ListDevicesRequest{OrganizationId: org.ID})); testenv.Code(err) != connect.CodeUnauthenticated {
		t.Fatalf("unknown key: %v", err)
	}
	// An enrollment token is not a bearer credential.
	if _, err := env.Clients("ose_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA").Device.ListDevices(ctx, connect.NewRequest(&ownsandv1.ListDevicesRequest{OrganizationId: org.ID})); testenv.Code(err) != connect.CodeUnauthenticated {
		t.Fatalf("enrollment token as bearer: %v", err)
	}
	var revoked int
	if err := env.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM control_events WHERE kind = 1 AND api_key_id = $1", org.APIKeyID).Scan(&revoked); err != nil || revoked != 1 {
		t.Fatalf("api key revocation events = %d, %v", revoked, err)
	}
}

func TestAPIKeyOrganizationIDCase(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	var org, other *testenv.Org
	// Organization IDs contain hex letters in most cases; retry until the
	// ID has a letter so that the uppercase spelling differs.
	for i := 0; i < 50; i++ {
		org = env.NewOrg("alice")
		if strings.ToUpper(org.ID) != org.ID {
			break
		}
	}
	if strings.ToUpper(org.ID) == org.ID {
		t.Skip("no organization ID with a hex letter")
	}
	other = env.NewOrg("bob")
	key := env.Clients(org.APIKey)
	list := func(id string) (*connect.Response[ownsandv1.ListDevicesResponse], error) {
		return key.Device.ListDevices(ctx, connect.NewRequest(&ownsandv1.ListDevicesRequest{OrganizationId: id}))
	}
	lower, err := list(org.ID)
	if err != nil {
		t.Fatal(err)
	}
	upper, err := list(strings.ToUpper(org.ID))
	if err != nil {
		t.Fatalf("uppercase own organization id: %v", err)
	}
	if len(lower.Msg.GetDevices()) != len(upper.Msg.GetDevices()) {
		t.Fatal("results differ between spellings")
	}
	if _, err := list(strings.ToUpper(other.ID)); testenv.Code(err) != connect.CodeNotFound {
		t.Fatalf("other organization: %v", err)
	}
	if _, err := list("not-a-uuid"); testenv.Code(err) != connect.CodeInvalidArgument {
		t.Fatalf("invalid id: %v", err)
	}
}

func TestChallengeRateLimitSharesBucketAcrossCase(t *testing.T) {
	limits := api.Limits{
		PerDevice: ratelimit.New(0.001, 2, nil),
		PerPrefix: ratelimit.New(1000, 1000, nil),
		PerClient: ratelimit.New(1000, 1000, nil),
	}
	env := testenv.New(t, func(o *app.Options) { o.Limits = &limits })
	ctx := context.Background()
	var id string
	for {
		id, _ = uuidv7.NewString()
		if strings.ToUpper(id) != id {
			break
		}
	}
	agent := env.Clients("").Agent
	call := func(d string) error {
		_, err := agent.CreateChallenge(ctx, connect.NewRequest(&ownsandv1.CreateChallengeRequest{DeviceId: d}))
		return err
	}
	if err := call(id); err != nil {
		t.Fatal(err)
	}
	if err := call(strings.ToUpper(id)); err != nil {
		t.Fatal(err)
	}
	if err := call(id); testenv.Code(err) != connect.CodeResourceExhausted {
		t.Fatalf("third spelling-mixed challenge: %v", err)
	}
}

func TestAPIKeyFailureRateLimit(t *testing.T) {
	limits := api.Limits{
		PerDevice: ratelimit.New(1000, 1000, nil),
		PerPrefix: ratelimit.New(0.001, 3, nil),
		PerClient: ratelimit.New(1000, 1000, nil),
	}
	env := testenv.New(t, func(o *app.Options) { o.Limits = &limits })
	ctx := context.Background()
	bad := env.Clients("osk_BBBBAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	for i := 0; i < 3; i++ {
		if _, err := bad.Device.ListDevices(ctx, connect.NewRequest(&ownsandv1.ListDevicesRequest{OrganizationId: "01900000-0000-7000-8000-000000000000"})); testenv.Code(err) != connect.CodeUnauthenticated {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	_, err := bad.Device.ListDevices(ctx, connect.NewRequest(&ownsandv1.ListDevicesRequest{OrganizationId: "01900000-0000-7000-8000-000000000000"}))
	if testenv.Code(err) != connect.CodeResourceExhausted {
		t.Fatalf("after failures: %v", err)
	}
	var ce *connect.Error
	if !errorsAs(err, &ce) || len(ce.Details()) == 0 {
		t.Fatalf("missing retry detail: %v", err)
	}
	v, derr := ce.Details()[0].Value()
	if detail, ok := v.(*ownsandv1.ErrorDetail); derr != nil || !ok || detail.GetRetryAfter().AsDuration() <= 0 {
		t.Fatalf("retry detail = %v, %v", v, derr)
	}
}

func TestEnrollment(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	owner := org.OwnerClient
	agent := env.Clients("").Agent
	newToken := func(ttl time.Duration) string {
		resp, err := owner.Device.CreateEnrollmentToken(ctx, connect.NewRequest(&ownsandv1.CreateEnrollmentTokenRequest{OrganizationId: org.ID, Ttl: durationpb.New(ttl)}))
		if err != nil {
			t.Fatal(err)
		}
		return resp.Msg.GetEnrollmentToken()
	}
	enroll := func(token string, pub ed25519.PublicKey) (*ownsandv1.EnrollResponse, error) {
		resp, err := agent.Enroll(ctx, connect.NewRequest(&ownsandv1.EnrollRequest{
			EnrollmentToken: token, PublicKey: pub, Name: "host", HostOs: ownsandv1.HostOs_HOST_OS_LINUX, Arch: ownsandv1.Arch_ARCH_AMD64, AgentVersion: "1.0.0",
		}))
		if err != nil {
			return nil, err
		}
		return resp.Msg, nil
	}
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	pub2, _, _ := ed25519.GenerateKey(rand.Reader)

	if _, err := owner.Device.CreateEnrollmentToken(ctx, connect.NewRequest(&ownsandv1.CreateEnrollmentTokenRequest{OrganizationId: org.ID, Ttl: durationpb.New(25 * time.Hour)})); testenv.Code(err) != connect.CodeInvalidArgument {
		t.Fatalf("ttl over 24h: %v", err)
	}
	token := newToken(time.Hour)
	first, err := enroll(token, pub)
	if err != nil {
		t.Fatal(err)
	}
	if first.GetOrganizationId() != org.ID {
		t.Fatalf("organization = %s", first.GetOrganizationId())
	}
	// A retry with the same key returns the same device.
	again, err := enroll(token, pub)
	if err != nil || again.GetDeviceId() != first.GetDeviceId() {
		t.Fatalf("retry: %v, %v", again, err)
	}
	// Reuse with another key fails like a used token.
	if _, err := enroll(token, pub2); testenv.Code(err) != connect.CodeUnauthenticated {
		t.Fatalf("reuse: %v", err)
	}
	// Revoked token.
	revoked := newToken(time.Hour)
	list, _ := owner.Device.ListEnrollmentTokens(ctx, connect.NewRequest(&ownsandv1.ListEnrollmentTokensRequest{OrganizationId: org.ID}))
	for _, tk := range list.Msg.GetTokens() {
		if tk.GetState() == ownsandv1.EnrollmentTokenState_ENROLLMENT_TOKEN_STATE_PENDING {
			if _, err := owner.Device.RevokeEnrollmentToken(ctx, connect.NewRequest(&ownsandv1.RevokeEnrollmentTokenRequest{OrganizationId: org.ID, EnrollmentTokenId: tk.GetEnrollmentTokenId()})); err != nil {
				t.Fatal(err)
			}
		} else if tk.GetState() == ownsandv1.EnrollmentTokenState_ENROLLMENT_TOKEN_STATE_USED {
			if tk.GetDeviceId() != first.GetDeviceId() {
				t.Fatalf("used token device = %s", tk.GetDeviceId())
			}
			if _, err := owner.Device.RevokeEnrollmentToken(ctx, connect.NewRequest(&ownsandv1.RevokeEnrollmentTokenRequest{OrganizationId: org.ID, EnrollmentTokenId: tk.GetEnrollmentTokenId()})); testenv.Code(err) != connect.CodeFailedPrecondition {
				t.Fatalf("revoke used token: %v", err)
			}
		}
	}
	if _, err := enroll(revoked, pub2); testenv.Code(err) != connect.CodeUnauthenticated {
		t.Fatalf("revoked: %v", err)
	}
	// Expired token.
	expired := newToken(time.Minute)
	env.Clock.Advance(2 * time.Minute)
	if _, err := enroll(expired, pub2); testenv.Code(err) != connect.CodeUnauthenticated {
		t.Fatalf("expired: %v", err)
	}
	// Garbage token and bad key.
	if _, err := enroll("ose_nope", pub2); testenv.Code(err) != connect.CodeUnauthenticated {
		t.Fatalf("garbage: %v", err)
	}
	if _, err := enroll(newToken(time.Hour), pub2[:10]); testenv.Code(err) != connect.CodeInvalidArgument {
		t.Fatalf("short key: %v", err)
	}
	// The new device starts with zero limits and 100 percent overcommit.
	dev, err := owner.Device.GetDevice(ctx, connect.NewRequest(&ownsandv1.GetDeviceRequest{OrganizationId: org.ID, DeviceId: first.GetDeviceId()}))
	if err != nil {
		t.Fatal(err)
	}
	l := dev.Msg.GetDevice().GetLimits()
	if l.GetVcpuCount() != 0 || l.GetVcpuOvercommitPercent() != 100 || dev.Msg.GetDevice().GetState() != ownsandv1.DeviceState_DEVICE_STATE_OFFLINE {
		t.Fatalf("new device = %v", dev.Msg.GetDevice())
	}
}

func TestDeviceAuthenticationAndRemoval(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	dev := env.EnrollDevice(org.ID, org.OwnerClient, ownsandv1.HostOs_HOST_OS_LINUX, ownsandv1.Arch_ARCH_AMD64)
	agent := env.Clients("").Agent

	resp, err := env.Authenticate(dev)
	if err != nil {
		t.Fatal(err)
	}
	s := resp.GetSession()
	if !strings.HasPrefix(s.GetDeviceSessionCredential(), "osd_") || s.GetExpiresIn().AsDuration() != 15*time.Minute || s.GetOrganizationId() != org.ID {
		t.Fatalf("session = %v", s)
	}

	// A nonce is single-use, bound to the device and to the purpose label.
	ch, _ := agent.CreateChallenge(ctx, connect.NewRequest(&ownsandv1.CreateChallengeRequest{DeviceId: dev.ID}))
	nonce := ch.Msg.GetNonce()
	req := &ownsandv1.AuthenticateRequest{DeviceId: dev.ID, Nonce: nonce, Signature: dev.Sign(auth.PurposeConfirmRemoval, nonce), StateEpoch: dev.Epoch}
	if _, err := agent.Authenticate(ctx, connect.NewRequest(req)); testenv.Code(err) != connect.CodeUnauthenticated {
		t.Fatalf("wrong purpose: %v", err)
	}
	req.Signature = dev.Sign(auth.PurposeAuthenticate, nonce)
	if _, err := agent.Authenticate(ctx, connect.NewRequest(req)); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Authenticate(ctx, connect.NewRequest(req)); testenv.Code(err) != connect.CodeUnauthenticated {
		t.Fatalf("replay: %v", err)
	}
	// Expired nonce.
	ch, _ = agent.CreateChallenge(ctx, connect.NewRequest(&ownsandv1.CreateChallengeRequest{DeviceId: dev.ID}))
	env.Clock.Advance(61 * time.Second)
	nonce = ch.Msg.GetNonce()
	if _, err := agent.Authenticate(ctx, connect.NewRequest(&ownsandv1.AuthenticateRequest{DeviceId: dev.ID, Nonce: nonce, Signature: dev.Sign(auth.PurposeAuthenticate, nonce), StateEpoch: dev.Epoch})); testenv.Code(err) != connect.CodeUnauthenticated {
		t.Fatalf("expired nonce: %v", err)
	}
	// Unknown device.
	unknownID, _ := uuidv7.NewString()
	ghost := &testenv.Device{ID: unknownID, PrivateKey: dev.PrivateKey, Epoch: dev.Epoch}
	if _, err := env.Authenticate(ghost); testenv.Code(err) != connect.CodeNotFound {
		t.Fatalf("unknown device: %v", err)
	}
	// ConfirmRemoval before removal.
	confirm := func(d *testenv.Device) error {
		ch, err := agent.CreateChallenge(ctx, connect.NewRequest(&ownsandv1.CreateChallengeRequest{DeviceId: d.ID}))
		if err != nil {
			return err
		}
		_, err = agent.ConfirmRemoval(ctx, connect.NewRequest(&ownsandv1.ConfirmRemovalRequest{DeviceId: d.ID, Nonce: ch.Msg.GetNonce(), Signature: d.Sign(auth.PurposeConfirmRemoval, ch.Msg.GetNonce())}))
		return err
	}
	if err := confirm(dev); testenv.Code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("confirm before removal: %v", err)
	}

	cred := env.SessionCredential(dev)
	removed, err := org.OwnerClient.Device.RemoveDevice(ctx, connect.NewRequest(&ownsandv1.RemoveDeviceRequest{OrganizationId: org.ID, DeviceId: dev.ID}))
	if err != nil || removed.Msg.GetDevice().GetState() != ownsandv1.DeviceState_DEVICE_STATE_REMOVED {
		t.Fatalf("remove: %v, %v", removed, err)
	}
	// The removal invalidated the session credential.
	if _, err := env.Clients(cred).Organization.GetOrganization(ctx, connect.NewRequest(&ownsandv1.GetOrganizationRequest{OrganizationId: org.ID})); testenv.Code(err) != connect.CodeUnauthenticated {
		t.Fatalf("removed device credential: %v", err)
	}
	// A removed device gets only the removal notice.
	resp, err = env.Authenticate(dev)
	if err != nil || resp.GetRemovalNotice() == nil || resp.GetSession() != nil {
		t.Fatalf("removed authenticate: %v, %v", resp, err)
	}
	if err := confirm(dev); err != nil {
		t.Fatalf("confirm removal: %v", err)
	}
	// After the confirmation the device is unknown.
	if err := confirm(dev); testenv.Code(err) != connect.CodeNotFound {
		t.Fatalf("confirm again: %v", err)
	}
	if _, err := env.Authenticate(dev); testenv.Code(err) != connect.CodeNotFound {
		t.Fatalf("authenticate after confirm: %v", err)
	}

	// Tombstone expiry: one year after the removal the device is unknown.
	dev2 := env.EnrollDevice(org.ID, org.OwnerClient, ownsandv1.HostOs_HOST_OS_LINUX, ownsandv1.Arch_ARCH_AMD64)
	if _, err := org.OwnerClient.Device.RemoveDevice(ctx, connect.NewRequest(&ownsandv1.RemoveDeviceRequest{OrganizationId: org.ID, DeviceId: dev2.ID})); err != nil {
		t.Fatal(err)
	}
	env.Clock.Advance(366 * 24 * time.Hour)
	if _, err := env.Authenticate(dev2); testenv.Code(err) != connect.CodeNotFound {
		t.Fatalf("expired tombstone: %v", err)
	}
}

func TestRemovedDeviceAfterOrganizationDeletion(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	dev := env.EnrollDevice(org.ID, org.OwnerClient, ownsandv1.HostOs_HOST_OS_LINUX, ownsandv1.Arch_ARCH_AMD64)
	// A sandbox and an exported snapshot record, written directly.
	sandboxID, _ := uuidv7.NewString()
	if _, err := env.DB.Pool.Exec(ctx, `INSERT INTO sandboxes (sandbox_id, organization_id, device_id, agent_state, vcpu_count, memory_mib, disk_gib,
		guest_os, host_os, arch, network_mode, idle_ttl_ms, max_ttl_ms, max_deadline, created_by_kind, created_by_id, create_time)
		VALUES ($1, $2, $3, 2, 1, 512, 1, 1, 1, 1, 1, 60000, 60000, now() + interval '1 hour', 1, 'alice', now())`, sandboxID, org.ID, dev.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := org.OwnerClient.Organization.DeleteOrganization(ctx, connect.NewRequest(&ownsandv1.DeleteOrganizationRequest{OrganizationId: org.ID})); err != nil {
		t.Fatal(err)
	}
	// The removal tombstone stays after the organization deletion.
	resp, err := env.Authenticate(dev)
	if err != nil || resp.GetRemovalNotice() == nil {
		t.Fatalf("authenticate after organization deletion: %v, %v", resp, err)
	}
	var audits, jobs, orgs int
	_ = env.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM audit_events WHERE organization_id = $1 AND action = 18 AND end_reason = 5", org.ID).Scan(&audits)
	_ = env.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM r2_cleanup_jobs WHERE prefix = $1", "organizations/"+org.ID+"/").Scan(&jobs)
	_ = env.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM organizations WHERE organization_id = $1", org.ID).Scan(&orgs)
	if audits != 1 || jobs != 1 || orgs != 0 {
		t.Fatalf("sandbox audits = %d, cleanup jobs = %d, organizations = %d", audits, jobs, orgs)
	}
	// The API key of the deleted organization no longer authenticates.
	if _, err := env.Clients(org.APIKey).Device.ListDevices(ctx, connect.NewRequest(&ownsandv1.ListDevicesRequest{OrganizationId: org.ID})); testenv.Code(err) != connect.CodeUnauthenticated {
		t.Fatalf("api key after deletion: %v", err)
	}
	if _, err := org.OwnerClient.Organization.GetOrganization(ctx, connect.NewRequest(&ownsandv1.GetOrganizationRequest{OrganizationId: org.ID})); testenv.Code(err) != connect.CodeNotFound {
		t.Fatalf("get after deletion: %v", err)
	}
}

// TestRemovedDeviceRowsHoldNoInventory removes one device with RemoveDevice
// and another with DeleteOrganization, and checks that both tombstones hold
// no inventory (also after a late capability and update-field write), keep
// their identity and expiry, and still answer the signed removal paths.
func TestRemovedDeviceRowsHoldNoInventory(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	viaRPC := env.EnrollDevice(org.ID, org.OwnerClient, ownsandv1.HostOs_HOST_OS_MACOS, ownsandv1.Arch_ARCH_ARM64)
	viaOrg := env.EnrollDevice(org.ID, org.OwnerClient, ownsandv1.HostOs_HOST_OS_LINUX, ownsandv1.Arch_ARCH_AMD64)
	keep := env.EnrollDevice(org.ID, org.OwnerClient, ownsandv1.HostOs_HOST_OS_LINUX, ownsandv1.Arch_ARCH_AMD64)
	for _, d := range []*testenv.Device{viaRPC, viaOrg, keep} {
		if _, err := org.OwnerClient.Device.UpdateDeviceLimits(ctx, connect.NewRequest(&ownsandv1.UpdateDeviceLimitsRequest{OrganizationId: org.ID, DeviceId: d.ID,
			Limits: &ownsandv1.DeviceLimits{VcpuCount: 8, MemoryMib: 4096, DiskGib: 100, VcpuOvercommitPercent: 150}})); err != nil {
			t.Fatal(err)
		}
		if _, err := env.DB.Pool.Exec(ctx, `UPDATE devices SET name = 'secret-name', agent_version = '2.3.4', update_state = 2, update_target_version = '2.4.0',
			pending_idle_since = now(), capabilities = '\x0a00', last_seen_time = now(), state_epoch = $2 WHERE device_id = $1`, d.ID, d.Epoch); err != nil {
			t.Fatal(err)
		}
	}
	inventoryRows := func(id string) (name, version, epoch string, limit, overcommit int32, caps, idle, seen bool) {
		t.Helper()
		if err := env.DB.Pool.QueryRow(ctx, `SELECT name, agent_version, state_epoch, limit_vcpu_count, vcpu_overcommit_percent,
			capabilities IS NOT NULL, pending_idle_since IS NOT NULL, last_seen_time IS NOT NULL FROM devices WHERE device_id = $1`, id).
			Scan(&name, &version, &epoch, &limit, &overcommit, &caps, &idle, &seen); err != nil {
			t.Fatal(err)
		}
		return
	}
	if name, version, _, limit, _, caps, _, _ := inventoryRows(viaRPC.ID); name != "secret-name" || version != "2.3.4" || limit != 8 || !caps {
		t.Fatalf("active device before removal holds no inventory: %q %q %d %v", name, version, limit, caps)
	}

	removed, err := org.OwnerClient.Device.RemoveDevice(ctx, connect.NewRequest(&ownsandv1.RemoveDeviceRequest{OrganizationId: org.ID, DeviceId: viaRPC.ID}))
	if err != nil {
		t.Fatal(err)
	}
	// The RemoveDevice response reads the tombstone: state and IDs stay, the
	// inventory fields are blank.
	rd := removed.Msg.GetDevice()
	if rd.GetState() != ownsandv1.DeviceState_DEVICE_STATE_REMOVED || rd.GetDeviceId() != viaRPC.ID || rd.GetOrganizationId() != org.ID ||
		rd.GetName() != "" || rd.GetAgentVersion() != "" || rd.GetCapabilities() != nil || rd.GetLimits().GetVcpuCount() != 0 || rd.GetEffectiveVcpuLimit() != 0 {
		t.Fatalf("RemoveDevice response = %v", rd)
	}
	// ListDevices(state=REMOVED) shows the same blank tombstone.
	removedState := ownsandv1.DeviceState_DEVICE_STATE_REMOVED
	list, err := org.OwnerClient.Device.ListDevices(ctx, connect.NewRequest(&ownsandv1.ListDevicesRequest{OrganizationId: org.ID, State: &removedState}))
	if err != nil || len(list.Msg.GetDevices()) != 1 || list.Msg.GetDevices()[0].GetName() != "" || list.Msg.GetDevices()[0].GetDeviceId() != viaRPC.ID {
		t.Fatalf("ListDevices(removed) = %v, %v", list, err)
	}

	// A late capability report, update fields and built images on the
	// tombstone cannot restore inventory.
	if _, err := env.DB.Pool.Exec(ctx, `UPDATE devices SET capabilities = '\x0a00', update_state = 2, update_target_version = '9.9.9', name = 'late',
		limit_vcpu_count = 16, last_seen_time = now() WHERE device_id = $1`, viaRPC.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := org.OwnerClient.Organization.DeleteOrganization(ctx, connect.NewRequest(&ownsandv1.DeleteOrganizationRequest{OrganizationId: org.ID})); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{viaRPC.ID, viaOrg.ID, keep.ID} {
		name, version, epoch, limit, overcommit, caps, idle, seen := inventoryRows(id)
		if name != "" || version != "" || epoch != "" || limit != 0 || overcommit != 100 || caps || idle || seen {
			t.Fatalf("tombstone %s holds inventory: %q %q %q %d %d %v %v %v", id, name, version, epoch, limit, overcommit, caps, idle, seen)
		}
		var state int16
		var key []byte
		var orgID string
		var removeTime, expire *time.Time
		if err := env.DB.Pool.QueryRow(ctx, "SELECT state, public_key, organization_id::text, remove_time, tombstone_expire_time FROM devices WHERE device_id = $1", id).
			Scan(&state, &key, &orgID, &removeTime, &expire); err != nil {
			t.Fatal(err)
		}
		if ownsandv1.DeviceState(state) != ownsandv1.DeviceState_DEVICE_STATE_REMOVED || len(key) != 32 || orgID != org.ID || removeTime == nil || expire == nil {
			t.Fatalf("tombstone %s identity: state %d, key %d bytes, org %s, remove %v, expire %v", id, state, len(key), orgID, removeTime, expire)
		}
	}

	// The removal notice and the signed confirmation work on a scrubbed row.
	for _, d := range []*testenv.Device{viaRPC, viaOrg} {
		resp, err := env.Authenticate(d)
		if err != nil || resp.GetRemovalNotice() == nil || resp.GetSession() != nil {
			t.Fatalf("authenticate removed device %s: %v, %v", d.ID, resp, err)
		}
	}
	agent := env.Clients("").Agent
	ch, err := agent.CreateChallenge(ctx, connect.NewRequest(&ownsandv1.CreateChallengeRequest{DeviceId: viaRPC.ID}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.ConfirmRemoval(ctx, connect.NewRequest(&ownsandv1.ConfirmRemovalRequest{DeviceId: viaRPC.ID, Nonce: ch.Msg.GetNonce(),
		Signature: viaRPC.Sign(auth.PurposeConfirmRemoval, ch.Msg.GetNonce())})); err != nil {
		t.Fatalf("confirm removal: %v", err)
	}
	var left int
	if err := env.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM devices WHERE device_id = $1", viaRPC.ID).Scan(&left); err != nil || left != 0 {
		t.Fatalf("tombstone rows after confirmation = %d, %v", left, err)
	}
	// An unconfirmed tombstone still expires.
	env.Clock.Advance(366 * 24 * time.Hour)
	if _, err := env.Authenticate(viaOrg); testenv.Code(err) != connect.CodeNotFound {
		t.Fatalf("expired tombstone: %v", err)
	}
}

func TestDeviceLimitsAndAllocation(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	dev := env.EnrollDevice(org.ID, org.OwnerClient, ownsandv1.HostOs_HOST_OS_MACOS, ownsandv1.Arch_ARCH_ARM64)
	update := func(l *ownsandv1.DeviceLimits) (*ownsandv1.Device, error) {
		resp, err := org.OwnerClient.Device.UpdateDeviceLimits(ctx, connect.NewRequest(&ownsandv1.UpdateDeviceLimitsRequest{OrganizationId: org.ID, DeviceId: dev.ID, Limits: l}))
		if err != nil {
			return nil, err
		}
		return resp.Msg.GetDevice(), nil
	}
	// A newly enrolled device has zero limits and reports a zero effective limit.
	fresh, err := org.OwnerClient.Device.GetDevice(ctx, connect.NewRequest(&ownsandv1.GetDeviceRequest{OrganizationId: org.ID, DeviceId: dev.ID}))
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Msg.GetDevice().GetEffectiveVcpuLimit() != 0 || fresh.Msg.GetDevice().GetEffectiveVcpuLimitCount() != 0 {
		t.Fatalf("new device effective = %d / %d", fresh.Msg.GetDevice().GetEffectiveVcpuLimit(), fresh.Msg.GetDevice().GetEffectiveVcpuLimitCount())
	}
	for _, bad := range []int32{0, 99, 110, 425, 50} {
		if _, err := update(&ownsandv1.DeviceLimits{VcpuCount: 8, MemoryMib: 1024, DiskGib: 10, VcpuOvercommitPercent: bad}); testenv.Code(err) != connect.CodeInvalidArgument {
			t.Fatalf("overcommit %d: %v", bad, err)
		}
	}
	d, err := update(&ownsandv1.DeviceLimits{VcpuCount: 5, MemoryMib: 1024, DiskGib: 10, VcpuOvercommitPercent: 150})
	if err != nil {
		t.Fatal(err)
	}
	if d.GetEffectiveVcpuLimit() != 7 || d.GetEffectiveVcpuLimitCount() != 7 { // floor(5 * 150 / 100)
		t.Fatalf("effective vcpu = %d / %d", d.GetEffectiveVcpuLimit(), d.GetEffectiveVcpuLimitCount())
	}
	// A limit that exceeds the int32 range is exact in the int64 field and
	// capped in the legacy field, on the update, get and list responses.
	// The last case restores the limits of the allocation checks below.
	wide := []struct {
		vcpu, percent int32
		count         int64
		legacy        int32
	}{
		{536870912, 400, 2147483648, math.MaxInt32},
		{math.MaxInt32, 400, 8589934588, math.MaxInt32},
		{math.MaxInt32, 100, math.MaxInt32, math.MaxInt32},
		{5, 150, 7, 7},
	}
	for _, tc := range wide {
		big, err := update(&ownsandv1.DeviceLimits{VcpuCount: tc.vcpu, MemoryMib: 1024, DiskGib: 10, VcpuOvercommitPercent: tc.percent})
		if err != nil {
			t.Fatalf("update %d at %d: %v", tc.vcpu, tc.percent, err)
		}
		got, err := org.OwnerClient.Device.GetDevice(ctx, connect.NewRequest(&ownsandv1.GetDeviceRequest{OrganizationId: org.ID, DeviceId: dev.ID}))
		if err != nil {
			t.Fatal(err)
		}
		list, err := org.OwnerClient.Device.ListDevices(ctx, connect.NewRequest(&ownsandv1.ListDevicesRequest{OrganizationId: org.ID}))
		if err != nil || len(list.Msg.GetDevices()) != 1 {
			t.Fatalf("list: %v, %v", list, err)
		}
		for name, d := range map[string]*ownsandv1.Device{"update": big, "get": got.Msg.GetDevice(), "list": list.Msg.GetDevices()[0]} {
			if d.GetEffectiveVcpuLimitCount() != tc.count || d.GetEffectiveVcpuLimit() != tc.legacy {
				t.Errorf("%s %d vCPU at %d percent: count = %d, legacy = %d, want %d, %d", name, tc.vcpu, tc.percent, d.GetEffectiveVcpuLimitCount(), d.GetEffectiveVcpuLimit(), tc.count, tc.legacy)
			}
		}
	}
	// Allocation counts CREATING, RUNNING, PAUSED and DELETING sandboxes, and
	// macOS guests separately; ended sandboxes do not count.
	for _, st := range []int{1, 2, 3, 4, 5, 6} {
		id, _ := uuidv7.NewString()
		if _, err := env.DB.Pool.Exec(ctx, `INSERT INTO sandboxes (sandbox_id, organization_id, device_id, agent_state, vcpu_count, memory_mib, disk_gib,
			guest_os, host_os, arch, network_mode, idle_ttl_ms, max_ttl_ms, max_deadline, created_by_kind, created_by_id, create_time)
			VALUES ($1, $2, $3, $4, 2, 100, 1, 2, 2, 2, 1, 60000, 60000, now(), 1, 'alice', now())`, id, org.ID, dev.ID, st); err != nil {
			t.Fatal(err)
		}
	}
	got, err := org.OwnerClient.Device.GetDevice(ctx, connect.NewRequest(&ownsandv1.GetDeviceRequest{OrganizationId: org.ID, DeviceId: dev.ID}))
	if err != nil {
		t.Fatal(err)
	}
	a := got.Msg.GetDevice().GetAllocation()
	if a.GetVcpuCount() != 8 || a.GetMemoryMib() != 400 || a.GetDiskGib() != 4 || a.GetMacosGuestCount() != 4 {
		t.Fatalf("allocation = %v", a)
	}
	var events int
	_ = env.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM control_events WHERE kind = 4 AND device_id = $1", dev.ID).Scan(&events)
	if want := 1 + len(wide); events != want { // every accepted update writes one
		t.Fatalf("config events = %d, want %d", events, want)
	}
}

func TestAgentVersionPolicy(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	if _, err := env.DB.Pool.Exec(ctx, "INSERT INTO agent_releases (version, host_os, arch, is_default, revoked) VALUES ('1.0.0', 1, 1, false, true), ('1.1.0', 1, 1, true, false)"); err != nil {
		t.Fatal(err)
	}
	dc := org.OwnerClient.Device
	policy, err := dc.GetAgentVersionPolicy(ctx, connect.NewRequest(&ownsandv1.GetAgentVersionPolicyRequest{OrganizationId: org.ID}))
	if err != nil || policy.Msg.GetPolicy().GetMode() != ownsandv1.AgentVersionPolicyMode_AGENT_VERSION_POLICY_MODE_FOLLOW_DEFAULT {
		t.Fatalf("default policy: %v, %v", policy, err)
	}
	if _, err := dc.UpdateAgentVersionPolicy(ctx, connect.NewRequest(&ownsandv1.UpdateAgentVersionPolicyRequest{OrganizationId: org.ID, Mode: ownsandv1.AgentVersionPolicyMode_AGENT_VERSION_POLICY_MODE_PIN, PinnedVersion: "1.0.0"})); testenv.Code(err) != connect.CodeInvalidArgument {
		t.Fatalf("pin revoked: %v", err)
	}
	if _, err := dc.UpdateAgentVersionPolicy(ctx, connect.NewRequest(&ownsandv1.UpdateAgentVersionPolicyRequest{OrganizationId: org.ID, Mode: ownsandv1.AgentVersionPolicyMode_AGENT_VERSION_POLICY_MODE_PIN})); testenv.Code(err) != connect.CodeInvalidArgument {
		t.Fatalf("pin without version: %v", err)
	}
	if _, err := dc.UpdateAgentVersionPolicy(ctx, connect.NewRequest(&ownsandv1.UpdateAgentVersionPolicyRequest{OrganizationId: org.ID})); testenv.Code(err) != connect.CodeInvalidArgument {
		t.Fatalf("unspecified mode: %v", err)
	}
	updated, err := dc.UpdateAgentVersionPolicy(ctx, connect.NewRequest(&ownsandv1.UpdateAgentVersionPolicyRequest{OrganizationId: org.ID, Mode: ownsandv1.AgentVersionPolicyMode_AGENT_VERSION_POLICY_MODE_PIN, PinnedVersion: "1.1.0"}))
	if err != nil || updated.Msg.GetPolicy().GetPinnedVersion() != "1.1.0" || updated.Msg.GetPolicy().GetUpdatedBy().GetId() != org.Owner {
		t.Fatalf("pin: %v, %v", updated, err)
	}
}

// TestAuditAndLogRedaction checks that audit events are written for admin
// actions and that no secret appears in the logs or the audit table.
func TestAuditAndLogRedaction(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	dev := env.EnrollDevice(org.ID, org.OwnerClient, ownsandv1.HostOs_HOST_OS_LINUX, ownsandv1.Arch_ARCH_AMD64)
	cred := env.SessionCredential(dev)
	tok, _ := org.OwnerClient.Device.CreateEnrollmentToken(ctx, connect.NewRequest(&ownsandv1.CreateEnrollmentTokenRequest{OrganizationId: org.ID, Ttl: durationpb.New(time.Hour)}))
	inv, _ := org.OwnerClient.Member.CreateInvitation(ctx, connect.NewRequest(&ownsandv1.CreateInvitationRequest{OrganizationId: org.ID, Role: ownsandv1.Role_ROLE_MEMBER}))

	logs := env.Logs.String()
	for name, secret := range map[string]string{
		"api key": org.APIKey, "device credential": cred, "enrollment token": tok.Msg.GetEnrollmentToken(),
		"invitation": inv.Msg.GetInvitationSecret(), "user token": testenv.UserToken(org.Owner),
	} {
		if secret == "" || strings.Contains(logs, secret) {
			t.Fatalf("%s appears in the logs or is empty", name)
		}
	}
	if !strings.Contains(logs, `"procedure":"/delino.ownsand.v1.OrganizationService/CreateOrganization"`) {
		t.Fatalf("missing RPC log record")
	}
	rows, err := env.DB.Pool.Query(ctx, "SELECT action FROM audit_events WHERE organization_id = $1 ORDER BY create_time, audit_event_id", org.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := map[ownsandv1.AuditAction]int{}
	for rows.Next() {
		var a int16
		_ = rows.Scan(&a)
		seen[ownsandv1.AuditAction(a)]++
	}
	for _, want := range []ownsandv1.AuditAction{
		ownsandv1.AuditAction_AUDIT_ACTION_ORGANIZATION_CREATED,
		ownsandv1.AuditAction_AUDIT_ACTION_INVITATION_CREATED,
		ownsandv1.AuditAction_AUDIT_ACTION_MEMBER_JOINED,
		ownsandv1.AuditAction_AUDIT_ACTION_API_KEY_CREATED,
		ownsandv1.AuditAction_AUDIT_ACTION_ENROLLMENT_TOKEN_CREATED,
		ownsandv1.AuditAction_AUDIT_ACTION_DEVICE_ENROLLED,
	} {
		if seen[want] == 0 {
			t.Fatalf("missing audit action %v (have %v)", want, seen)
		}
	}
}

func TestChallengeRateLimit(t *testing.T) {
	limits := api.Limits{
		PerDevice: ratelimit.New(0.001, 2, nil),
		PerPrefix: ratelimit.New(1000, 1000, nil),
		PerClient: ratelimit.New(1000, 1000, nil),
	}
	env := testenv.New(t, func(o *app.Options) { o.Limits = &limits })
	ctx := context.Background()
	id, _ := uuidv7.NewString()
	agent := env.Clients("").Agent
	for i := 0; i < 2; i++ {
		if _, err := agent.CreateChallenge(ctx, connect.NewRequest(&ownsandv1.CreateChallengeRequest{DeviceId: id})); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := agent.CreateChallenge(ctx, connect.NewRequest(&ownsandv1.CreateChallengeRequest{DeviceId: id})); testenv.Code(err) != connect.CodeResourceExhausted {
		t.Fatalf("third challenge: %v", err)
	}
}

// clientBucketLimits returns limits with a two-token client bucket that
// refills one token per second on the test clock and generous per-device and
// per-prefix limits, so only the shared per-client bucket can reject.
func clientBucketLimits(o *app.Options) {
	o.TrustProxyClientAddress = true
	o.Limits = &api.Limits{
		PerDevice: ratelimit.New(1000, 1000, o.Now),
		PerPrefix: ratelimit.New(1000, 1000, o.Now),
		PerClient: ratelimit.New(1, 2, o.Now),
	}
}

// fromAddress returns clients whose requests carry the proxy-appended client
// address addr.
func fromAddress(env *testenv.Env, token, addr string) *testenv.Clients {
	return env.ClientsWith(token, http.Header{"X-Forwarded-For": []string{addr}})
}

// rateLimitRetry returns the retry delay of a resource_exhausted error, or 0
// when err is another error.
func rateLimitRetry(t *testing.T, err error) time.Duration {
	t.Helper()
	var ce *connect.Error
	if testenv.Code(err) != connect.CodeResourceExhausted || !errorsAs(err, &ce) || len(ce.Details()) == 0 {
		return 0
	}
	v, derr := ce.Details()[0].Value()
	detail, ok := v.(*ownsandv1.ErrorDetail)
	if derr != nil || !ok {
		t.Fatalf("retry detail = %v, %v", v, derr)
	}
	return detail.GetRetryAfter().AsDuration()
}

// TestClientRateLimitSharedBucket checks that device RPCs, AcceptInvitation
// and failed API key validations draw from one per-client-address bucket.
func TestClientRateLimitSharedBucket(t *testing.T) {
	env := testenv.New(t, clientBucketLimits)
	ctx := context.Background()
	org := env.NewOrg("alice")
	newInvitation := func() string {
		inv, err := org.OwnerClient.Member.CreateInvitation(ctx, connect.NewRequest(&ownsandv1.CreateInvitationRequest{OrganizationId: org.ID, Role: ownsandv1.Role_ROLE_MEMBER}))
		if err != nil {
			t.Fatal(err)
		}
		return inv.Msg.GetInvitationSecret()
	}
	users := 0
	// Each category makes one request that passes its other limits. It returns
	// the error of the request and whether the request is otherwise eligible.
	type category struct {
		name string
		call func(addr string) error
		// ok reports whether err is the answer of a request that the limiter let
		// through.
		ok func(err error) bool
	}
	categories := []category{
		{"device RPC", func(addr string) error {
			id, _ := uuidv7.NewString()
			_, err := fromAddress(env, "", addr).Agent.CreateChallenge(ctx, connect.NewRequest(&ownsandv1.CreateChallengeRequest{DeviceId: id}))
			return err
		}, func(err error) bool { return err == nil }},
		{"AcceptInvitation", func(addr string) error {
			users++
			secret := newInvitation()
			_, err := fromAddress(env, testenv.UserToken(fmt.Sprintf("joiner-%d", users)), addr).Member.AcceptInvitation(ctx, connect.NewRequest(&ownsandv1.AcceptInvitationRequest{InvitationSecret: secret}))
			return err
		}, func(err error) bool { return err == nil }},
		{"failed API key validation", func(addr string) error {
			_, err := fromAddress(env, "osk_BBBBAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", addr).Device.ListDevices(ctx, connect.NewRequest(&ownsandv1.ListDevicesRequest{OrganizationId: org.ID}))
			return err
		}, func(err error) bool { return testenv.Code(err) == connect.CodeUnauthenticated }},
	}
	orders := [][3]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}}
	for n, order := range orders {
		addr := fmt.Sprintf("203.0.113.%d", n+1)
		first, second, third := categories[order[0]], categories[order[1]], categories[order[2]]
		t.Run(first.name+" then "+second.name+" then "+third.name, func(t *testing.T) {
			for _, c := range []category{first, second} {
				if err := c.call(addr); !c.ok(err) {
					t.Fatalf("%s before exhaustion: %v", c.name, err)
				}
			}
			err := third.call(addr)
			retry := rateLimitRetry(t, err)
			if retry <= 0 {
				t.Fatalf("%s after two other categories: want resource_exhausted with positive retry_after, got %v", third.name, err)
			}
			// The rejected request took no token: one second on the fixed clock
			// refills exactly one.
			env.Clock.Advance(retry)
			if err := third.call(addr); !third.ok(err) {
				t.Fatalf("%s after waiting %s: %v", third.name, retry, err)
			}
		})
	}

	t.Run("other address keeps its allowance", func(t *testing.T) {
		const exhausted, other = "198.51.100.1", "198.51.100.2"
		for _, c := range categories[:2] {
			if err := c.call(exhausted); !c.ok(err) {
				t.Fatalf("%s: %v", c.name, err)
			}
		}
		if rateLimitRetry(t, categories[2].call(exhausted)) <= 0 {
			t.Fatal("the exhausted address is not limited")
		}
		for _, c := range categories {
			if err := c.call(other); !c.ok(err) {
				t.Fatalf("%s from another address: %v", c.name, err)
			}
			env.Clock.Advance(time.Second)
		}
	})

	t.Run("valid API key validation takes no token", func(t *testing.T) {
		const addr = "198.51.100.3"
		keyClient := fromAddress(env, org.APIKey, addr)
		for i := 0; i < 5; i++ {
			if _, err := keyClient.Device.ListDevices(ctx, connect.NewRequest(&ownsandv1.ListDevicesRequest{OrganizationId: org.ID})); err != nil {
				t.Fatalf("valid API key call %d: %v", i, err)
			}
		}
		for _, c := range categories[:2] {
			if err := c.call(addr); !c.ok(err) {
				t.Fatalf("%s after valid API key calls: %v", c.name, err)
			}
		}
		if rateLimitRetry(t, categories[2].call(addr)) <= 0 {
			t.Fatal("two charged calls do not exhaust the bucket")
		}
	})
}

// TestClientRateLimitUnresolvedAddressAndOtherLimits checks that a request
// without a resolved client address skips only the client bucket and that
// the per-device limit still applies next to it.
func TestClientRateLimitUnresolvedAddressAndOtherLimits(t *testing.T) {
	env := testenv.New(t, func(o *app.Options) {
		clientBucketLimits(o)
		o.Limits.PerDevice = ratelimit.New(0.001, 3, o.Now)
	})
	ctx := context.Background()
	challenge := func(c *testenv.Clients, id string) error {
		_, err := c.Agent.CreateChallenge(ctx, connect.NewRequest(&ownsandv1.CreateChallengeRequest{DeviceId: id}))
		return err
	}
	// Behind a trusted proxy, a request without a valid X-Forwarded-For entry
	// has no client address and takes no client token.
	noAddr := env.Clients("")
	for i := 0; i < 3; i++ {
		id, _ := uuidv7.NewString()
		if err := challenge(noAddr, id); err != nil {
			t.Fatalf("request %d without address: %v", i, err)
		}
	}
	// The per-device limit (burst 3) rejects the fourth challenge for one
	// device although each client address (burst 2) still has tokens left.
	same, _ := uuidv7.NewString()
	for i := 0; i < 3; i++ {
		if err := challenge(fromAddress(env, "", fmt.Sprintf("192.0.2.%d", i+1)), same); err != nil {
			t.Fatalf("device call %d: %v", i, err)
		}
	}
	if rateLimitRetry(t, challenge(fromAddress(env, "", "192.0.2.4"), same)) <= 0 {
		t.Fatal("the per-device limit does not reject the fourth call of one device")
	}
}

func errorsAs(err error, target **connect.Error) bool { return errors.As(err, target) }
