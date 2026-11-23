// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/app"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// TestPauseCommandIDCapturedAfterRPCReturns pins the delivery race that the
// command-expiry tests used to lose: the agent stops reading its channel, so
// PauseSandbox ends its command wait before the agent recorded the command.
// A one-time snapshot of the received commands at that point is empty, while
// the bounded wait finds the command once the agent reads again.
func TestPauseCommandIDCapturedAfterRPCReturns(t *testing.T) {
	timing := commandExpiryTiming()
	env := testenv.New(t, func(o *app.Options) { o.Timing = timing })
	ctx := context.Background()
	org := env.NewOrg("alice")
	c := org.OwnerClient
	_, agent := setupAgent(t, env, org, nil)
	created, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(createReq(org.ID)))
	if err != nil {
		t.Fatal(err)
	}
	sandboxID := created.Msg.GetSandbox().GetSandboxId()
	waitState(t, c, org.ID, sandboxID, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)

	// The read loop arms the stall itself when it receives the heartbeat
	// acknowledgement, so the stall is in effect once WaitFor returns.
	withholdAll(agent)
	agent.StallReadsAfter(func(e *ownsandv1.AgentEnvelope) bool { return e.GetHeartbeatAck().GetSequence() == 700 })
	agent.Heartbeat(700)
	agent.WaitFor("stalling heartbeat ack", func(e *ownsandv1.AgentEnvelope) bool { return e.GetHeartbeatAck().GetSequence() == 700 })

	if _, err := c.Sandbox.PauseSandbox(ctx, connect.NewRequest(&ownsandv1.PauseSandboxRequest{OrganizationId: org.ID, SandboxId: sandboxID})); err != nil {
		t.Fatal(err)
	}
	// The agent did not read since the stall, so the RPC returned before
	// the command arrived: the snapshot that the old tests took is empty.
	for _, cmd := range agent.Commands() {
		if cmd.GetPauseSandbox() != nil {
			t.Fatal("the stalled agent received the pause command before the RPC returned")
		}
	}

	agent.ResumeReads()
	// waitPauseCommandID fails on an empty command ID, and its wait is
	// bounded by FakeAgent.WaitFor.
	waitPauseCommandID(t, agent, sandboxID)
}
