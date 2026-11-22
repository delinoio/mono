// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// requireInvalidNaming fails unless err is invalid_argument whose message
// names field, and unless the server logged no internal error: before the
// check, PostgreSQL rejected the NUL byte (SQLSTATE 22021 or 22P05) inside
// the transaction and the caller got an internal error.
func requireInvalidNaming(t *testing.T, env *testenv.Env, what string, err error, field string) {
	t.Helper()
	if testenv.Code(err) != connect.CodeInvalidArgument {
		t.Fatalf("%s: code = %v (%v), want invalid_argument", what, testenv.Code(err), err)
	}
	if !strings.Contains(err.Error(), field) {
		t.Fatalf("%s: error %q does not name %q", what, err, field)
	}
	if strings.Contains(env.Logs.String(), "internal_error") {
		t.Fatalf("%s: the server logged an internal error:\n%s", what, env.Logs.String())
	}
}

func countRows(t *testing.T, env *testenv.Env, query string, args ...any) int {
	t.Helper()
	var n int
	if err := env.DB.Pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestEnrollRejectsNULBeforeTheTransaction sends a device name and an agent
// version with a NUL byte, which PostgreSQL text cannot store. Both fail
// with invalid_argument before the transaction, so no device exists and the
// enrollment token is still unused: the same token then enrolls a device
// with valid values.
func TestEnrollRejectsNULBeforeTheTransaction(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	f := newEnrollOrgLockFixture(t, env)
	enroll := func(name, version string) error {
		_, err := env.Clients("").Agent.Enroll(ctx, connect.NewRequest(&ownsandv1.EnrollRequest{
			EnrollmentToken: f.token, PublicKey: f.pub, Name: name, HostOs: ownsandv1.HostOs_HOST_OS_LINUX,
			Arch: ownsandv1.Arch_ARCH_AMD64, AgentVersion: version,
		}))
		return err
	}
	requireInvalidNaming(t, env, "name with NUL", enroll("a\x00b", "1.0.0"), "name")
	requireInvalidNaming(t, env, "agent_version with NUL", enroll("host", "1.0\x00"), "agent_version")
	if n := countRows(t, env, "SELECT count(*) FROM devices WHERE organization_id = $1", f.org.ID); n != 0 {
		t.Fatalf("devices after the rejected enrollments = %d, want 0", n)
	}
	if n := countRows(t, env, "SELECT count(*) FROM enrollment_tokens WHERE organization_id = $1 AND use_time IS NULL", f.org.ID); n != 1 {
		t.Fatalf("unused enrollment tokens = %d, want 1", n)
	}
	// Other control characters stay valid in a device name.
	if err := enroll("host\x01\x7f", "1.0.0"); err != nil {
		t.Fatalf("enroll with the same token and valid values: %v", err)
	}
	if n := countRows(t, env, "SELECT count(*) FROM devices WHERE organization_id = $1", f.org.ID); n != 1 {
		t.Fatalf("devices after the valid enrollment = %d, want 1", n)
	}
}

// TestUpdateAgentVersionPolicyRejectsNUL pins a version with a NUL byte and
// expects invalid_argument and no stored policy; a valid pin still works.
func TestUpdateAgentVersionPolicyRejectsNUL(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	pin := func(version string) error {
		_, err := org.OwnerClient.Device.UpdateAgentVersionPolicy(ctx, connect.NewRequest(&ownsandv1.UpdateAgentVersionPolicyRequest{
			OrganizationId: org.ID, Mode: ownsandv1.AgentVersionPolicyMode_AGENT_VERSION_POLICY_MODE_PIN, PinnedVersion: version,
		}))
		return err
	}
	requireInvalidNaming(t, env, "pinned_version with NUL", pin("1.0\x00.0"), "pinned_version")
	if n := countRows(t, env, "SELECT count(*) FROM agent_version_policies WHERE organization_id = $1", org.ID); n != 0 {
		t.Fatalf("policy rows after the rejected pin = %d, want 0", n)
	}
	if err := pin("1.2.3"); err != nil {
		t.Fatalf("valid pin: %v", err)
	}
}

// TestCreateSandboxRejectsNULInLabels sends a label key and a label value
// with a NUL byte, which PostgreSQL jsonb cannot store, and expects
// invalid_argument and no sandbox. A connected device makes the unfixed
// server reach the insert, where the jsonb column fails.
func TestCreateSandboxRejectsNULInLabels(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	setupAgent(t, env, org, nil)
	for name, labels := range map[string]map[string]string{
		"key":   {"bad\x00key": "v"},
		"value": {"k": "bad\x00value"},
	} {
		req := createReq(org.ID)
		req.Labels = labels
		_, err := org.OwnerClient.Sandbox.CreateSandbox(ctx, connect.NewRequest(req))
		requireInvalidNaming(t, env, "label "+name+" with NUL", err, "labels")
	}
	if n := countRows(t, env, "SELECT count(*) FROM sandboxes WHERE organization_id = $1", org.ID); n != 0 {
		t.Fatalf("sandboxes after the rejected creates = %d, want 0", n)
	}
	// Other control characters stay valid in labels.
	req := createReq(org.ID)
	req.Labels = map[string]string{"k\x01": "v\t\x7f"}
	if _, err := org.OwnerClient.Sandbox.CreateSandbox(ctx, connect.NewRequest(req)); err != nil {
		t.Fatalf("create with control characters other than NUL: %v", err)
	}
}

// TestMemberRPCsRejectNULUserID sends a user_id with a NUL byte to
// UpdateMemberRole, RemoveMember and TransferOwnership. PostgreSQL text
// cannot store it, so before the check the lookup failed with an internal
// error. Each call now fails with invalid_argument, and the memberships and
// their roles stay unchanged. A user_id with another control character is
// still an ordinary unknown member.
func TestMemberRPCsRejectNULUserID(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	owner := org.OwnerClient
	members := func() (total, owners, admins, plain int) {
		q := "SELECT count(*) FROM memberships WHERE organization_id = $1"
		return countRows(t, env, q, org.ID),
			countRows(t, env, q+" AND role = $2", org.ID, int16(ownsandv1.Role_ROLE_OWNER)),
			countRows(t, env, q+" AND role = $2", org.ID, int16(ownsandv1.Role_ROLE_ADMIN)),
			countRows(t, env, q+" AND role = $2", org.ID, int16(ownsandv1.Role_ROLE_MEMBER))
	}
	total, owners, admins, plain := members()
	calls := map[string]func(userID string) error{
		"UpdateMemberRole": func(userID string) error {
			_, err := owner.Member.UpdateMemberRole(ctx, connect.NewRequest(&ownsandv1.UpdateMemberRoleRequest{
				OrganizationId: org.ID, UserId: userID, Role: ownsandv1.Role_ROLE_ADMIN,
			}))
			return err
		},
		"RemoveMember": func(userID string) error {
			_, err := owner.Member.RemoveMember(ctx, connect.NewRequest(&ownsandv1.RemoveMemberRequest{
				OrganizationId: org.ID, UserId: userID,
			}))
			return err
		},
		"TransferOwnership": func(userID string) error {
			_, err := owner.Organization.TransferOwnership(ctx, connect.NewRequest(&ownsandv1.TransferOwnershipRequest{
				OrganizationId: org.ID, UserId: userID,
			}))
			return err
		},
	}
	for name, call := range calls {
		for _, userID := range []string{"abc\x00", "\x00", org.Member + "\x00"} {
			requireInvalidNaming(t, env, name+" with user_id "+strconv.Quote(userID), call(userID), "user_id")
		}
		// Other control characters stay valid text: the user is unknown.
		if err := call("a\x01b"); testenv.Code(err) != connect.CodeNotFound {
			t.Fatalf("%s with user_id a\\x01b: code = %v (%v), want not_found", name, testenv.Code(err), err)
		}
	}
	gotTotal, gotOwners, gotAdmins, gotPlain := members()
	if gotTotal != total || gotOwners != owners || gotAdmins != admins || gotPlain != plain {
		t.Fatalf("memberships after the rejected calls = total %d owner %d admin %d member %d, want %d %d %d %d",
			gotTotal, gotOwners, gotAdmins, gotPlain, total, owners, admins, plain)
	}
	// A valid user ID still works.
	if _, err := owner.Member.UpdateMemberRole(ctx, connect.NewRequest(&ownsandv1.UpdateMemberRoleRequest{
		OrganizationId: org.ID, UserId: org.Member, Role: ownsandv1.Role_ROLE_ADMIN,
	})); err != nil {
		t.Fatalf("valid UpdateMemberRole: %v", err)
	}
}
