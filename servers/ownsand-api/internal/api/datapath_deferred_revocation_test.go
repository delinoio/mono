// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/events"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

func TestOpenSessionDeferredRevocation(t *testing.T) {
	env := testenv.NewWithoutPeriodicRevocationSweep(t, shortSignaling(time.Minute))
	env.App.Service.Hub.QueueFullWait = committedStateQueueFullWait
	org, agent, sandboxID := runningSandbox(t, env)
	agent.SetAckRevocations(false)
	silentAgent(agent)
	grant, err := authorizeExec(t, env.Clients(org.APIKey), newExec(org.ID, sandboxID))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	result := make(chan error, 1)
	go func() {
		_, err := env.Clients(org.APIKey).DataPath.OpenSession(ctx, openSessionRequest(org.ID, sandboxID))
		result <- err
	}()
	offer := agent.WaitFor("offer", isOffer).GetSignaling().GetOffer()
	saturate(t, env, agent)
	t.Cleanup(agent.ResumeReads)

	conn := env.App.Service.Hub.Get(agent.Device.ID)
	cancel()
	select {
	case err := <-result:
		if testenv.Code(err) != connect.CodeCanceled {
			t.Fatalf("OpenSession error: %v, want canceled", err)
		}
	case <-time.After(rpcTimeout):
		t.Fatal("the canceled OpenSession did not return")
	}
	var sessionRevocationID string
	testenv.Eventually(t, "session revocation enqueue deferred", func() bool {
		sessionRevocationID = deferredRevocationID(t, env, agent.Device.ID, "session")
		return sessionRevocationID != ""
	})

	// Apply the same event that API key revocation writes. The short event
	// context ends before the hub would close the saturated channel.
	eventCtx, cancelEvent := context.WithTimeout(t.Context(), enqueueFailureDeadline)
	defer cancelEvent()
	env.App.Plane.HandleEvent(eventCtx, events.Event{Kind: events.KindAPIKeyRevoked, APIKeyID: org.APIKeyID})
	grantRevocationID := deferredRevocationID(t, env, agent.Device.ID, "relay_grant")
	if grantRevocationID == "" || grantRevocationID == sessionRevocationID {
		t.Fatalf("expected a distinct deferred relay grant revocation, got %q", grantRevocationID)
	}

	start := time.Now()
	if n := sweepRevocationsNow(env); n != 2 {
		t.Fatalf("pending revocations on the saturated channel = %d, want 2", n)
	}
	if elapsed := time.Since(start); elapsed >= abandonEnqueueBudget {
		t.Fatalf("the sweep waited %v on the saturated channel", elapsed)
	}
	if env.App.Service.Hub.Get(agent.Device.ID) != conn {
		t.Fatal("the original channel must stay open")
	}

	agent.ResumeReads()
	want := map[string]string{
		sessionRevocationID: offer.GetSessionId(),
		grantRevocationID:   grant.GetGrantId(),
	}
	testenv.Eventually(t, "both deferred revocations received", func() bool {
		env.App.Plane.SweepExpired(t.Context())
		received := 0
		for _, msg := range agent.Received() {
			if _, ok := want[msg.GetSessionRevocation().GetRevocationId()]; ok {
				received++
			}
		}
		return received >= len(want)
	})
	if n := sweepRevocationsNow(env); n != 2 {
		t.Fatalf("pending revocations before acknowledgement = %d, want 2", n)
	}
	agent.Heartbeat(901)
	agent.WaitFor("heartbeat ack after the second sweep", func(e *ownsandv1.AgentEnvelope) bool {
		return e.GetHeartbeatAck().GetSequence() == 901
	})
	for _, msg := range agent.Received() {
		r := msg.GetSessionRevocation()
		if r == nil {
			continue
		}
		id := r.GetRevocationId()
		if target, ok := want[id]; !ok || (r.GetSessionId() != target && r.GetRelayGrantId() != target) {
			t.Fatalf("unexpected or repeated revocation %v", r)
		}
		delete(want, id)
		if !logHasRevocationSent(env.Logs.String(), id) {
			t.Fatalf("missing sent event for revocation %s", id)
		}
		ackRevocation(agent, id)
	}
	if len(want) != 0 {
		t.Fatalf("missing revocations %v", want)
	}
	testenv.Eventually(t, "both revocations acknowledged", func() bool {
		return env.App.Plane.PendingRevocations() == 0
	})
}

const abandonEnqueueBudget = 2 * time.Second

func deferredRevocationID(t *testing.T, env *testenv.Env, deviceID, kind string) string {
	t.Helper()
	for line := range strings.SplitSeq(env.Logs.String(), "\n") {
		if !strings.Contains(line, "session_revocation_enqueue_deferred") {
			continue
		}
		var logged struct {
			DeviceID     string `json:"device_id"`
			RevocationID string `json:"revocation_id"`
			Target       string `json:"target"`
		}
		if err := json.Unmarshal([]byte(line), &logged); err != nil {
			t.Fatal(err)
		}
		if logged.DeviceID == deviceID && logged.Target == kind {
			return logged.RevocationID
		}
	}
	return ""
}

func logHasRevocationSent(logs, id string) bool {
	for line := range strings.SplitSeq(logs, "\n") {
		if strings.Contains(line, "session_revocation_sent") && strings.Contains(line, id) {
			return true
		}
	}
	return false
}
