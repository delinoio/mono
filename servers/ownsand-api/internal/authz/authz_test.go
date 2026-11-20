// SPDX-License-Identifier: FSL-1.1-ALv2

package authz

import (
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
	"github.com/delinoio/mono/servers/ownsand-api/internal/auth"
)

// Every RPC of every OwnSand service has an access rule.
func TestEveryProcedureHasARule(t *testing.T) {
	_ = ownsandv1.Role_ROLE_OWNER // load the package descriptors
	seen := 0
	protoregistry.GlobalFiles.RangeFilesByPackage("delino.ownsand.v1", func(fd protoreflect.FileDescriptor) bool {
		for i := 0; i < fd.Services().Len(); i++ {
			svc := fd.Services().Get(i)
			for j := 0; j < svc.Methods().Len(); j++ {
				proc := "/" + string(svc.FullName()) + "/" + string(svc.Methods().Get(j).Name())
				seen++
				if _, ok := Procedures[proc]; !ok {
					t.Errorf("procedure %s has no access rule", proc)
				}
			}
		}
		return true
	})
	if seen != len(Procedures) {
		t.Errorf("schema has %d procedures, the rule table has %d", seen, len(Procedures))
	}
}

type principalCase struct {
	name      string
	kind      ownsandv1.PrincipalKind
	role      ownsandv1.Role // for users; UNSPECIFIED = no membership
	ownOrgKey bool           // for API keys
}

var principals = []principalCase{
	{"owner", ownsandv1.PrincipalKind_PRINCIPAL_KIND_USER, ownsandv1.Role_ROLE_OWNER, false},
	{"admin", ownsandv1.PrincipalKind_PRINCIPAL_KIND_USER, ownsandv1.Role_ROLE_ADMIN, false},
	{"member", ownsandv1.PrincipalKind_PRINCIPAL_KIND_USER, ownsandv1.Role_ROLE_MEMBER, false},
	{"non_member", ownsandv1.PrincipalKind_PRINCIPAL_KIND_USER, ownsandv1.Role_ROLE_UNSPECIFIED, false},
	{"api_key", ownsandv1.PrincipalKind_PRINCIPAL_KIND_API_KEY, 0, true},
	{"other_org_api_key", ownsandv1.PrincipalKind_PRINCIPAL_KIND_API_KEY, 0, false},
	{"device", ownsandv1.PrincipalKind_PRINCIPAL_KIND_DEVICE, 0, false},
}

// expected returns the contract outcome of each principal on each action:
// connect.Code(0) for allowed.
func expected(action Action, pc principalCase) connect.Code {
	type row struct{ owner, admin, member, key bool }
	matrix := map[Action]row{
		ActionDeleteOrganization: {true, false, false, false},
		ActionTransferOwnership:  {true, false, false, false},
		ActionManageMembers:      {true, true, false, false},
		ActionLeaveOrganization:  {false, true, true, false},
		ActionManageDevices:      {true, true, false, false},
		ActionManageAPIKeys:      {true, true, false, false},
		ActionReadOrganization:   {true, true, true, false},
		ActionReadDevices:        {true, true, true, true},
		ActionUseSandboxes:       {true, true, true, true},
	}
	r := matrix[action]
	allow := func(ok bool) connect.Code {
		if ok {
			return 0
		}
		return connect.CodePermissionDenied
	}
	switch pc.name {
	case "owner":
		return allow(r.owner)
	case "admin":
		return allow(r.admin)
	case "member":
		return allow(r.member)
	case "non_member", "other_org_api_key":
		return connect.CodeNotFound
	case "api_key":
		return allow(r.key)
	default:
		return connect.CodePermissionDenied
	}
}

// The authorization matrix for every RPC with each principal kind, including
// cross-organization access (not_found) and device credentials.
func TestMatrixPerProcedure(t *testing.T) {
	const org = "0190a0c0-0000-7000-8000-000000000001"
	const otherOrg = "0190a0c0-0000-7000-8000-000000000002"
	for proc, rule := range Procedures {
		for _, pc := range principals {
			p := auth.Principal{Kind: pc.kind, ID: "id-" + pc.name}
			if pc.kind == ownsandv1.PrincipalKind_PRINCIPAL_KIND_API_KEY {
				p.OrganizationID = otherOrg
				if pc.ownOrgKey {
					p.OrganizationID = org
				}
			}
			_, accessErr := CheckAccess(proc, pc.kind)
			switch rule.Access {
			case AccessAgentUnauthenticated:
				if accessErr != nil {
					t.Errorf("%s: unauthenticated agent RPC rejected %s", proc, pc.name)
				}
				continue
			case AccessDevice:
				wantOK := pc.kind == ownsandv1.PrincipalKind_PRINCIPAL_KIND_DEVICE
				if (accessErr == nil) != wantOK || (accessErr != nil && connect.CodeOf(accessErr) != connect.CodePermissionDenied) {
					t.Errorf("%s: device-only RPC with %s: err=%v", proc, pc.name, accessErr)
				}
				continue
			case AccessUserOnly:
				wantOK := pc.kind == ownsandv1.PrincipalKind_PRINCIPAL_KIND_USER
				if (accessErr == nil) != wantOK || (accessErr != nil && connect.CodeOf(accessErr) != connect.CodePermissionDenied) {
					t.Errorf("%s: user-only RPC with %s: err=%v", proc, pc.name, accessErr)
				}
				continue
			}
			if pc.kind == ownsandv1.PrincipalKind_PRINCIPAL_KIND_DEVICE {
				if accessErr == nil || connect.CodeOf(accessErr) != connect.CodePermissionDenied {
					t.Errorf("%s: device credential accepted on a public RPC", proc)
				}
				continue
			}
			if accessErr != nil {
				t.Errorf("%s: %s rejected before the matrix: %v", proc, pc.name, accessErr)
				continue
			}
			var m *Membership
			if pc.role != ownsandv1.Role_ROLE_UNSPECIFIED {
				m = &Membership{Role: pc.role}
			}
			actions := []Action{rule.Action}
			if proc == ownsandv1connect.MemberServiceRemoveMemberProcedure {
				actions = append(actions, ActionLeaveOrganization)
			}
			for _, action := range actions {
				err := Decide(p, org, m, action)
				want := expected(action, pc)
				got := connect.Code(0)
				if err != nil {
					got = connect.CodeOf(err)
				}
				if got != want {
					t.Errorf("%s (action %d) as %s: got %v, want %v", proc, action, pc.name, got, want)
				}
			}
		}
	}
}

// API keys are not accepted on the RPCs the contract lists, but a key of
// another organization still gets not_found there.
func TestAPIKeyExclusions(t *testing.T) {
	excluded := []string{
		ownsandv1connect.DeviceServiceCreateEnrollmentTokenProcedure,
		ownsandv1connect.DeviceServiceListEnrollmentTokensProcedure,
		ownsandv1connect.DeviceServiceRevokeEnrollmentTokenProcedure,
		ownsandv1connect.DeviceServiceUpdateDeviceLimitsProcedure,
		ownsandv1connect.DeviceServiceRemoveDeviceProcedure,
		ownsandv1connect.DeviceServiceUpdateAgentVersionPolicyProcedure,
		ownsandv1connect.OrganizationServiceGetOrganizationProcedure,
		ownsandv1connect.MemberServiceListMembersProcedure,
		ownsandv1connect.ApiKeyServiceCreateApiKeyProcedure,
	}
	for _, proc := range excluded {
		rule := Procedures[proc]
		own := auth.Principal{Kind: ownsandv1.PrincipalKind_PRINCIPAL_KIND_API_KEY, ID: "k", OrganizationID: "a"}
		if connect.CodeOf(Decide(own, "a", nil, rule.Action)) != connect.CodePermissionDenied {
			t.Errorf("%s: own key not denied", proc)
		}
		if connect.CodeOf(Decide(own, "b", nil, rule.Action)) != connect.CodeNotFound {
			t.Errorf("%s: other organization key not not_found", proc)
		}
	}
}
