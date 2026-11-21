// SPDX-License-Identifier: Apache-2.0

//go:build linux || darwin

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/pion/webrtc/v4"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/testcp"
	"github.com/delinoio/mono/go/ownsand/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// TestBinaryWithProcessRuntime runs the built agent and guest helper
// binaries with the process test runtime, configured only through
// environment variables, against the fake control plane.
func TestBinaryWithProcessRuntime(t *testing.T) {
	if testing.Short() {
		t.Skip("builds binaries")
	}
	bin := t.TempDir()
	for _, pkg := range []string{"ownsand-agent", "ownsand-guest"} {
		if out, err := exec.Command("go", "build", "-o", filepath.Join(bin, pkg), "github.com/delinoio/mono/cmds/"+pkg).CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", pkg, err, out)
		}
	}
	cp := testcp.New(t)
	env := append(os.Environ(),
		"OWNSAND_AGENT_STATE_DIR="+t.TempDir(),
		"OWNSAND_AGENT_RUNTIME=process",
		"OWNSAND_AGENT_GUEST_BINARY="+filepath.Join(bin, "ownsand-guest"),
		"NO_COLOR=1",
	)
	enroll := exec.Command(filepath.Join(bin, "ownsand-agent"), "enroll", "--api-url", cp.URL)
	enroll.Env = env
	enroll.Stdin = strings.NewReader(testcp.Token)
	if out, err := enroll.CombinedOutput(); err != nil {
		t.Fatalf("enroll: %v\n%s", err, out)
	}
	run := exec.Command(filepath.Join(bin, "ownsand-agent"), "run")
	run.Env = env
	var logs strings.Builder
	run.Stderr = &logs
	if err := run.Start(); err != nil {
		t.Fatal(err)
	}
	defer run.Process.Kill()
	if env, _ := cp.WaitFor(20*time.Second, 0, func(e *ownsandv1.AgentEnvelope) bool { return e.GetStateReport() != nil }); env == nil {
		t.Fatalf("no state report\n%s", logs.String())
	}
	id, _ := uuidv7.NewString()
	cmdID, _ := uuidv7.NewString()
	now := time.Now()
	cp.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_Command{Command: &ownsandv1.AgentCommand{CommandId: cmdID, Command: &ownsandv1.AgentCommand_CreateSandbox{CreateSandbox: &ownsandv1.CreateSandboxCommand{
		SandboxId: id, Source: &ownsandv1.CreateSandboxCommand_BaseImage{BaseImage: ownsandv1.BaseImage_BASE_IMAGE_LINUX},
		Resources: &ownsandv1.Resources{VcpuCount: 1, MemoryMib: 128, DiskGib: 1}, NetworkMode: ownsandv1.NetworkMode_NETWORK_MODE_NONE,
		GuestOs: ownsandv1.GuestOs_GUEST_OS_LINUX, IdleTtl: durationpb.New(time.Hour), MaxTtl: durationpb.New(time.Hour),
		CreateTime: timestamppb.New(now), MaxDeadline: timestamppb.New(now.Add(time.Hour)),
		Limits: &ownsandv1.DeviceLimits{VcpuCount: 2, MemoryMib: 1024, DiskGib: 2, VcpuOvercommitPercent: 100},
	}}}}})
	res := cp.CommandResult(30*time.Second, cmdID)
	if res.GetCreateSandbox().GetSandbox().GetState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		t.Fatalf("create result %v\n%s", res, logs.String())
	}
	// The data path is wired: an offer gets an SDP answer, and a relay
	// grant is acknowledged and opens a relay connection.
	relayOpened := make(chan string, 4)
	cp.SetRelayHandler(func(ctx context.Context, deviceID string, stream *connect.BidiStream[ownsandv1.ConnectRequest, ownsandv1.ConnectResponse]) error {
		req, err := stream.Receive()
		if err == nil && req.GetFrame().GetKind() == ownsandv1.DataFrameKind_DATA_FRAME_KIND_RELAY_KEEPALIVE {
			relayOpened <- deviceID
		}
		<-ctx.Done()
		return nil
	})
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	if _, err := pc.CreateDataChannel("ownsand-data", nil); err != nil {
		t.Fatal(err)
	}
	offer, _ := pc.CreateOffer(nil)
	gathered := webrtc.GatheringCompletePromise(pc)
	pc.SetLocalDescription(offer)
	<-gathered
	sessionID, _ := uuidv7.NewString()
	cp.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_Signaling{Signaling: &ownsandv1.SignalingMessage{Message: &ownsandv1.SignalingMessage_Offer{Offer: &ownsandv1.SignalingOffer{
		SessionId: sessionID, SandboxId: id, ExpiresIn: durationpb.New(time.Hour), Sdp: pc.LocalDescription().SDP,
	}}}}})
	answer, _ := cp.WaitFor(20*time.Second, 0, func(e *ownsandv1.AgentEnvelope) bool {
		return e.GetSignaling().GetAnswer().GetSessionId() == sessionID
	})
	if answer.GetSignaling().GetAnswer().GetSdp() == "" {
		t.Fatalf("signaling answer %v\n%s", answer, logs.String())
	}
	grantID, _ := uuidv7.NewString()
	execID, _ := uuidv7.NewString()
	cp.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_RelayGrant{RelayGrant: &ownsandv1.RelayGrant{
		GrantId: grantID, SandboxId: id, OperationKind: ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_ATTACH_EXEC, OperationId: execID,
		ExpiresIn: durationpb.New(15 * time.Minute), StreamDeadlineIn: durationpb.New(30 * time.Minute),
	}}})
	ack, _ := cp.WaitFor(20*time.Second, 0, func(e *ownsandv1.AgentEnvelope) bool { return e.GetRelayGrantAck().GetGrantId() == grantID })
	if ack.GetRelayGrantAck().GetResult() != ownsandv1.RelayGrantResult_RELAY_GRANT_RESULT_ACCEPTED {
		t.Fatalf("relay grant ack %v", ack)
	}
	select {
	case <-relayOpened:
	case <-time.After(20 * time.Second):
		t.Fatalf("no relay connection\n%s", logs.String())
	}

	run.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- run.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("agent exit: %v\n%s", err, logs.String())
		}
	case <-time.After(20 * time.Second):
		t.Fatal("agent did not stop on SIGTERM")
	}
	for _, secret := range []string{testcp.Token, "osd_"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("logs contain a secret prefix %q", secret)
		}
	}
}
