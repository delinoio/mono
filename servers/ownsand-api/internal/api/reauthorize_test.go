// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/go/ownsand/relaybinding"
	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/api"
	"github.com/delinoio/mono/servers/ownsand-api/internal/auth"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// keyContext returns a context that carries the API key principal of org, as
// the authentication interceptor sets it after it checked the key. A test that
// calls a handler with it controls when the key is revoked relative to the
// handler: the interceptor check is already behind the call.
func keyContext(org *testenv.Org) context.Context {
	return auth.WithPrincipal(context.Background(), auth.Principal{
		Kind: ownsandv1.PrincipalKind_PRINCIPAL_KIND_API_KEY, ID: org.APIKeyID, OrganizationID: org.ID,
	})
}

func revokeKey(t *testing.T, org *testenv.Org) {
	t.Helper()
	if _, err := org.OwnerClient.APIKey.RevokeApiKey(context.Background(), connect.NewRequest(&ownsandv1.RevokeApiKeyRequest{OrganizationId: org.ID, ApiKeyId: org.APIKeyID})); err != nil {
		t.Fatal(err)
	}
}

func offersReceived(agent *testenv.FakeAgent) (n int) {
	for _, e := range agent.Received() {
		if e.GetSignaling().GetOffer() != nil {
			n++
		}
	}
	return n
}

// TestOpenSessionRevokedKeyBeforeRegistration covers an API key that was
// revoked, with its revocation event already handled, after the interceptor
// accepted the key and before OpenSession registered the pending session.
// The event found no session to end, so the check after the registration must
// reject the call: no offer reaches the agent and no session stays behind.
func TestOpenSessionRevokedKeyBeforeRegistration(t *testing.T) {
	env := testenv.New(t)
	org, agent, sandboxID := runningSandbox(t, env)
	plane := env.App.Plane
	handler := api.DataPathHandler{DataPlane: plane}
	req := connect.NewRequest(&ownsandv1.OpenSessionRequest{OrganizationId: org.ID, SandboxId: sandboxID, SdpOffer: "v=0"})
	// A valid key opens a session as before.
	if _, err := handler.OpenSession(keyContext(org), req); err != nil {
		t.Fatalf("open session with a valid key: %v", err)
	}
	testenv.Eventually(t, "valid session registered", func() bool { return plane.SessionCount() == 1 })
	offers := offersReceived(agent)
	revokeKey(t, org)
	// The revocation event ends the open session; wait until it is handled so
	// that it cannot end the pending session of the next call.
	testenv.Eventually(t, "revocation handled", func() bool { return plane.SessionCount() == 0 })
	_, err := handler.OpenSession(keyContext(org), req)
	if testenv.Code(err) != connect.CodeUnauthenticated {
		t.Fatalf("open session with a revoked key: %v", err)
	}
	if n := offersReceived(agent); n != offers {
		t.Fatalf("the agent received %d offers, want %d", n, offers)
	}
	if n := plane.SessionCount(); n != 0 {
		t.Fatalf("%d sessions after the rejected call", n)
	}
}

// TestAuthorizeRelayRevokedKeyBeforeRegistration is the AuthorizeRelay
// counterpart: the revoked key gets no grant, a grant that an earlier call of
// the same authorization registered stays, and a user caller is unchanged.
func TestAuthorizeRelayRevokedKeyBeforeRegistration(t *testing.T) {
	env := testenv.New(t)
	org, agent, sandboxID := runningSandbox(t, env)
	plane := env.App.Plane
	handler := api.DataPathHandler{DataPlane: plane}
	exec := newExec(org.ID, sandboxID)
	newReq := func(op *ownsandv1.ExecRequest, authID string) *connect.Request[ownsandv1.AuthorizeRelayRequest] {
		return connect.NewRequest(&ownsandv1.AuthorizeRelayRequest{
			OrganizationId: org.ID, SandboxId: sandboxID, OperationKind: ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC,
			OperationId: op.GetExecId(), RequestDigest: relaybinding.ExecDigest(op), AuthorizationId: authID,
		})
	}
	authID, _ := uuidv7.NewString()
	first, err := handler.AuthorizeRelay(keyContext(org), newReq(exec, authID))
	if err != nil {
		t.Fatalf("authorize relay with a valid key: %v", err)
	}
	grantID := first.Msg.GetGrantId()
	// Revoke the key in the database without the revocation event, so that
	// the registered grant stays and the retry finds it as an existing grant.
	if _, err := env.DB.Pool.Exec(context.Background(), "UPDATE api_keys SET revoke_time = now() WHERE api_key_id = $1", org.APIKeyID); err != nil {
		t.Fatal(err)
	}
	if _, err := handler.AuthorizeRelay(keyContext(org), newReq(exec, authID)); testenv.Code(err) != connect.CodeUnauthenticated {
		t.Fatalf("retry of an existing grant with a revoked key: %v", err)
	}
	if plane.Grants.Len() != 1 || plane.Grants.Admittable(grantID) == nil {
		t.Fatal("the rejected retry removed the existing grant")
	}
	// A new authorization of the revoked key registers no grant and sends none.
	other := newExec(org.ID, sandboxID)
	otherAuth, _ := uuidv7.NewString()
	if _, err := handler.AuthorizeRelay(keyContext(org), newReq(other, otherAuth)); testenv.Code(err) != connect.CodeUnauthenticated {
		t.Fatalf("new authorization with a revoked key: %v", err)
	}
	if plane.Grants.Len() != 1 {
		t.Fatalf("%d grants after the rejected call, want 1", plane.Grants.Len())
	}
	if ids := grantsReceived(agent); len(ids) != 1 || ids[0] != grantID {
		t.Fatalf("grants sent = %v, want only %s", ids, grantID)
	}
	// A user is not affected.
	userCtx := auth.WithPrincipal(context.Background(), auth.Principal{Kind: ownsandv1.PrincipalKind_PRINCIPAL_KIND_USER, ID: org.Owner})
	userAuth, _ := uuidv7.NewString()
	if _, err := handler.AuthorizeRelay(userCtx, newReq(other, userAuth)); err != nil {
		t.Fatalf("authorize relay as a user: %v", err)
	}
}

// TestReauthorizeAPIKeyOrganizationDeleted checks the post-registration
// authorization of an API key whose organization was deleted: a call cannot
// reach that step after the deletion removed its sandboxes, so the test calls
// the step itself. A live key passes and a user is checked by membership.
func TestReauthorizeAPIKeyOrganizationDeleted(t *testing.T) {
	env := testenv.New(t)
	org := env.NewOrg("alice")
	svc := env.App.Service
	if err := svc.ReauthorizeForTest(keyContext(org), org.ID); err != nil {
		t.Fatalf("live key: %v", err)
	}
	userCtx := auth.WithPrincipal(context.Background(), auth.Principal{Kind: ownsandv1.PrincipalKind_PRINCIPAL_KIND_USER, ID: org.Owner})
	if err := svc.ReauthorizeForTest(userCtx, org.ID); err != nil {
		t.Fatalf("owner: %v", err)
	}
	if _, err := org.OwnerClient.Organization.DeleteOrganization(context.Background(), connect.NewRequest(&ownsandv1.DeleteOrganizationRequest{OrganizationId: org.ID})); err != nil {
		t.Fatal(err)
	}
	if err := svc.ReauthorizeForTest(keyContext(org), org.ID); testenv.Code(err) != connect.CodeUnauthenticated {
		t.Fatalf("key of a deleted organization: %v", err)
	}
	if err := svc.ReauthorizeForTest(userCtx, org.ID); testenv.Code(err) != connect.CodeNotFound {
		t.Fatalf("owner of a deleted organization: %v", err)
	}
}
