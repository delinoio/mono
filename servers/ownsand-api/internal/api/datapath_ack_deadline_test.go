// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/go/ownsand/relaybinding"
	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/app"
	"github.com/delinoio/mono/servers/ownsand-api/internal/grants"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// TestAuthorizeRelayAckAfterAdmissionDeadline covers an acknowledgement that
// arrives after the admission deadline of the pending grant while the caller
// still waits: the grant is not installed, the caller gets unavailable without
// a retry delay (not a grant-limit result and not a grant that Admittable would
// reject), and the authorization_id is free for a retry.
func TestAuthorizeRelayAckAfterAdmissionDeadline(t *testing.T) {
	// The acknowledgement wait is far longer than the test, so the wait ends
	// only by the acknowledgement and never by its own timeout.
	env := testenv.New(t, func(o *app.Options) {
		timing := *o.RelayTiming
		timing.GrantAck = time.Minute
		o.RelayTiming = &timing
	})
	org, agent, sandboxID := runningSandbox(t, env)
	req := newExec(org.ID, sandboxID)
	authID, _ := uuidv7.NewString()
	areq := &ownsandv1.AuthorizeRelayRequest{OrganizationId: org.ID, SandboxId: sandboxID, OperationKind: ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC,
		OperationId: req.GetExecId(), RequestDigest: relaybinding.ExecDigest(req), AuthorizationId: authID}
	receivedCh := make(chan *ownsandv1.RelayGrant, 1)
	agent.SetOnGrant(func(g *ownsandv1.RelayGrant) *ownsandv1.RelayGrantAck { receivedCh <- g; return nil })
	t.Cleanup(func() { agent.SetOnGrant(nil) })
	// The cleanup cancels the call when the test fails before it returns.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		_, err := org.OwnerClient.DataPath.AuthorizeRelay(ctx, connect.NewRequest(areq))
		errCh <- err
	}()
	received, err := awaitRelayGrant(receivedCh, errCh, time.After(10*time.Second))
	if err != nil {
		t.Fatalf("waiting for the relay grant: %v", err)
	}
	// The grant lifetime and the admission tolerance pass before the agent
	// acknowledges.
	env.Clock.Advance(grants.Lifetime + grants.AdmissionTolerance)
	agent.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_RelayGrantAck{RelayGrantAck: &ownsandv1.RelayGrantAck{
		GrantId: received.GetGrantId(), Result: ownsandv1.RelayGrantResult_RELAY_GRANT_RESULT_ACCEPTED}}})
	select {
	case err = <-errCh:
	case <-time.After(10 * time.Second):
		t.Fatal("AuthorizeRelay did not return after the late acknowledgement")
	}
	if testenv.Code(err) != connect.CodeUnavailable {
		t.Fatalf("late acknowledgement: %v", err)
	}
	if detail := errDetail(err); detail.GetRetryAfter() != nil {
		t.Fatalf("late acknowledgement carries a retry delay: %v", err)
	}
	// The removal path answers; the acknowledgement timeout did not.
	if !strings.Contains(err.Error(), "channel closed before it acknowledged") {
		t.Fatalf("late acknowledgement was not answered by the removal path: %v", err)
	}
	if env.App.Plane.Grants.Get(received.GetGrantId()) != nil || env.App.Plane.Grants.Admittable(received.GetGrantId()) != nil {
		t.Fatal("the late acknowledgement installed the expired grant")
	}
}
