// SPDX-License-Identifier: FSL-1.1-ALv2

package app

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
	"github.com/delinoio/mono/servers/ownsand-api/internal/secret"
)

type clients struct {
	org    ownsandv1connect.OrganizationServiceClient
	member ownsandv1connect.MemberServiceClient
	keys   ownsandv1connect.ApiKeyServiceClient
	device ownsandv1connect.DeviceServiceClient
}

func (h *harness) clients(credential string) clients {
	opt := bearer(credential)
	return clients{
		org:    ownsandv1connect.NewOrganizationServiceClient(h.client, h.url, opt),
		member: ownsandv1connect.NewMemberServiceClient(h.client, h.url, opt),
		keys:   ownsandv1connect.NewApiKeyServiceClient(h.client, h.url, opt),
		device: ownsandv1connect.NewDeviceServiceClient(h.client, h.url, opt),
	}
}

// allowed lists who may call an RPC according to the contract matrix.
type allowed struct{ owner, admin, member, key bool }

type rpcCase struct {
	name  string
	allow allowed
	call  func(ctx context.Context, c clients, f fixture) error
}

func ignore[T any](_ T, err error) error { return err }

var coreCases = []rpcCase{
	{"GetOrganization", allowed{true, true, true, false}, func(ctx context.Context, c clients, f fixture) error {
		return ignore(c.org.GetOrganization(ctx, connect.NewRequest(&ownsandv1.GetOrganizationRequest{OrganizationId: f.org})))
	}},
	{"DeleteOrganization", allowed{true, false, false, false}, func(ctx context.Context, c clients, f fixture) error {
		return ignore(c.org.DeleteOrganization(ctx, connect.NewRequest(&ownsandv1.DeleteOrganizationRequest{OrganizationId: f.org})))
	}},
	{"TransferOwnership", allowed{true, false, false, false}, func(ctx context.Context, c clients, f fixture) error {
		return ignore(c.org.TransferOwnership(ctx, connect.NewRequest(&ownsandv1.TransferOwnershipRequest{OrganizationId: f.org, NewOwnerUserId: f.memberSub})))
	}},
	{"CreateInvitation", allowed{true, true, false, false}, func(ctx context.Context, c clients, f fixture) error {
		return ignore(c.member.CreateInvitation(ctx, connect.NewRequest(&ownsandv1.CreateInvitationRequest{OrganizationId: f.org, Role: ownsandv1.Role_ROLE_MEMBER})))
	}},
	{"ListInvitations", allowed{true, true, false, false}, func(ctx context.Context, c clients, f fixture) error {
		return ignore(c.member.ListInvitations(ctx, connect.NewRequest(&ownsandv1.ListInvitationsRequest{OrganizationId: f.org})))
	}},
	{"RevokeInvitation", allowed{true, true, false, false}, func(ctx context.Context, c clients, f fixture) error {
		return ignore(c.member.RevokeInvitation(ctx, connect.NewRequest(&ownsandv1.RevokeInvitationRequest{OrganizationId: f.org, InvitationId: f.invitationID})))
	}},
	{"ListMembers", allowed{true, true, true, false}, func(ctx context.Context, c clients, f fixture) error {
		return ignore(c.member.ListMembers(ctx, connect.NewRequest(&ownsandv1.ListMembersRequest{OrganizationId: f.org})))
	}},
	{"UpdateMemberRole", allowed{true, true, false, false}, func(ctx context.Context, c clients, f fixture) error {
		return ignore(c.member.UpdateMemberRole(ctx, connect.NewRequest(&ownsandv1.UpdateMemberRoleRequest{OrganizationId: f.org, UserId: f.memberSub, Role: ownsandv1.Role_ROLE_ADMIN})))
	}},
	{"RemoveMember(other)", allowed{true, true, false, false}, func(ctx context.Context, c clients, f fixture) error {
		return ignore(c.member.RemoveMember(ctx, connect.NewRequest(&ownsandv1.RemoveMemberRequest{OrganizationId: f.org, UserId: f.adminSub})))
	}},
	{"CreateApiKey", allowed{true, true, false, false}, func(ctx context.Context, c clients, f fixture) error {
		return ignore(c.keys.CreateApiKey(ctx, connect.NewRequest(&ownsandv1.CreateApiKeyRequest{OrganizationId: f.org, Name: "n"})))
	}},
	{"ListApiKeys", allowed{true, true, false, false}, func(ctx context.Context, c clients, f fixture) error {
		return ignore(c.keys.ListApiKeys(ctx, connect.NewRequest(&ownsandv1.ListApiKeysRequest{OrganizationId: f.org})))
	}},
	{"RevokeApiKey", allowed{true, true, false, false}, func(ctx context.Context, c clients, f fixture) error {
		return ignore(c.keys.RevokeApiKey(ctx, connect.NewRequest(&ownsandv1.RevokeApiKeyRequest{OrganizationId: f.org, ApiKeyId: f.apiKeyID})))
	}},
	{"CreateEnrollmentToken", allowed{true, true, false, false}, func(ctx context.Context, c clients, f fixture) error {
		return ignore(c.device.CreateEnrollmentToken(ctx, connect.NewRequest(&ownsandv1.CreateEnrollmentTokenRequest{OrganizationId: f.org})))
	}},
	{"ListEnrollmentTokens", allowed{true, true, false, false}, func(ctx context.Context, c clients, f fixture) error {
		return ignore(c.device.ListEnrollmentTokens(ctx, connect.NewRequest(&ownsandv1.ListEnrollmentTokensRequest{OrganizationId: f.org})))
	}},
	{"RevokeEnrollmentToken", allowed{true, true, false, false}, func(ctx context.Context, c clients, f fixture) error {
		return ignore(c.device.RevokeEnrollmentToken(ctx, connect.NewRequest(&ownsandv1.RevokeEnrollmentTokenRequest{OrganizationId: f.org, EnrollmentTokenId: f.tokenID})))
	}},
	{"ListDevices", allowed{true, true, true, true}, func(ctx context.Context, c clients, f fixture) error {
		return ignore(c.device.ListDevices(ctx, connect.NewRequest(&ownsandv1.ListDevicesRequest{OrganizationId: f.org})))
	}},
	{"GetDevice", allowed{true, true, true, true}, func(ctx context.Context, c clients, f fixture) error {
		return ignore(c.device.GetDevice(ctx, connect.NewRequest(&ownsandv1.GetDeviceRequest{OrganizationId: f.org, DeviceId: f.deviceID})))
	}},
	{"UpdateDeviceLimits", allowed{true, true, false, false}, func(ctx context.Context, c clients, f fixture) error {
		return ignore(c.device.UpdateDeviceLimits(ctx, connect.NewRequest(&ownsandv1.UpdateDeviceLimitsRequest{OrganizationId: f.org, DeviceId: f.deviceID, Limits: &ownsandv1.DeviceLimits{VcpuCount: 4, MemoryMib: 4096, DiskGib: 20}})))
	}},
	{"RemoveDevice", allowed{true, true, false, false}, func(ctx context.Context, c clients, f fixture) error {
		return ignore(c.device.RemoveDevice(ctx, connect.NewRequest(&ownsandv1.RemoveDeviceRequest{OrganizationId: f.org, DeviceId: f.deviceID})))
	}},
	{"GetAgentVersionPolicy", allowed{true, true, true, true}, func(ctx context.Context, c clients, f fixture) error {
		return ignore(c.device.GetAgentVersionPolicy(ctx, connect.NewRequest(&ownsandv1.GetAgentVersionPolicyRequest{OrganizationId: f.org})))
	}},
	{"UpdateAgentVersionPolicy", allowed{true, true, false, false}, func(ctx context.Context, c clients, f fixture) error {
		return ignore(c.device.UpdateAgentVersionPolicy(ctx, connect.NewRequest(&ownsandv1.UpdateAgentVersionPolicyRequest{OrganizationId: f.org, Mode: ownsandv1.AgentVersionPolicyMode_AGENT_VERSION_POLICY_MODE_HOLD})))
	}},
}

type principalCase struct {
	name       string
	credential func(f fixture) string
	expect     func(a allowed) connect.Code // 0 = success
}

func allowIf(ok bool) connect.Code {
	if ok {
		return 0
	}
	return connect.CodePermissionDenied
}

var principalCases = []principalCase{
	{"owner", func(f fixture) string { return userToken(f.ownerSub) }, func(a allowed) connect.Code { return allowIf(a.owner) }},
	{"admin", func(f fixture) string { return userToken(f.adminSub) }, func(a allowed) connect.Code { return allowIf(a.admin) }},
	{"member", func(f fixture) string { return userToken(f.memberSub) }, func(a allowed) connect.Code { return allowIf(a.member) }},
	{"non_member", func(f fixture) string { return userToken("stranger") }, func(allowed) connect.Code { return connect.CodeNotFound }},
	{"api_key", func(f fixture) string { return f.apiKey }, func(a allowed) connect.Code { return allowIf(a.key) }},
	{"other_org_api_key", func(f fixture) string { return f.otherAPIKey }, func(allowed) connect.Code { return connect.CodeNotFound }},
	{"device", func(f fixture) string { return f.deviceCredential }, func(allowed) connect.Code { return connect.CodePermissionDenied }},
	{"no_credential", func(f fixture) string { return "" }, func(allowed) connect.Code { return connect.CodeUnauthenticated }},
	{"revoked_api_key", nil, func(allowed) connect.Code { return connect.CodeUnauthenticated }},
}

// The authorization matrix for every core RPC with each principal kind,
// through the real handlers, interceptor and storage.
func TestCoreAuthorizationMatrix(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	for _, rc := range coreCases {
		for _, pc := range principalCases {
			f := h.newFixture()
			cred := ""
			if pc.credential != nil {
				cred = pc.credential(f)
			} else {
				_, err := h.clients(userToken(f.ownerSub)).keys.RevokeApiKey(ctx, connect.NewRequest(&ownsandv1.RevokeApiKeyRequest{OrganizationId: f.org, ApiKeyId: f.apiKeyID}))
				must(t, err)
				cred = f.apiKey
			}
			err := rc.call(ctx, h.clients(cred), f)
			want := pc.expect(rc.allow)
			got := connect.Code(0)
			if err != nil {
				got = connect.CodeOf(err)
			}
			if got != want {
				t.Errorf("%s as %s: got %v (%v), want %v", rc.name, pc.name, got, err, want)
			}
		}
	}
}

func TestUserOnlyRPCs(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	f := h.newFixture()
	for name, cred := range map[string]string{"api_key": f.apiKey, "device": f.deviceCredential} {
		c := h.clients(cred)
		if _, err := c.org.CreateOrganization(ctx, connect.NewRequest(&ownsandv1.CreateOrganizationRequest{Name: "x"})); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("CreateOrganization as %s: %v", name, err)
		}
		if _, err := c.org.ListOrganizations(ctx, connect.NewRequest(&ownsandv1.ListOrganizationsRequest{})); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("ListOrganizations as %s: %v", name, err)
		}
		if _, err := c.member.AcceptInvitation(ctx, connect.NewRequest(&ownsandv1.AcceptInvitationRequest{InvitationSecret: secret.New(secret.KindInvitation)})); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("AcceptInvitation as %s: %v", name, err)
		}
	}
	// A user without memberships sees only organizations they belong to.
	c := h.clients(userToken("fresh-user"))
	created, err := c.org.CreateOrganization(ctx, connect.NewRequest(&ownsandv1.CreateOrganizationRequest{Name: "mine"}))
	must(t, err)
	if created.Msg.Organization.CallerRole != ownsandv1.Role_ROLE_OWNER {
		t.Fatal("creator is not the owner")
	}
	list, err := c.org.ListOrganizations(ctx, connect.NewRequest(&ownsandv1.ListOrganizationsRequest{}))
	must(t, err)
	if len(list.Msg.Organizations) != 1 || list.Msg.Organizations[0].OrganizationId != created.Msg.Organization.OrganizationId {
		t.Fatalf("list: %v", list.Msg.Organizations)
	}
}

// Member rules beyond the matrix: leaving, owner protection, admin limits and
// AcceptInvitation codes.
func TestMemberRules(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	f := h.newFixture()
	owner, admin, member := h.clients(userToken(f.ownerSub)), h.clients(userToken(f.adminSub)), h.clients(userToken(f.memberSub))
	code := func(err error) connect.Code { return connect.CodeOf(err) }

	if _, err := admin.member.UpdateMemberRole(ctx, connect.NewRequest(&ownsandv1.UpdateMemberRoleRequest{OrganizationId: f.org, UserId: f.ownerSub, Role: ownsandv1.Role_ROLE_MEMBER})); code(err) != connect.CodePermissionDenied {
		t.Errorf("admin demotes owner: %v", err)
	}
	if _, err := admin.member.RemoveMember(ctx, connect.NewRequest(&ownsandv1.RemoveMemberRequest{OrganizationId: f.org, UserId: f.ownerSub})); code(err) != connect.CodePermissionDenied {
		t.Errorf("admin removes owner: %v", err)
	}
	if _, err := admin.member.UpdateMemberRole(ctx, connect.NewRequest(&ownsandv1.UpdateMemberRoleRequest{OrganizationId: f.org, UserId: f.memberSub, Role: ownsandv1.Role_ROLE_OWNER})); code(err) != connect.CodeInvalidArgument {
		t.Errorf("admin grants owner: %v", err)
	}
	if _, err := owner.member.RemoveMember(ctx, connect.NewRequest(&ownsandv1.RemoveMemberRequest{OrganizationId: f.org, UserId: f.ownerSub})); code(err) != connect.CodePermissionDenied {
		t.Errorf("owner leaves: %v", err)
	}
	if _, err := member.member.RemoveMember(ctx, connect.NewRequest(&ownsandv1.RemoveMemberRequest{OrganizationId: f.org, UserId: f.memberSub})); err != nil {
		t.Errorf("member leaves: %v", err)
	}
	if _, err := member.org.GetOrganization(ctx, connect.NewRequest(&ownsandv1.GetOrganizationRequest{OrganizationId: f.org})); code(err) != connect.CodeNotFound {
		t.Errorf("former member reads organization: %v", err)
	}
	if _, err := admin.member.RemoveMember(ctx, connect.NewRequest(&ownsandv1.RemoveMemberRequest{OrganizationId: f.org, UserId: f.adminSub})); err != nil {
		t.Errorf("admin leaves: %v", err)
	}

	inv, err := owner.member.CreateInvitation(ctx, connect.NewRequest(&ownsandv1.CreateInvitationRequest{OrganizationId: f.org, Role: ownsandv1.Role_ROLE_ADMIN}))
	must(t, err)
	value := inv.Msg.InvitationSecret
	if secret.Classify(value) != secret.KindInvitation {
		t.Fatal("invitation secret format")
	}
	if _, err := owner.member.AcceptInvitation(ctx, connect.NewRequest(&ownsandv1.AcceptInvitationRequest{InvitationSecret: value})); code(err) != connect.CodeAlreadyExists {
		t.Errorf("existing member accepts: %v", err)
	}
	joiner := h.clients(userToken("joiner"))
	got, err := joiner.member.AcceptInvitation(ctx, connect.NewRequest(&ownsandv1.AcceptInvitationRequest{InvitationSecret: value}))
	if err != nil || got.Msg.Member.Role != ownsandv1.Role_ROLE_ADMIN {
		t.Fatalf("accept: %v", err)
	}
	other := h.clients(userToken("late"))
	_, usedErr := other.member.AcceptInvitation(ctx, connect.NewRequest(&ownsandv1.AcceptInvitationRequest{InvitationSecret: value}))
	_, unknownErr := other.member.AcceptInvitation(ctx, connect.NewRequest(&ownsandv1.AcceptInvitationRequest{InvitationSecret: secret.New(secret.KindInvitation)}))
	if code(usedErr) != connect.CodeNotFound || code(unknownErr) != connect.CodeNotFound || usedErr.Error() != unknownErr.Error() {
		t.Errorf("used vs unknown invitation: %v / %v", usedErr, unknownErr)
	}

	// Display names fall back to empty when delino-accounts is unavailable.
	members, err := owner.member.ListMembers(ctx, connect.NewRequest(&ownsandv1.ListMembersRequest{OrganizationId: f.org}))
	must(t, err)
	for _, m := range members.Msg.Members {
		if m.DisplayName != "" {
			t.Fatal("display name without a source")
		}
	}
}

func TestEnrollmentTokenValidation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	f := h.newFixture()
	c := h.clients(userToken(f.ownerSub))
	for _, d := range []time.Duration{0, -time.Second, 24*time.Hour + time.Second} {
		_, err := c.device.CreateEnrollmentToken(ctx, connect.NewRequest(&ownsandv1.CreateEnrollmentTokenRequest{OrganizationId: f.org, ExpiresIn: durationpb.New(d)}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("expires_in %v: %v", d, err)
		}
	}
	resp, err := c.device.CreateEnrollmentToken(ctx, connect.NewRequest(&ownsandv1.CreateEnrollmentTokenRequest{OrganizationId: f.org, ExpiresIn: durationpb.New(24 * time.Hour)}))
	must(t, err)
	tok := resp.Msg.EnrollmentToken
	if tok.ExpireTime.AsTime().Sub(tok.CreateTime.AsTime()) != 24*time.Hour || tok.State != ownsandv1.EnrollmentTokenState_ENROLLMENT_TOKEN_STATE_PENDING {
		t.Fatalf("token: %v", tok)
	}
	if secret.Classify(resp.Msg.EnrollmentTokenSecret) != secret.KindEnrollmentToken {
		t.Fatal("token format")
	}
	_, err = c.device.UpdateAgentVersionPolicy(ctx, connect.NewRequest(&ownsandv1.UpdateAgentVersionPolicyRequest{OrganizationId: f.org}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("unspecified mode: %v", err)
	}
}

// Logs never contain secrets that the server issued or received.
func TestLogRedaction(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	f := h.newFixture()
	c := h.clients(userToken(f.ownerSub))
	key, err := c.keys.CreateApiKey(ctx, connect.NewRequest(&ownsandv1.CreateApiKeyRequest{OrganizationId: f.org, Name: "k"}))
	must(t, err)
	inv, err := c.member.CreateInvitation(ctx, connect.NewRequest(&ownsandv1.CreateInvitationRequest{OrganizationId: f.org, Role: ownsandv1.Role_ROLE_MEMBER}))
	must(t, err)
	tok, err := c.device.CreateEnrollmentToken(ctx, connect.NewRequest(&ownsandv1.CreateEnrollmentTokenRequest{OrganizationId: f.org}))
	must(t, err)
	_, _ = h.clients(key.Msg.ApiKeySecret).device.ListDevices(ctx, connect.NewRequest(&ownsandv1.ListDevicesRequest{OrganizationId: f.org}))
	_, _ = h.clients(userToken("x")).member.AcceptInvitation(ctx, connect.NewRequest(&ownsandv1.AcceptInvitationRequest{InvitationSecret: inv.Msg.InvitationSecret}))
	_, _ = h.clients(secret.New(secret.KindAPIKey)).device.ListDevices(ctx, connect.NewRequest(&ownsandv1.ListDevicesRequest{OrganizationId: f.org}))
	logs := h.logs.String()
	if !strings.Contains(logs, `"procedure"`) {
		t.Fatal("no RPC log records")
	}
	for _, s := range []string{key.Msg.ApiKeySecret, inv.Msg.InvitationSecret, tok.Msg.EnrollmentTokenSecret, f.apiKey, f.deviceCredential, userToken(f.ownerSub)} {
		if strings.Contains(logs, s) {
			t.Fatalf("a secret reached the logs")
		}
	}
}

func TestHealthzAndCORS(t *testing.T) {
	h := newHarness(t)
	resp, err := h.client.Get(h.url + "/healthz")
	must(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || strings.TrimSpace(string(body)) != `{"status":"ok"}` || resp.ProtoMajor != 2 {
		t.Fatalf("healthz: %d %q proto %d", resp.StatusCode, body, resp.ProtoMajor)
	}
	preflight := func(origin string) *http.Response {
		req, _ := http.NewRequest(http.MethodOptions, h.url+ownsandv1connect.DeviceServiceListDevicesProcedure, nil)
		req.Header.Set("Origin", origin)
		req.Header.Set("Access-Control-Request-Method", "POST")
		req.Header.Set("Access-Control-Request-Headers", "authorization,content-type")
		resp, err := h.client.Do(req)
		must(t, err)
		resp.Body.Close()
		return resp
	}
	if r := preflight("http://127.0.0.1:3300"); r.Header.Get("Access-Control-Allow-Origin") != "http://127.0.0.1:3300" || !strings.Contains(r.Header.Get("Access-Control-Allow-Headers"), "Authorization") {
		t.Fatalf("allowed origin: %v", r.Header)
	}
	if r := preflight("https://evil.example"); r.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("disallowed origin got CORS headers")
	}
}
