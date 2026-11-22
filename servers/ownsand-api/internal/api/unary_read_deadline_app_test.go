// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
	"github.com/delinoio/mono/servers/ownsand-api/internal/app"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// TestUnaryBodyReadDeadlineOnTheWiredApp checks the wiring of
// api.UnaryReadDeadline in the control plane: a unary RPC whose body arrives
// after the deadline ends with deadline_exceeded, one whose body arrives in
// time succeeds, and the agent channel and the relay RPCs are not bounded by
// it (the relay keeps its own request deadline).
func TestUnaryBodyReadDeadlineOnTheWiredApp(t *testing.T) {
	const deadline = 400 * time.Millisecond
	env := testenv.New(t, func(o *app.Options) { o.Timing.UnaryBodyReadDeadline = deadline })
	org, agent, sandboxID := runningSandbox(t, env)
	owner := testenv.UserToken(org.Owner)
	ctx := context.Background()

	sandboxClient := func(delay time.Duration) ownsandv1connect.SandboxServiceClient {
		httpClient := &http.Client{Transport: bodyDelayTransport{base: env.Server.Client().Transport, delay: delay, token: owner}}
		return ownsandv1connect.NewSandboxServiceClient(httpClient, env.Server.URL)
	}
	get := func(delay time.Duration) (time.Duration, error) {
		start := time.Now()
		_, err := sandboxClient(delay).GetSandbox(ctx, connect.NewRequest(&ownsandv1.GetSandboxRequest{OrganizationId: org.ID, SandboxId: sandboxID}))
		return time.Since(start), err
	}

	t.Run("stalled unary body", func(t *testing.T) {
		took, err := get(10 * time.Second)
		if testenv.Code(err) != connect.CodeDeadlineExceeded {
			t.Fatalf("stalled body: %v after %v, want deadline_exceeded", err, took)
		}
		if took > 5*time.Second {
			t.Fatalf("stalled body ended after %v, want about the %v deadline", took, deadline)
		}
	})
	t.Run("slow unary body within the deadline", func(t *testing.T) {
		if took, err := get(deadline / 4); err != nil {
			t.Fatalf("slow body: %v after %v", err, took)
		}
	})
	t.Run("agent channel stays open", func(t *testing.T) {
		time.Sleep(3 * deadline)
		agent.Heartbeat(42)
		agent.WaitFor("heartbeat ack after three deadlines", func(e *ownsandv1.AgentEnvelope) bool { return e.GetHeartbeatAck().GetSequence() == 42 })
		select {
		case <-agent.Closed():
			t.Fatalf("the agent channel ended: %v", agent.CloseErr())
		default:
		}
	})
	t.Run("relay RPCs keep their own deadline", func(t *testing.T) {
		r := agent.ConnectRelay(serveRelay)
		r.WaitFrame("supported frame kinds", func(f *ownsandv1.DataFrame) bool { return f.GetSupportedFrameKinds() != nil })
		stop := keepAlive(r)
		defer stop()
		// The upload takes twice the unary deadline and stays within the relay
		// request deadline of the test environment (two seconds).
		for _, tc := range relayCases() {
			if _, err := tc.call(t, env, org, sandboxID, 2*deadline); err != nil && !(tc.name == "AttachExec" && testenv.Code(err) == connect.CodeNotFound) {
				t.Fatalf("%s with an upload of %v: %v", tc.name, 2*deadline, err)
			}
		}
	})
}
