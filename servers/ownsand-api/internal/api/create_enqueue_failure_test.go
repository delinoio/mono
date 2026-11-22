// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// TestCreateSandboxEnqueueFailureFailsCreate checks that a CreateSandbox
// whose request ends before the create command is queued does not leave the
// committed CREATING reservation behind: the sandbox becomes FAILED with
// CREATE_FAILED, its capacity is released although the request context that
// caused the failure is done, the channel stays open, and a replay of the
// idempotency key returns the failed record and queues no command.
func TestCreateSandboxEnqueueFailureFailsCreate(t *testing.T) {
	env := testenv.New(t)
	env.App.Service.Hub.QueueFullWait = 30 * time.Second
	ctx := context.Background()
	org := env.NewOrg("alice")
	c := org.OwnerClient
	// The device has room for exactly one create, so a released reservation
	// is visible as a second create that places.
	dev, agent := setupAgent(t, env, org, &ownsandv1.DeviceLimits{VcpuCount: 2, MemoryMib: 8192, DiskGib: 100, VcpuOvercommitPercent: 100})

	saturate(t, env, agent)
	req := createReq(org.ID)
	// The queue-full wait is far longer than the test, so once the placement
	// committed the create command send waits for room that never comes.
	// Canceling the request then ends its context while the channel stays
	// open.
	rpcCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	rpcDone := make(chan error, 1)
	go func() {
		_, err := c.Sandbox.CreateSandbox(rpcCtx, connect.NewRequest(req))
		rpcDone <- err
	}()
	testenv.Eventually(t, "sandbox record committed", func() bool {
		list, err := c.Sandbox.ListSandboxes(ctx, connect.NewRequest(&ownsandv1.ListSandboxesRequest{OrganizationId: org.ID, IncludeEnded: true}))
		return err == nil && len(list.Msg.GetSandboxes()) == 1
	})
	cancel()
	if err := <-rpcDone; err == nil {
		t.Fatal("CreateSandbox on a saturated channel succeeded")
	}

	list, err := c.Sandbox.ListSandboxes(ctx, connect.NewRequest(&ownsandv1.ListSandboxesRequest{OrganizationId: org.ID, IncludeEnded: true}))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(list.Msg.GetSandboxes()); n != 1 {
		t.Fatalf("sandboxes = %d, want the one record of the failed attempt", n)
	}
	sandboxID := list.Msg.GetSandboxes()[0].GetSandboxId()
	assertImportCreateFailed(t, org, sandboxID, dev.ID)
	if conn := env.App.Service.Hub.Get(dev.ID); conn == nil || conn.Context().Err() != nil {
		t.Fatal("the channel must stay connected")
	}

	// The idempotency key replays the failed record; it does not start a
	// new create.
	replay, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(req))
	if err != nil {
		t.Fatal(err)
	}
	if got := replay.Msg.GetSandbox(); got.GetSandboxId() != sandboxID || got.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_FAILED ||
		got.GetEndReason() != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_CREATE_FAILED {
		t.Fatalf("replay = %v, want the failed record %s", got, sandboxID)
	}

	// The agent reads again: no create command for the failed sandbox
	// arrived, and the released capacity serves a new create.
	agent.ResumeReads()
	agent.Heartbeat(901)
	agent.WaitFor("heartbeat ack after the resume", func(e *ownsandv1.AgentEnvelope) bool { return e.GetHeartbeatAck().GetSequence() == 901 })
	for _, cmd := range agent.Commands() {
		if cmd.GetCreateSandbox().GetSandboxId() == sandboxID {
			t.Fatal("the agent received the create command of the failed sandbox")
		}
	}
	next, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(createReq(org.ID)))
	if err != nil {
		t.Fatalf("a create after the failed one: %v", err)
	}
	waitState(t, c, org.ID, next.Msg.GetSandbox().GetSandboxId(), ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
}
