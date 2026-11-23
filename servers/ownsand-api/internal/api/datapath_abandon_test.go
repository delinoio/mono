// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/app"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// shortSignaling shortens the signaling wait of OpenSession, so a test that
// lets it time out does not wait for the contract value.
func shortSignaling(d time.Duration) testenv.Option {
	return func(o *app.Options) {
		t := *o.RelayTiming
		t.Signaling = d
		o.RelayTiming = &t
	}
}

func openSessionRequest(orgID, sandboxID string) *connect.Request[ownsandv1.OpenSessionRequest] {
	return connect.NewRequest(&ownsandv1.OpenSessionRequest{OrganizationId: orgID, SandboxId: sandboxID, SdpOffer: "v=0"})
}

func isOffer(e *ownsandv1.AgentEnvelope) bool { return e.GetSignaling().GetOffer() != nil }

// silentAgent makes the fake agent receive offers without answering them.
func silentAgent(agent *testenv.FakeAgent) {
	agent.SetOnOffer(func(*ownsandv1.SignalingOffer) *ownsandv1.SignalingAnswer { return nil })
}

// revocationsOf returns the session revocations that the agent received for
// the session ID.
func revocationsOf(agent *testenv.FakeAgent, sessionID string) []*ownsandv1.SessionRevocation {
	var out []*ownsandv1.SessionRevocation
	for _, e := range agent.Received() {
		if r := e.GetSessionRevocation(); r != nil && r.GetSessionId() == sessionID {
			out = append(out, r)
		}
	}
	return out
}

// settleChannel makes a successful round trip over the agent channel. The
// channel is ordered, so every envelope that the server enqueued before the
// call has reached the agent when it returns.
func settleChannel(t *testing.T, env *testenv.Env, agent *testenv.FakeAgent, org *testenv.Org, sandboxID string) {
	t.Helper()
	agent.SetOnOffer(nil)
	resp, err := env.Clients(org.APIKey).DataPath.OpenSession(context.Background(), openSessionRequest(org.ID, sandboxID))
	if err != nil {
		t.Fatalf("settle round trip: %v", err)
	}
	agent.WaitFor("settle offer", func(e *ownsandv1.AgentEnvelope) bool {
		return e.GetSignaling().GetOffer().GetSessionId() == resp.Msg.GetSessionId()
	})
}

func ackRevocation(agent *testenv.FakeAgent, revocationID string) {
	agent.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_RevocationAck{RevocationAck: &ownsandv1.RevocationAck{RevocationId: revocationID}}})
}

// requireAbandonedSessionRevoked checks the effects of an OpenSession that
// ended after its offer was sent: the session is forgotten, the agent got one
// revocation for it, and the revocation stays pending until the agent
// acknowledges it; a repeated acknowledgement is harmless.
func requireAbandonedSessionRevoked(t *testing.T, env *testenv.Env, agent *testenv.FakeAgent, sessionID string) {
	t.Helper()
	rev := agent.WaitFor("revocation of the abandoned session", func(e *ownsandv1.AgentEnvelope) bool {
		return e.GetSessionRevocation().GetSessionId() == sessionID
	}).GetSessionRevocation()
	if n := env.App.Plane.OpenSessionCount(); n != 0 {
		t.Fatalf("tracked sessions = %d, want 0", n)
	}
	if n := env.App.Plane.PendingRevocations(); n != 1 {
		t.Fatalf("pending revocations = %d, want 1 until the acknowledgement", n)
	}
	ackRevocation(agent, rev.GetRevocationId())
	testenv.Eventually(t, "revocation acknowledged", func() bool { return env.App.Plane.PendingRevocations() == 0 })
	ackRevocation(agent, rev.GetRevocationId())
}

// A caller that cancels OpenSession after the offer reached the agent gets
// the session revoked on the originating channel, although the request
// context is canceled.
func TestOpenSessionCancelRevokesSession(t *testing.T) {
	env := testenv.New(t)
	org, agent, sandboxID := runningSandbox(t, env)
	agent.SetAckRevocations(false)
	silentAgent(agent)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errs := make(chan error, 1)
	go func() {
		_, err := env.Clients(org.APIKey).DataPath.OpenSession(ctx, openSessionRequest(org.ID, sandboxID))
		errs <- err
	}()
	offer := agent.WaitFor("offer", isOffer).GetSignaling().GetOffer()
	cancel()
	if err := <-errs; testenv.Code(err) != connect.CodeCanceled {
		t.Fatalf("OpenSession error: %v, want canceled", err)
	}
	requireAbandonedSessionRevoked(t, env, agent, offer.GetSessionId())
	if got := len(revocationsOf(agent, offer.GetSessionId())); got != 1 {
		t.Fatalf("revocations of the session = %d, want 1", got)
	}
}

// An OpenSession whose agent does not answer within the signaling wait
// revokes the session as well.
func TestOpenSessionSignalingTimeoutRevokesSession(t *testing.T) {
	env := testenv.New(t, shortSignaling(50*time.Millisecond))
	org, agent, sandboxID := runningSandbox(t, env)
	agent.SetAckRevocations(false)
	silentAgent(agent)
	_, err := env.Clients(org.APIKey).DataPath.OpenSession(context.Background(), openSessionRequest(org.ID, sandboxID))
	if testenv.Code(err) != connect.CodeUnavailable {
		t.Fatalf("OpenSession error: %v, want unavailable", err)
	}
	offer := agent.WaitFor("offer", isOffer).GetSignaling().GetOffer()
	requireAbandonedSessionRevoked(t, env, agent, offer.GetSessionId())
}

// The control cases send no revocation: a successful open keeps its session,
// and an agent error answer, a channel that closes while the offer waits and a
// failure before the offer is dispatched have nothing to revoke.
func TestOpenSessionEndsWithoutRevocation(t *testing.T) {
	env := testenv.New(t)
	org, agent, sandboxID := runningSandbox(t, env)
	key := env.Clients(org.APIKey)
	ctx := context.Background()

	resp, err := key.DataPath.OpenSession(ctx, openSessionRequest(org.ID, sandboxID))
	if err != nil {
		t.Fatal(err)
	}
	opened := resp.Msg.GetSessionId()

	agent.SetOnOffer(func(o *ownsandv1.SignalingOffer) *ownsandv1.SignalingAnswer {
		return &ownsandv1.SignalingAnswer{SessionId: o.GetSessionId(), Error: &ownsandv1.ErrorDetail{Code: ownsandv1.ErrorCode_ERROR_CODE_RESOURCE_EXHAUSTED, Message: "too many sessions"}}
	})
	if _, err := key.DataPath.OpenSession(ctx, openSessionRequest(org.ID, sandboxID)); testenv.Code(err) != connect.CodeResourceExhausted {
		t.Fatalf("agent error answer: %v", err)
	}
	settleChannel(t, env, agent, org, sandboxID)
	if n := len(revocationsOf(agent, opened)); n != 0 {
		t.Fatalf("a successful open was revoked %d times", n)
	}
	for _, e := range agent.Received() {
		if e.GetSessionRevocation() != nil {
			t.Fatalf("unexpected revocation %v", e.GetSessionRevocation())
		}
	}
	// The opened session and the settle session are tracked; the failed one
	// is not.
	if n := env.App.Plane.OpenSessionCount(); n != 2 {
		t.Fatalf("tracked sessions = %d, want 2", n)
	}
	if n := env.App.Plane.PendingRevocations(); n != 0 {
		t.Fatalf("pending revocations = %d, want 0", n)
	}

	// A channel that closes while the offer waits needs no revocation: the
	// agent ends its sessions with the channel.
	agent.SetOnOffer(func(*ownsandv1.SignalingOffer) *ownsandv1.SignalingAnswer {
		agent.Close()
		return nil
	})
	if _, err := key.DataPath.OpenSession(ctx, openSessionRequest(org.ID, sandboxID)); testenv.Code(err) != connect.CodeUnavailable {
		t.Fatalf("closed channel: %v, want unavailable", err)
	}
	if n := env.App.Plane.PendingRevocations(); n != 0 {
		t.Fatalf("pending revocations after a channel close = %d, want 0", n)
	}
	for _, e := range agent.Received() {
		if e.GetSessionRevocation() != nil {
			t.Fatalf("unexpected revocation after a channel close: %v", e.GetSessionRevocation())
		}
	}
}

// A session that a revocation event already removed was revoked by the event
// handler, so canceling its OpenSession later sends no second revocation.
func TestOpenSessionCancelAfterRevocationEventSendsNoSecondRevocation(t *testing.T) {
	env := testenv.New(t)
	org, agent, sandboxID := runningSandbox(t, env)
	silentAgent(agent)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errs := make(chan error, 1)
	go func() {
		_, err := env.Clients(org.APIKey).DataPath.OpenSession(ctx, openSessionRequest(org.ID, sandboxID))
		errs <- err
	}()
	offer := agent.WaitFor("offer", isOffer).GetSignaling().GetOffer()
	if _, err := org.OwnerClient.APIKey.RevokeApiKey(ctx, connect.NewRequest(&ownsandv1.RevokeApiKeyRequest{OrganizationId: org.ID, ApiKeyId: org.APIKeyID})); err != nil {
		t.Fatal(err)
	}
	agent.WaitFor("revocation by the event", func(e *ownsandv1.AgentEnvelope) bool {
		return e.GetSessionRevocation().GetSessionId() == offer.GetSessionId()
	})
	cancel()
	<-errs
	// The channel is ordered, so a later offer proves that nothing else was
	// queued before it. The API key is revoked, so use the owner.
	agent.SetOnOffer(nil)
	resp, err := org.OwnerClient.DataPath.OpenSession(context.Background(), openSessionRequest(org.ID, sandboxID))
	if err != nil {
		t.Fatal(err)
	}
	agent.WaitFor("later offer", func(e *ownsandv1.AgentEnvelope) bool {
		return e.GetSignaling().GetOffer().GetSessionId() == resp.Msg.GetSessionId()
	})
	if got := len(revocationsOf(agent, offer.GetSessionId())); got != 1 {
		t.Fatalf("revocations of the session = %d, want 1", got)
	}
}
