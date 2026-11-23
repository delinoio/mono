// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// initialStateReportFault faults the update of the device row that the
// reconciliation of a first state report makes (OFFLINE to ONLINE). The
// authentication, capability reports and heartbeats do not change the state.
func initialStateReportFault(t *testing.T, env *testenv.Env) *updateFault {
	return newUpdateFault(t, env, "state_report_fault", "devices",
		fmt.Sprintf("OLD.state <> %d AND NEW.state = %d", ownsandv1.DeviceState_DEVICE_STATE_ONLINE, ownsandv1.DeviceState_DEVICE_STATE_ONLINE))
}

// laterStateReportFault faults the update of the device row that the
// reconciliation of a report with a new state epoch makes on an ONLINE
// device. Authentication sets the epoch it receives before the channel opens,
// so only the report matches.
func laterStateReportFault(t *testing.T, env *testenv.Env) *updateFault {
	return newUpdateFault(t, env, "state_report_fault", "devices",
		fmt.Sprintf("NEW.state = %d AND NEW.state_epoch IS DISTINCT FROM OLD.state_epoch", ownsandv1.DeviceState_DEVICE_STATE_ONLINE))
}

// stateAcks returns the state report acknowledgements that the agent received.
func stateAcks(agent *testenv.FakeAgent) []*ownsandv1.StateReportAck {
	var out []*ownsandv1.StateReportAck
	for _, env := range agent.Received() {
		if a := env.GetStateAck().GetStateReport(); a != nil {
			out = append(out, a)
		}
	}
	return out
}

func waitHeartbeatAck(agent *testenv.FakeAgent, seq uint64) {
	agent.WaitFor(fmt.Sprintf("heartbeat ack %d", seq), func(e *ownsandv1.AgentEnvelope) bool {
		return e.GetHeartbeatAck() != nil && e.GetHeartbeatAck().GetSequence() == seq
	})
}

// TestFailedInitialStateReportClosesChannel checks that a channel whose first
// state report fails in the database is closed without an acknowledgement and
// is not eligible, and that the replacement channel of the agent reconciles,
// is acknowledged and becomes eligible for placement.
func TestFailedInitialStateReportClosesChannel(t *testing.T) {
	env := testenv.New(t)
	org := env.NewOrg("alice")
	dev := env.EnrollDevice(org.ID, org.OwnerClient, ownsandv1.HostOs_HOST_OS_LINUX, ownsandv1.Arch_ARCH_AMD64)
	if _, err := org.OwnerClient.Device.UpdateDeviceLimits(context.Background(), connect.NewRequest(&ownsandv1.UpdateDeviceLimitsRequest{
		OrganizationId: org.ID, DeviceId: dev.ID,
		Limits: &ownsandv1.DeviceLimits{VcpuCount: 4, MemoryMib: 8192, DiskGib: 100, VcpuOvercommitPercent: 100},
	})); err != nil {
		t.Fatal(err)
	}
	fault := initialStateReportFault(t, env)
	fault.on("40001", 1)

	failed := env.NewFakeAgent(dev)
	failed.OpenChannel()
	select {
	case <-failed.Closed():
	case <-time.After(10 * time.Second):
		t.Fatal("the server kept the channel open after the failed first state report")
	}
	if acks := stateAcks(failed); len(acks) != 0 {
		t.Fatalf("state acknowledgements after the failed report = %v, want none", acks)
	}
	if fault.calls() != 1 {
		t.Fatalf("the fault was reached %d times, want 1", fault.calls())
	}
	testenv.Eventually(t, "the failed channel to leave the hub", func() bool { return env.App.Service.Hub.Get(dev.ID) == nil })
	if env.App.Service.Hub.Live(dev.ID) != nil {
		t.Fatal("the device is eligible after its first state report failed")
	}
	logs := env.Logs.String()
	if !strings.Contains(logs, "agent_state_report_failed") || !strings.Contains(logs, "the first state report of the channel could not be applied") {
		t.Fatalf("the failure and the close cause are not logged: %s", logs)
	}
	if strings.Contains(logs, "agent_envelope_failed") {
		t.Fatalf("an unreconciled channel must not take the open-channel failure path: %s", logs)
	}
	if _, err := org.OwnerClient.Sandbox.CreateSandbox(context.Background(), connect.NewRequest(createReq(org.ID))); err == nil {
		t.Fatal("a sandbox was placed on a device without a reconciled channel")
	}

	// The database recovers (the fault was one-shot) and the agent connects
	// again, as its reconnect backoff does.
	replacement := env.NewFakeAgent(dev)
	replacement.Connect()
	replacement.WaitFor("the state acknowledgement of the replacement channel", func(e *ownsandv1.AgentEnvelope) bool {
		return e.GetStateAck().GetStateReport() != nil
	})
	if acks := stateAcks(replacement); len(acks) != 1 {
		t.Fatalf("state acknowledgements of the replacement channel = %v, want one", acks)
	}
	if env.App.Service.Hub.Live(dev.ID) == nil {
		t.Fatal("the device is not eligible after the replacement channel reconciled")
	}
	created, err := org.OwnerClient.Sandbox.CreateSandbox(context.Background(), connect.NewRequest(createReq(org.ID)))
	if err != nil {
		t.Fatalf("placement after the replacement channel reconciled: %v", err)
	}
	waitState(t, org.OwnerClient, org.ID, created.Msg.GetSandbox().GetSandboxId(), ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
}

// TestHealthyInitialStateReportKeepsChannel checks that a first state report
// that applies keeps the channel open, is acknowledged once and leaves the
// device eligible.
func TestHealthyInitialStateReportKeepsChannel(t *testing.T) {
	env := testenv.New(t)
	org := env.NewOrg("alice")
	dev, agent := setupAgent(t, env, org, nil)
	agent.Heartbeat(1)
	waitHeartbeatAck(agent, 1)
	select {
	case <-agent.Closed():
		t.Fatalf("the channel ended after a healthy first report: %v", agent.CloseErr())
	default:
	}
	if acks := stateAcks(agent); len(acks) != 1 {
		t.Fatalf("state acknowledgements = %v, want one", acks)
	}
	if env.App.Service.Hub.Live(dev.ID) == nil {
		t.Fatal("the device is not eligible after a healthy first report")
	}
	if strings.Contains(env.Logs.String(), "agent_state_report_failed") {
		t.Fatal("a healthy report logged a failure")
	}
}

// TestFailedLaterStateReportKeepsChannel checks that a failed state report on
// an already reconciled channel is only logged: the channel stays open and
// eligible, no acknowledgement is sent for the report and heartbeats continue.
func TestFailedLaterStateReportKeepsChannel(t *testing.T) {
	env := testenv.New(t)
	org := env.NewOrg("alice")
	dev, agent := setupAgent(t, env, org, nil)
	fault := laterStateReportFault(t, env)
	fault.on("40001", 1)

	// A new state epoch makes the reconciliation update the device row, which
	// the fault matches.
	epoch, err := uuidv7.NewString()
	if err != nil {
		t.Fatal(err)
	}
	dev.Epoch = epoch
	agent.SendStateReport()
	testenv.Eventually(t, "the fault to be reached", func() bool { return fault.calls() >= 1 })
	testenv.Eventually(t, "the failure to be logged", func() bool {
		return strings.Contains(env.Logs.String(), "agent_envelope_failed")
	})
	agent.Heartbeat(1)
	waitHeartbeatAck(agent, 1)
	select {
	case <-agent.Closed():
		t.Fatalf("the channel ended after a failed later report: %v", agent.CloseErr())
	default:
	}
	if acks := stateAcks(agent); len(acks) != 1 {
		t.Fatalf("state acknowledgements = %v, want only the one of the initial report", acks)
	}
	if env.App.Service.Hub.Live(dev.ID) == nil {
		t.Fatal("the device lost its eligibility after a failed later report")
	}
	if strings.Contains(env.Logs.String(), "agent_state_report_failed") {
		t.Fatal("a reconciled channel took the initial report failure path")
	}
}
