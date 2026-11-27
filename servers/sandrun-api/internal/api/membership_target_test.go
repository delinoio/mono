// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/servers/sandrun-api/internal/testenv"
)

func TestMembershipTargetUserID(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	const mixedCaseID = "Member-Example"
	calls := []struct {
		name       string
		ownerOnly  bool
		targetRole sandrunv1.Role
		call       func(*testenv.Clients, string, string) error
	}{
		{
			name:       "UpdateMemberRole",
			targetRole: sandrunv1.Role_ROLE_MEMBER,
			call: func(c *testenv.Clients, orgID, userID string) error {
				_, err := c.Member.UpdateMemberRole(ctx, connect.NewRequest(&sandrunv1.UpdateMemberRoleRequest{
					OrganizationId: orgID, UserId: userID, Role: sandrunv1.Role_ROLE_MEMBER,
				}))
				return err
			},
		},
		{
			name: "RemoveMember",
			call: func(c *testenv.Clients, orgID, userID string) error {
				_, err := c.Member.RemoveMember(ctx, connect.NewRequest(&sandrunv1.RemoveMemberRequest{
					OrganizationId: orgID, UserId: userID,
				}))
				return err
			},
		},
		{
			name:       "TransferOwnership",
			ownerOnly:  true,
			targetRole: sandrunv1.Role_ROLE_OWNER,
			call: func(c *testenv.Clients, orgID, userID string) error {
				_, err := c.Organization.TransferOwnership(ctx, connect.NewRequest(&sandrunv1.TransferOwnershipRequest{
					OrganizationId: orgID, UserId: userID,
				}))
				return err
			},
		},
	}
	for _, rpc := range calls {
		t.Run(rpc.name, func(t *testing.T) {
			org := env.NewOrg("owner")
			env.Join(org.ID, org.Owner, mixedCaseID, sandrunv1.Role_ROLE_ADMIN)
			env.NewAPIKey(org.ID, mixedCaseID)
			caller := env.User(org.Admin)
			if rpc.ownerOnly {
				caller = env.User(org.Owner)
			}

			listRoles := func() map[string]sandrunv1.Role {
				t.Helper()
				resp, err := caller.Member.ListMembers(ctx, connect.NewRequest(&sandrunv1.ListMembersRequest{OrganizationId: org.ID}))
				if err != nil {
					t.Fatal(err)
				}
				roles := make(map[string]sandrunv1.Role)
				for _, member := range resp.Msg.GetMembers() {
					roles[member.GetUserId()] = member.GetRole()
				}
				return roles
			}
			listKeys := func() *sandrunv1.ListApiKeysResponse {
				t.Helper()
				resp, err := env.User(mixedCaseID).APIKey.ListApiKeys(ctx, connect.NewRequest(&sandrunv1.ListApiKeysRequest{OrganizationId: org.ID}))
				if err != nil {
					t.Fatal(err)
				}
				return resp.Msg
			}
			wantInvalid := func(userID, message string) {
				t.Helper()
				err := rpc.call(caller, org.ID, userID)
				wantCode(t, "target user ID", err, connect.CodeInvalidArgument)
				var connectErr *connect.Error
				if !errors.As(err, &connectErr) || connectErr.Message() != message {
					t.Fatalf("target user ID error = %v, want message %q", err, message)
				}
			}

			roles, audit, keys := listRoles(), auditActions(t, env, org.ID), listKeys()
			for _, invalid := range []string{"\x00", "\x00abc", "ab\x00cd", "abc\x00"} {
				wantInvalid(invalid, "invalid user_id: must not contain NUL")
			}
			wantInvalid("", "invalid user_id: is required")
			for _, unknown := range []string{"no-such-user", strings.ToLower(mixedCaseID)} {
				wantCode(t, "unknown target", rpc.call(caller, org.ID, unknown), connect.CodeNotFound)
			}
			wantCode(t, "member with malformed target", rpc.call(env.User(org.Member), org.ID, "\x00"), connect.CodePermissionDenied)
			if rpc.ownerOnly {
				setDeleting := func(deleting bool) {
					t.Helper()
					_, err := env.DB.Pool.Exec(ctx, `
						UPDATE organizations SET deletion_attempt_id = CASE WHEN $2 THEN gen_random_uuid() END
						WHERE organization_id = $1`, org.ID, deleting)
					if err != nil {
						t.Fatal(err)
					}
				}
				setDeleting(true)
				wantCode(t, "transfer during deletion", rpc.call(caller, org.ID, "\x00"), connect.CodeFailedPrecondition)
				setDeleting(false)
			}

			if got := listRoles(); !maps.Equal(got, roles) {
				t.Fatalf("roles after rejection = %v, want %v", got, roles)
			}
			if got := auditActions(t, env, org.ID); !slices.Equal(got, audit) {
				t.Fatalf("audit after rejection = %v, want %v", got, audit)
			}
			if got := listKeys(); !proto.Equal(got, keys) {
				t.Fatal("personal API keys changed after rejection")
			}
			if strings.Contains(env.Logs.String(), `"msg":"internal_error"`) {
				t.Fatal("rejected membership request logged internal_error")
			}

			if err := rpc.call(caller, org.ID, mixedCaseID); err != nil {
				t.Fatalf("valid mixed-case target: %v", err)
			}
			switch rpc.targetRole {
			case sandrunv1.Role_ROLE_UNSPECIFIED:
				delete(roles, mixedCaseID)
			case sandrunv1.Role_ROLE_OWNER:
				roles[org.Owner] = sandrunv1.Role_ROLE_ADMIN
				roles[mixedCaseID] = rpc.targetRole
			default:
				roles[mixedCaseID] = rpc.targetRole
			}
			if got := listRoles(); !maps.Equal(got, roles) {
				t.Fatalf("roles after successful mutation = %v, want %v", got, roles)
			}
		})
	}
}
