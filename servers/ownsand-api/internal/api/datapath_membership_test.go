// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"maps"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	"github.com/delinoio/mono/go/ownsand/relaybinding"
	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/app"
	"github.com/delinoio/mono/servers/ownsand-api/internal/events"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

func replaceMembership(t *testing.T, peer *testenv.Env, org *testenv.Org) {
	t.Helper()
	ctx := t.Context()
	owner := peer.User(org.Owner)
	if _, err := owner.Member.RemoveMember(ctx, connect.NewRequest(&ownsandv1.RemoveMemberRequest{OrganizationId: org.ID, UserId: org.Member})); err != nil {
		t.Fatal(err)
	}
	invite, err := owner.Member.CreateInvitation(ctx, connect.NewRequest(&ownsandv1.CreateInvitationRequest{
		OrganizationId: org.ID, Role: ownsandv1.Role_ROLE_MEMBER,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := peer.User(org.Member).Member.AcceptInvitation(ctx, connect.NewRequest(&ownsandv1.AcceptInvitationRequest{
		InvitationSecret: invite.Msg.GetInvitationSecret(),
	})); err != nil {
		t.Fatal(err)
	}
}

func membershipGrantRequest(exec *ownsandv1.ExecRequest) *ownsandv1.AuthorizeRelayRequest {
	authID, _ := uuidv7.NewString()
	return &ownsandv1.AuthorizeRelayRequest{
		OrganizationId: exec.GetOrganizationId(), SandboxId: exec.GetSandboxId(), AuthorizationId: authID,
		OperationKind: ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC, OperationId: exec.GetExecId(),
		RequestDigest: relaybinding.ExecDigest(exec),
	}
}

// The peer commits the removal without waking the lease holder. Deliver the
// event only after the replacement membership has authorized new work.
func TestDelayedMembershipRevocation(t *testing.T) {
	env := testenv.New(t, func(o *app.Options) {
		o.Timing.EventPoll = time.Hour
		o.RelayTiming.Signaling = 20 * time.Second
		o.RelayTiming.GrantAck = 20 * time.Second
	})
	org, agent, sandboxID := runningSandbox(t, env)
	peer := env.StartPeer()
	agent.ConnectRelay(serveExec)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	member := env.User(org.Member)
	openRequest := func(sdp string) *connect.Request[ownsandv1.OpenSessionRequest] {
		return connect.NewRequest(&ownsandv1.OpenSessionRequest{OrganizationId: org.ID, SandboxId: sandboxID, SdpOffer: sdp})
	}
	open := func(c *testenv.Clients) string {
		t.Helper()
		resp, err := c.DataPath.OpenSession(ctx, openRequest("v=0"))
		if err != nil {
			t.Fatal(err)
		}
		return resp.Msg.GetSessionId()
	}
	issueGrant := func(c *testenv.Clients, req *ownsandv1.AuthorizeRelayRequest) string {
		t.Helper()
		resp, err := c.DataPath.AuthorizeRelay(ctx, connect.NewRequest(req))
		if err != nil {
			t.Fatal(err)
		}
		return resp.Msg.GetGrantId()
	}
	execWith := func(grantID string, req *ownsandv1.ExecRequest) error {
		c := env.ClientsWith(testenv.UserToken(org.Member), grantHeader(grantID))
		stream, err := c.Relay.Exec(ctx, connect.NewRequest(req))
		if err != nil {
			return err
		}
		_, err = collectExec(t, stream)
		return err
	}

	oldSession := open(member)
	ownerSession := open(org.OwnerClient)
	oldExec := newExec(org.ID, sandboxID)
	oldRequest := membershipGrantRequest(oldExec)
	oldGrant := issueGrant(member, oldRequest)
	ownerGrant := issueGrant(org.OwnerClient, membershipGrantRequest(newExec(org.ID, sandboxID)))
	if err := execWith(oldGrant, oldExec); err != nil {
		t.Fatalf("original membership cannot use its grant: %v", err)
	}

	pendingRequest := membershipGrantRequest(newExec(org.ID, sandboxID))
	agent.SetOnOffer(func(o *ownsandv1.SignalingOffer) *ownsandv1.SignalingAnswer {
		if o.GetSdp() == "pending" {
			return nil
		}
		return &ownsandv1.SignalingAnswer{SessionId: o.GetSessionId(), Sdp: "v=0 answer"}
	})
	agent.SetOnGrant(func(g *ownsandv1.RelayGrant) *ownsandv1.RelayGrantAck {
		if g.GetOperationId() == pendingRequest.GetOperationId() {
			return nil
		}
		return &ownsandv1.RelayGrantAck{GrantId: g.GetGrantId(), Result: ownsandv1.RelayGrantResult_RELAY_GRANT_RESULT_ACCEPTED, ReplacedGrantId: g.GetReplacesGrantId()}
	})
	sessionResult := make(chan error, 1)
	grantResult := make(chan error, 1)
	go func() {
		_, err := member.DataPath.OpenSession(ctx, openRequest("pending"))
		sessionResult <- err
	}()
	go func() {
		_, err := member.DataPath.AuthorizeRelay(ctx, connect.NewRequest(pendingRequest))
		grantResult <- err
	}()
	pendingSession := agent.WaitFor("pending offer", func(e *ownsandv1.AgentEnvelope) bool {
		return e.GetSignaling().GetOffer().GetSdp() == "pending"
	}).GetSignaling().GetOffer().GetSessionId()
	pendingGrant := agent.WaitFor("pending grant", func(e *ownsandv1.AgentEnvelope) bool {
		return e.GetRelayGrant().GetOperationId() == pendingRequest.GetOperationId()
	}).GetRelayGrant().GetGrantId()

	replaceMembership(t, peer, org)
	eventID, err := events.LatestID(ctx, env.DB.Pool)
	if err != nil {
		t.Fatal(err)
	}
	if cursor, ok := env.App.Control.ProcessedCursor(); !ok || cursor >= eventID {
		t.Fatalf("membership event %d was not paused: cursor = %d, initialized = %t", eventID, cursor, ok)
	}
	if err := execWith(oldGrant, oldExec); testenv.Code(err) != connect.CodeUnavailable ||
		errDetail(err).GetSessionEndReason() != ownsandv1.SessionEndReason_SESSION_END_REASON_GRANT_INVALID {
		t.Fatalf("replacement membership used the original grant: %v", err)
	}
	if _, err := member.DataPath.AuthorizeRelay(ctx, connect.NewRequest(oldRequest)); testenv.Code(err) != connect.CodeInvalidArgument {
		t.Fatalf("retry with the original authorization ID: %v", err)
	}

	newSession := open(member)
	newRequest := membershipGrantRequest(oldExec)
	newRequest.ReplacesGrantId = &oldGrant
	newGrant := issueGrant(member, newRequest)
	if got := issueGrant(member, newRequest); got != newGrant {
		t.Fatalf("same-membership retry = %s, want %s", got, newGrant)
	}
	newEnvelope := agent.WaitFor("replacement membership grant", func(e *ownsandv1.AgentEnvelope) bool {
		return e.GetRelayGrant().GetGrantId() == newGrant
	})
	if predecessor := newEnvelope.GetRelayGrant().GetReplacesGrantId(); predecessor != "" {
		t.Fatalf("grant names predecessor %s from the ended membership", predecessor)
	}

	env.App.Control.Kick()
	testenv.Eventually(t, "membership event processed", func() bool {
		cursor, ok := env.App.Control.ProcessedCursor()
		return ok && cursor >= eventID
	})
	want := map[string]bool{oldSession: true, pendingSession: true, oldGrant: true, pendingGrant: true}
	for target := range want {
		agent.WaitFor("revocation of "+target, func(e *ownsandv1.AgentEnvelope) bool {
			r := e.GetSessionRevocation()
			return r.GetSessionId() == target || r.GetRelayGrantId() == target
		})
	}
	got := make(map[string]bool)
	for _, e := range agent.Received() {
		if r := e.GetSessionRevocation(); r != nil {
			id := r.GetSessionId() + r.GetRelayGrantId()
			if got[id] {
				t.Errorf("duplicate revocation for %s", id)
			}
			got[id] = true
		}
	}
	if !maps.Equal(got, want) {
		t.Fatalf("revoked targets = %v, want %v (preserve sessions %s, %s and grants %s, %s)", got, want, newSession, ownerSession, newGrant, ownerGrant)
	}
	plane := env.App.Plane
	testenv.Eventually(t, "all four revocations acknowledged", func() bool { return plane.PendingRevocations() == 0 })
	if n := plane.SessionCount(); n != 2 {
		t.Fatalf("remaining session count = %d, want 2", n)
	}
	if plane.Grants.Admittable(newGrant) == nil || plane.Grants.Admittable(ownerGrant) == nil {
		t.Fatal("revocation removed an unrelated grant")
	}
	agent.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_Signaling{Signaling: &ownsandv1.SignalingMessage{
		Message: &ownsandv1.SignalingMessage_Answer{Answer: &ownsandv1.SignalingAnswer{SessionId: pendingSession, Sdp: "v=0 answer"}},
	}}})
	if err := <-sessionResult; testenv.Code(err) != connect.CodeUnavailable {
		t.Fatalf("pending session returned %v, want unavailable", err)
	}
	if err := <-grantResult; testenv.Code(err) != connect.CodeUnavailable {
		t.Fatalf("pending grant returned %v, want unavailable", err)
	}
	if err := execWith(newGrant, oldExec); err != nil {
		t.Fatalf("replacement membership grant failed after revocation: %v", err)
	}
	nextRequest := proto.CloneOf(newRequest)
	nextRequest.AuthorizationId, _ = uuidv7.NewString()
	nextRequest.ReplacesGrantId = &newGrant
	nextGrant := issueGrant(member, nextRequest)
	nextEnvelope := agent.WaitFor("same-membership replacement", func(e *ownsandv1.AgentEnvelope) bool {
		return e.GetRelayGrant().GetGrantId() == nextGrant
	})
	if predecessor := nextEnvelope.GetRelayGrant().GetReplacesGrantId(); predecessor != newGrant {
		t.Fatalf("same-membership predecessor = %q, want %q", predecessor, newGrant)
	}
	t.Log("the HTTP/2 agent acknowledged four old targets; two unaffected sessions and both unaffected grants survived")
}

func TestReauthorizeDataPathAfterMembershipReplacement(t *testing.T) {
	env := testenv.New(t, func(o *app.Options) { o.Timing.EventPoll = time.Hour })
	org, agent, sandboxID := runningSandbox(t, env)
	peer := env.StartPeer()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	// Both handlers read the sandbox after their first authorization and
	// before pending registration. Hold that read while the membership changes.
	tx, err := env.DB.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, "LOCK TABLE sandboxes IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	member := env.User(org.Member)
	sessionResult := make(chan error, 1)
	grantResult := make(chan error, 1)
	go func() {
		_, err := member.DataPath.OpenSession(ctx, connect.NewRequest(&ownsandv1.OpenSessionRequest{
			OrganizationId: org.ID, SandboxId: sandboxID, SdpOffer: "v=0",
		}))
		sessionResult <- err
	}()
	go func() {
		_, err := member.DataPath.AuthorizeRelay(ctx, connect.NewRequest(membershipGrantRequest(newExec(org.ID, sandboxID))))
		grantResult <- err
	}()
	testenv.Eventually(t, "both handlers waiting for the sandbox read", func() bool {
		var waiters int
		err := tx.QueryRow(ctx, `SELECT count(*) FROM pg_locks
			WHERE relation = 'sandboxes'::regclass AND mode = 'AccessShareLock' AND NOT granted`).Scan(&waiters)
		if err != nil {
			t.Fatal(err)
		}
		return waiters == 2
	})
	replaceMembership(t, peer, org)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-sessionResult; testenv.Code(err) != connect.CodeNotFound {
		t.Errorf("OpenSession returned %v, want not_found", err)
	}
	if err := <-grantResult; testenv.Code(err) != connect.CodeNotFound {
		t.Errorf("AuthorizeRelay returned %v, want not_found", err)
	}
	plane := env.App.Plane
	if plane.SessionCount() != 0 || plane.Grants.Len() != 0 || offersReceived(agent) != 0 || len(grantsReceived(agent)) != 0 {
		t.Fatal("a rejected handler kept pending work or sent it to the agent")
	}
	t.Log("both HTTP/2 calls returned not_found without leaving a grant, session, or agent message")
}
