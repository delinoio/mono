package agent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/channel"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/controlplane"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/cptest"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/runtime/runtimetest"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/state"
	"github.com/delinoio/mono/packages/ownsand-sdk-go/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type harness struct {
	t     *testing.T
	cp    *cptest.Server
	dir   *state.Dir
	rt    *runtimetest.Runtime
	agent *Agent
	errc  chan error
	stop  context.CancelFunc
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	cp := cptest.New(t, nil)
	dir, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	id := &state.Identity{DeviceID: "0190a000-0000-7000-8000-000000000001", OrganizationID: "org", Origin: cp.URL, PrivateKey: priv}
	if err := dir.SaveIdentity(id); err != nil {
		t.Fatal(err)
	}
	cp.AddDevice(id.DeviceID, priv.Public().(ed25519.PublicKey))
	client, err := controlplane.New(cp.URL)
	if err != nil {
		t.Fatal(err)
	}
	rt := runtimetest.New(t.TempDir())
	a, err := New(context.Background(), Config{
		Dir: dir, Identity: id, Version: "1.0.0", Runtime: rt, Client: client,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Install: func(string) error { return errors.New("not in tests") },
		ChannelTiming: &channel.Config{
			HeartbeatInterval: 50 * time.Millisecond,
			LossTimeout:       400 * time.Millisecond,
			MinBackoff:        10 * time.Millisecond,
			MaxBackoff:        50 * time.Millisecond,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &harness{t: t, cp: cp, dir: dir, rt: rt, agent: a, errc: make(chan error, 1), stop: cancel}
	go func() { h.errc <- a.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-h.errc:
		case <-time.After(10 * time.Second):
			t.Error("agent did not stop")
		}
	})
	return h
}

// expect waits for an envelope that matches.
func expect(t *testing.T, ch *cptest.Channel, match func(*ownsandv1.AgentEnvelope) bool) *ownsandv1.AgentEnvelope {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case env := <-ch.Received:
			if match(env) {
				return env
			}
		case <-timeout:
			t.Fatal("expected envelope did not arrive")
			return nil
		}
	}
}

func TestChannelReportsAndFreshCredentialPerChannel(t *testing.T) {
	h := newHarness(t)
	ch := h.cp.NextChannel(10 * time.Second)
	expect(t, ch, func(e *ownsandv1.AgentEnvelope) bool {
		c := e.GetCapabilityReport()
		return c != nil && c.GetAgentVersion() == "1.0.0" && len(c.GetGuestOses()) == 1
	})
	rep := expect(t, ch, func(e *ownsandv1.AgentEnvelope) bool { return e.GetStateReport() != nil }).GetStateReport()
	if rep.GetAgentVersion() != "1.0.0" || rep.GetUpdateState() != ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_UP_TO_DATE {
		t.Fatalf("unexpected state report %v", rep)
	}
	first := ch.Credential
	ch.Close()
	ch2 := h.cp.NextChannel(10 * time.Second)
	if ch2.Credential == first || !strings.HasPrefix(ch2.Credential, "osd_") {
		t.Fatal("the agent reused a device session credential for a new channel")
	}
	if auth, _ := h.cp.Counts(); auth < 2 {
		t.Fatalf("authenticate count %d", auth)
	}
}

func TestHeartbeatLossReconnects(t *testing.T) {
	h := newHarness(t)
	ch := h.cp.NextChannel(10 * time.Second)
	h.cp.SetAckHeartbeats(false)
	ch2 := h.cp.NextChannel(10 * time.Second)
	if ch2.Generation == ch.Generation {
		t.Fatal("no new channel after the loss timeout")
	}
}

func TestCreateCommandOverChannel(t *testing.T) {
	h := newHarness(t)
	ch := h.cp.NextChannel(10 * time.Second)
	ch.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_Configuration{Configuration: &ownsandv1.AgentConfiguration{
		Limits: &ownsandv1.DeviceLimits{VcpuCount: 4, MemoryMib: 4096, DiskGib: 50},
	}}})
	sid := datapath.NewID()
	now := time.Now()
	ch.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_Command{Command: &ownsandv1.AgentCommand{
		CommandId: "cmd-1",
		Command: &ownsandv1.AgentCommand_CreateSandbox{CreateSandbox: &ownsandv1.CreateSandboxCommand{
			SandboxId:    sid,
			Source:       &ownsandv1.SandboxSource{Source: &ownsandv1.SandboxSource_BaseImage{BaseImage: ownsandv1.BaseImage_BASE_IMAGE_LINUX}},
			Resources:    &ownsandv1.Resources{VcpuCount: 1, MemoryMib: 512, DiskGib: 1},
			IdleDeadline: timestamppb.New(now.Add(time.Minute)),
			MaxDeadline:  timestamppb.New(now.Add(time.Hour)),
			IdleTtl:      durationpb.New(time.Minute),
			Limits:       &ownsandv1.DeviceLimits{VcpuCount: 4, MemoryMib: 4096, DiskGib: 50},
		}},
	}}})
	upd := expect(t, ch, func(e *ownsandv1.AgentEnvelope) bool {
		return e.GetSandboxUpdate().GetSandbox().GetState() == ownsandv1.SandboxState_SANDBOX_STATE_RUNNING
	}).GetSandboxUpdate().GetSandbox()
	res := expect(t, ch, func(e *ownsandv1.AgentEnvelope) bool { return e.GetCommandResult().GetCommandId() == "cmd-1" }).GetCommandResult()
	if res.GetErrorCode() != "" || res.GetSandbox().GetState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		t.Fatalf("create result %v", res)
	}
	ch.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_StateAck{StateAck: &ownsandv1.StateAck{
		Target: &ownsandv1.StateAck_Sandbox{Sandbox: &ownsandv1.SandboxAck{SandboxId: sid, Revision: upd.GetRevision()}},
	}}})
	deadline := time.Now().Add(5 * time.Second)
	for len(h.agent.Mgr.PendingUpdates()) > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(h.agent.Mgr.PendingUpdates()) > 0 {
		t.Fatal("acknowledged update still pending")
	}
	// Revocations are acknowledged even without a data path.
	ch.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_SessionRevocation{SessionRevocation: &ownsandv1.SessionRevocation{
		RevocationId: "rev-1", Target: &ownsandv1.SessionRevocation_SessionId{SessionId: "s"},
	}}})
	expect(t, ch, func(e *ownsandv1.AgentEnvelope) bool { return e.GetRevocationAck().GetRevocationId() == "rev-1" })
	// Offers are answered with unimplemented in this layer.
	ch.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_SignalingOffer{SignalingOffer: &ownsandv1.SignalingOffer{SessionId: "s1", SandboxId: sid}}})
	ans := expect(t, ch, func(e *ownsandv1.AgentEnvelope) bool { return e.GetSignalingAnswer() != nil }).GetSignalingAnswer()
	if ans.GetErrorCode() != "unimplemented" {
		t.Fatalf("offer answer %v", ans)
	}
}

func TestRemovalNoticeCleansUpAndConfirms(t *testing.T) {
	h := newHarness(t)
	ch := h.cp.NextChannel(10 * time.Second)
	h.cp.SetState(cptest.DeviceRemoved)
	ch.Close()
	select {
	case err := <-h.errc:
		if !errors.Is(err, ErrRemoved) {
			t.Fatalf("run ended with %v", err)
		}
		h.errc <- err
	case <-time.After(10 * time.Second):
		t.Fatal("agent did not stop after removal")
	}
	if _, confirm := h.cp.Counts(); confirm != 1 {
		t.Fatalf("confirm count %d", confirm)
	}
	if _, err := h.dir.LoadIdentity(); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("device key not deleted: %v", err)
	}
	if !contains(h.rt.CallLog(), "delete-all") {
		t.Fatal("local sandboxes were not deleted")
	}
}

func TestUnknownDeviceKeepsData(t *testing.T) {
	h := newHarness(t)
	h.cp.NextChannel(10 * time.Second)
	h.cp.SetState(cptest.DeviceUnknown)
	h.cp.SetState(cptest.DeviceUnknown)
	// Force a reconnect.
	h.cp.SetAckHeartbeats(false)
	time.Sleep(time.Second)
	if _, err := h.dir.LoadIdentity(); err != nil {
		t.Fatalf("identity deleted for an unknown device: %v", err)
	}
	if contains(h.rt.CallLog(), "delete-all") {
		t.Fatal("local data deleted for an unknown device")
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
