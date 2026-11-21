// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
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
	for _, bad := range []int32{0, 99, 110, 425, 50} {
		if _, err := update(&ownsandv1.DeviceLimits{VcpuCount: 8, MemoryMib: 1024, DiskGib: 10, VcpuOvercommitPercent: bad}); testenv.Code(err) != connect.CodeInvalidArgument {
			t.Fatalf("overcommit %d: %v", bad, err)
		}
	}
	d, err := update(&ownsandv1.DeviceLimits{VcpuCount: 5, MemoryMib: 1024, DiskGib: 10, VcpuOvercommitPercent: 150})
	if err != nil {
		t.Fatal(err)
	}
	if d.GetEffectiveVcpuLimit() != 7 { // floor(5 * 150 / 100)
		t.Fatalf("effective vcpu = %d", d.GetEffectiveVcpuLimit())
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
	if events != 1 {
		t.Fatalf("config events = %d", events)
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

func errorsAs(err error, target **connect.Error) bool { return errors.As(err, target) }
