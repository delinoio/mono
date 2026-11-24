// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

var invalidStateEpochs = []struct {
	name  string
	value string
}{
	{name: "NUL", value: "bad\x00epoch"},
	{name: "malformed", value: "not-a-uuid"},
	{name: "empty", value: ""},
	{name: "UUIDv4", value: "0192abcd-1234-4000-8000-000000000000"},
}

func stateReportEnvelope(report *ownsandv1.StateReport) *ownsandv1.AgentEnvelope {
	return &ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_StateReport{StateReport: report}}
}

func requireDeviceStateAndEpoch(t *testing.T, env *testenv.Env, deviceID string, wantState ownsandv1.DeviceState, wantEpoch string) {
	t.Helper()
	var state int16
	var epoch string
	if err := env.DB.Pool.QueryRow(context.Background(), "SELECT state, state_epoch FROM devices WHERE device_id = $1", deviceID).Scan(&state, &epoch); err != nil {
		t.Fatal(err)
	}
	if ownsandv1.DeviceState(state) != wantState || epoch != wantEpoch {
		t.Fatalf("stored device state and epoch = (%v, %q), want (%v, %q)", ownsandv1.DeviceState(state), epoch, wantState, wantEpoch)
	}
}

func requireRejectedStateEpochLog(t *testing.T, logs, deviceID, epoch string) {
	t.Helper()
	for _, unexpected := range []string{"SQLSTATE 22021", "agent_state_report_failed", "agent_envelope_failed", "internal_error"} {
		if strings.Contains(logs, unexpected) {
			t.Fatalf("unexpected %q log after rejection: %s", unexpected, logs)
		}
	}
	for line := range strings.SplitSeq(logs, "\n") {
		if !strings.Contains(line, `"msg":"agent_state_report_rejected"`) {
			continue
		}
		var entry struct {
			Level      string `json:"level"`
			DeviceID   string `json:"device_id"`
			ChannelID  string `json:"channel_id"`
			Field      string `json:"field"`
			ValueBytes *int   `json:"value_bytes"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err)
		}
		if entry.Level != "WARN" || entry.DeviceID != deviceID || entry.ChannelID == "" ||
			entry.Field != "state_epoch" || entry.ValueBytes == nil || *entry.ValueBytes != len(epoch) {
			t.Fatalf("incorrect rejection log fields: %s", line)
		}
		if epoch != "" {
			encoded, _ := json.Marshal(epoch)
			if strings.Contains(logs, strings.Trim(string(encoded), `"`)) {
				t.Fatal("the invalid epoch appeared in the logs")
			}
		}
		return
	}
	t.Fatalf("missing agent_state_report_rejected log: %s", logs)
}

func TestInvalidInitialStateEpochClosesChannel(t *testing.T) {
	for _, tc := range invalidStateEpochs {
		t.Run(tc.name, func(t *testing.T) {
			env := testenv.New(t)
			org := env.NewOrg("alice")
			dev := env.EnrollDevice(org.ID, org.OwnerClient, ownsandv1.HostOs_HOST_OS_LINUX, ownsandv1.Arch_ARCH_AMD64)
			if _, err := org.OwnerClient.Device.UpdateDeviceLimits(context.Background(), connect.NewRequest(&ownsandv1.UpdateDeviceLimitsRequest{
				OrganizationId: org.ID, DeviceId: dev.ID,
				Limits: &ownsandv1.DeviceLimits{VcpuCount: 4, MemoryMib: 8192, DiskGib: 100, VcpuOvercommitPercent: 100},
			})); err != nil {
				t.Fatal(err)
			}
			env.WaitForLease()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			stream := env.Clients(env.SessionCredential(dev)).Agent.Channel(ctx)
			defer stream.CloseResponse()
			if err := stream.Send(&ownsandv1.ChannelRequest{Envelope: stateReportEnvelope(&ownsandv1.StateReport{
				ReportSequence: 1, StateEpoch: tc.value,
			})}); err != nil {
				t.Fatal(err)
			}
			for {
				reply, err := stream.Receive()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatalf("receive on rejected channel: %v", err)
				}
				if reply.GetEnvelope().GetStateAck() != nil {
					t.Fatal("the server acknowledged an invalid initial report")
				}
			}
			if env.App.Service.Hub.Live(dev.ID) != nil {
				t.Fatal("the device became eligible after an invalid initial report")
			}
			requireDeviceStateAndEpoch(t, env, dev.ID, ownsandv1.DeviceState_DEVICE_STATE_OFFLINE, dev.Epoch)
			logs := env.Logs.String()
			requireRejectedStateEpochLog(t, logs, dev.ID, tc.value)
			if !strings.Contains(logs, "the first state report of the channel could not be applied") {
				t.Fatal("the initial report rejection did not close the channel with the expected cause")
			}
		})
	}
}

func TestInvalidLaterStateEpochKeepsChannel(t *testing.T) {
	env := testenv.New(t)
	org := env.NewOrg("alice")
	dev, agent := setupAgent(t, env, org, nil)
	created, err := org.OwnerClient.Sandbox.CreateSandbox(context.Background(), connect.NewRequest(createReq(org.ID)))
	if err != nil {
		t.Fatal(err)
	}
	id := created.Msg.GetSandbox().GetSandboxId()
	running := ownsandv1.SandboxState_SANDBOX_STATE_RUNNING
	waitState(t, org.OwnerClient, org.ID, id, running)

	// A full report that omits the running sandbox would mark it as lost.
	for i, tc := range invalidStateEpochs {
		t.Run(tc.name, func(t *testing.T) {
			before := len(env.Logs.String())
			seq := uint64(i + 2)
			agent.Send(stateReportEnvelope(&ownsandv1.StateReport{ReportSequence: seq, StateEpoch: tc.value}))
			agent.Heartbeat(seq)
			waitHeartbeatAck(agent, seq)
			if acks := stateAcks(agent); len(acks) != 1 {
				t.Fatalf("state acknowledgements = %v, want only the initial acknowledgement", acks)
			}
			requireDeviceStateAndEpoch(t, env, dev.ID, ownsandv1.DeviceState_DEVICE_STATE_ONLINE, dev.Epoch)
			if state := getSandbox(t, org.OwnerClient, org.ID, id).GetState(); state != running {
				t.Fatalf("sandbox state after rejected report = %v, want %v", state, running)
			}
			requireRejectedStateEpochLog(t, env.Logs.String()[before:], dev.ID, tc.value)
		})
	}

	const epoch = "0192ABCD-1234-7000-8000-000000000000"
	seq := uint64(len(invalidStateEpochs) + 2)
	agent.Send(stateReportEnvelope(&ownsandv1.StateReport{
		ReportSequence: seq, StateEpoch: epoch, Sandboxes: []*ownsandv1.SandboxStateEntry{agent.Sandbox(id)},
	}))
	agent.WaitFor("acknowledgement of valid later report", func(e *ownsandv1.AgentEnvelope) bool {
		return e.GetStateAck().GetStateReport().GetReportSequence() == seq
	})
	requireDeviceStateAndEpoch(t, env, dev.ID, ownsandv1.DeviceState_DEVICE_STATE_ONLINE, epoch)
}
