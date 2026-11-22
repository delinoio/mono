// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
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
