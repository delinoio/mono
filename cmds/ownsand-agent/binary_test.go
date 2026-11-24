// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build linux || darwin

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/pion/webrtc/v4"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/testcp"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime/processrt"
	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// logBuffer captures subprocess output. exec.Cmd copies a non-file Stderr
// from its own goroutine until Wait returns, so a plain strings.Builder
// races with any snapshot taken before Wait; every access holds one mutex.
type logBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

const logCaptureHelperEnv = "OWNSAND_AGENT_TEST_LOG_HELPER"

// TestLogCaptureHelperProcess is not a real test: TestLogCapture re-executes
// the test binary to run it as a subprocess that writes to stderr.
func TestLogCaptureHelperProcess(t *testing.T) {
	if os.Getenv(logCaptureHelperEnv) != "1" {
		t.Skip("helper subprocess only")
	}
	for i := 0; i < 2000; i++ {
		os.Stderr.WriteString("helper log line\n")
		time.Sleep(50 * time.Microsecond)
	}
	os.Stderr.WriteString("helper done\n")
}

// TestLogCapture takes log snapshots while a subprocess is still writing
// stderr, as the failure paths of TestBinaryWithProcessRuntime do. It must
// not report a race under go test -race.
func TestLogCapture(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestLogCaptureHelperProcess$")
	cmd.Env = append(os.Environ(), logCaptureHelperEnv+"=1")
	logs := &logBuffer{}
	cmd.Stderr = logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	deadline := time.After(30 * time.Second)
	for finished := false; !finished; {
		_ = logs.String()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("helper exit: %v\n%s", err, logs.String())
			}
			finished = true
		case <-deadline:
			cmd.Process.Kill()
			t.Fatalf("helper did not finish\n%s", logs.String())
		default:
		}
	}
	if !strings.Contains(logs.String(), "helper done") {
		t.Fatalf("captured logs miss the final line\n%s", logs.String())
	}
}

const binaryAgentTimeout = 20 * time.Second

func buildProcessRuntimeBinaries(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("builds binaries")
	}
	bin := t.TempDir()
	for _, pkg := range []string{"ownsand-agent", "ownsand-guest"} {
		if out, err := exec.Command("go", "build", "-o", filepath.Join(bin, pkg), "github.com/delinoio/mono/cmds/"+pkg).CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", pkg, err, out)
		}
	}
	return bin
}

func processRuntimeEnv(stateDir, guestBinary string) []string {
	// Inherited OWNSAND_AGENT_* variables could fail configuration
	// validation or replace the enrolled control plane URL.
	return append(withoutAgentEnv(os.Environ()),
		"OWNSAND_AGENT_STATE_DIR="+stateDir,
		"OWNSAND_AGENT_RUNTIME=process",
		"OWNSAND_AGENT_GUEST_BINARY="+guestBinary,
		"NO_COLOR=1",
	)
}

func enrollProcessRuntimeAgent(t *testing.T, bin string, cp *testcp.Server, env []string) {
	t.Helper()
	enroll := exec.Command(filepath.Join(bin, "ownsand-agent"), "enroll", "--api-url", cp.URL)
	enroll.Env = env
	enroll.Stdin = strings.NewReader(testcp.Token)
	if out, err := enroll.CombinedOutput(); err != nil {
		t.Fatalf("enroll: %v\n%s", err, out)
	}
}

type processRuntimeAgent struct {
	cmd    *exec.Cmd
	logs   *logBuffer
	exited chan struct{}
	err    error // Set before exited is closed.
}

func startProcessRuntimeAgent(t *testing.T, bin string, env []string) *processRuntimeAgent {
	t.Helper()
	run := exec.Command(filepath.Join(bin, "ownsand-agent"), "run")
	run.Env = env
	agent := &processRuntimeAgent{cmd: run, logs: &logBuffer{}, exited: make(chan struct{})}
	run.Stderr = agent.logs
	if err := run.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		agent.err = run.Wait()
		close(agent.exited)
	}()
	t.Cleanup(func() {
		_ = run.Process.Kill()
		select {
		case <-agent.exited:
		case <-time.After(binaryAgentTimeout):
			t.Error("agent child wait did not finish after kill")
		}
	})
	return agent
}

func (a *processRuntimeAgent) wait(t *testing.T, deadline time.Time) error {
	t.Helper()
	select {
	case <-a.exited:
		return a.err
	case <-time.After(time.Until(deadline)):
		t.Fatalf("agent wait with captured stderr did not finish\n%s", a.logs.String())
		return nil
	}
}

func stopBinaryAgent(t *testing.T, agent *processRuntimeAgent) {
	t.Helper()
	if err := agent.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := agent.wait(t, time.Now().Add(binaryAgentTimeout)); err != nil {
		t.Fatalf("agent exit after SIGTERM: %v\n%s", err, agent.logs.String())
	}
}

func (a *processRuntimeAgent) waitForReport(t *testing.T, cp *testcp.Server, from int) *ownsandv1.StateReport {
	t.Helper()
	env, _ := cp.WaitFor(binaryAgentTimeout, from, func(e *ownsandv1.AgentEnvelope) bool { return e.GetStateReport() != nil })
	if env == nil {
		t.Fatalf("no state report\n%s", a.logs.String())
	}
	return env.GetStateReport()
}

func sendProcessRuntimeCreate(t *testing.T, cp *testcp.Server, id string) string {
	t.Helper()
	cmdID, _ := uuidv7.NewString()
	now := time.Now()
	// The process runtime runs only guests of the host OS.
	guestOS, baseImage := processrt.HostGuest()
	if !cp.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_Command{Command: &ownsandv1.AgentCommand{CommandId: cmdID, Command: &ownsandv1.AgentCommand_CreateSandbox{CreateSandbox: &ownsandv1.CreateSandboxCommand{
		SandboxId: id, Source: &ownsandv1.CreateSandboxCommand_BaseImage{BaseImage: baseImage},
		Resources: &ownsandv1.Resources{VcpuCount: 1, MemoryMib: 128, DiskGib: 1}, NetworkMode: ownsandv1.NetworkMode_NETWORK_MODE_NONE,
		GuestOs: guestOS, IdleTtl: durationpb.New(time.Hour), MaxTtl: durationpb.New(time.Hour),
		CreateTime: timestamppb.New(now), MaxDeadline: timestamppb.New(now.Add(time.Hour)),
		Limits: &ownsandv1.DeviceLimits{VcpuCount: 2, MemoryMib: 1024, DiskGib: 2, VcpuOvercommitPercent: 100},
	}}}}}) {
		t.Fatal("send create command: agent channel is closed")
	}
	return cmdID
}

func processRuntimeSocketDir(t *testing.T, sandboxDir string) string {
	t.Helper()
	record, err := os.ReadFile(filepath.Join(sandboxDir, "socket-dir"))
	if err != nil {
		t.Fatal(err)
	}
	dir := string(record)
	if !filepath.IsAbs(dir) || !strings.HasPrefix(filepath.Base(dir), "osb-") {
		t.Fatalf("unexpected recorded socket directory %q", dir)
	}
	return dir
}

func requireProcessRuntimePathsRemoved(t *testing.T, paths ...string) {
	t.Helper()
	for _, path := range paths {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s: want removed, got stat error %v", path, err)
		}
	}
}

// TestBinaryWithProcessRuntime runs the built agent and guest helper
// binaries with the process test runtime, configured only through
// environment variables, against the fake control plane.
func TestBinaryWithProcessRuntime(t *testing.T) {
	bin := buildProcessRuntimeBinaries(t)
	cp := testcp.New(t)
	stateDir := t.TempDir()
	env := processRuntimeEnv(stateDir, filepath.Join(bin, "ownsand-guest"))
	enrollProcessRuntimeAgent(t, bin, cp, env)
	agent := startProcessRuntimeAgent(t, bin, env)
	agent.waitForReport(t, cp, 0)
	logs := agent.logs

	id, _ := uuidv7.NewString()
	cmdID := sendProcessRuntimeCreate(t, cp, id)
	res := cp.CommandResult(30*time.Second, cmdID)
	if res.GetCreateSandbox().GetSandbox().GetState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		t.Fatalf("create result %v\n%s", res, logs.String())
	}
	sandboxDir := filepath.Join(stateDir, "vms", id)
	paths := []string{sandboxDir}
	if runtime.GOOS == "darwin" {
		paths = append(paths, processRuntimeSocketDir(t, sandboxDir))
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
	offerSDP, err := createLocalOffer(pionOfferPeer{pc}, 20*time.Second)
	if err != nil {
		t.Fatalf("local offer: %v", err)
	}
	sessionID, _ := uuidv7.NewString()
	cp.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_Signaling{Signaling: &ownsandv1.SignalingMessage{Message: &ownsandv1.SignalingMessage_Offer{Offer: &ownsandv1.SignalingOffer{
		SessionId: sessionID, SandboxId: id, ExpiresIn: durationpb.New(time.Hour), Sdp: offerSDP,
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

	stopBinaryAgent(t, agent)
	requireProcessRuntimePathsRemoved(t, paths...)
	for _, secret := range []string{testcp.Token, "osd_"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("logs contain a secret prefix %q", secret)
		}
	}
}
