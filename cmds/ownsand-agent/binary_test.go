// SPDX-License-Identifier: Apache-2.0

//go:build linux || darwin

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/testcp"
	"github.com/delinoio/mono/packages/ownsand-sdk-go/uuidv7"
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
