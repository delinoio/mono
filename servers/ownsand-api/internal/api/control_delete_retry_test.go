// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/api"
	"github.com/delinoio/mono/servers/ownsand-api/internal/app"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// TestSandboxDeleteEventRetriesAfterLookupFailure drives a real control plane
// and a real agent channel. The first lookup of the sandbox of the delete
// event fails with a database-style error. The event processor must not move
// its cursor past the event: the next poll delivers the delete command to the
// original channel, without a second DeleteSandbox call, a state report or a
// reconnect.
func TestSandboxDeleteEventRetriesAfterLookupFailure(t *testing.T) {
	timing := api.Timing{HeartbeatTimeout: 45 * time.Second, CommandWait: 500 * time.Millisecond, EventPoll: 50 * time.Millisecond, OfflineSweep: time.Hour, CleanupPoll: time.Hour, ImportWait: time.Second}
	env := testenv.New(t, func(o *app.Options) { o.Timing = &timing })
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

	control := env.App.Control
	var lookups atomic.Int32
	var realLookup func(ctx context.Context, orgID, sandboxID string) (*api.SandboxRowForTest, error)
	realLookup = control.SetLoadDeleteTargetForTest(func(ctx context.Context, orgID, sandboxID string) (*api.SandboxRowForTest, error) {
		if lookups.Add(1) == 1 {
			return nil, errors.New("injected lookup failure")
		}
		return realLookup(ctx, orgID, sandboxID)
	})

	deleteDone := make(chan error, 1)
	go func() {
		_, err := c.Sandbox.DeleteSandbox(ctx, connect.NewRequest(&ownsandv1.DeleteSandboxRequest{OrganizationId: org.ID, SandboxId: sandboxID}))
		deleteDone <- err
	}()
	agent.WaitFor("the delete command after the retry", func(e *ownsandv1.AgentEnvelope) bool {
		return e.GetCommand().GetDeleteSandbox().GetSandboxId() == sandboxID
	})
	if err := <-deleteDone; err != nil {
		t.Fatalf("DeleteSandbox: %v", err)
	}
	waitState(t, c, org.ID, sandboxID, ownsandv1.SandboxState_SANDBOX_STATE_DELETED)
	if got := lookups.Load(); got != 2 {
		t.Fatalf("lookups = %d, want 2 (the injected failure and the retry)", got)
	}
	logs := env.Logs.String()
	for _, want := range []string{`"msg":"control_event_retry"`, `"sandbox_id":"` + sandboxID + `"`, `"event_id":`, "injected lookup failure"} {
		if !strings.Contains(logs, want) {
			t.Fatalf("logs lack %s:\n%s", want, logs)
		}
	}
	deletes := 0
	for _, cmd := range agent.Commands() {
		if cmd.GetDeleteSandbox().GetSandboxId() == sandboxID {
			deletes++
		}
	}
	if deletes != 1 {
		t.Fatalf("delete commands = %d, want exactly one", deletes)
	}
}
