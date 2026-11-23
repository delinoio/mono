// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build linux || darwin

package agent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/cpclient"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/sandboxes"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/testcp"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/update"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime/processrt"
	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

var guestBinary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "ownsand-agent-test-")
	if err != nil {
		panic(err)
	}
	guestBinary = filepath.Join(dir, "ownsand-guest")
	if out, err := exec.Command("go", "build", "-o", guestBinary, "github.com/delinoio/mono/cmds/ownsand-guest").CombinedOutput(); err != nil {
		panic(fmt.Sprintf("build guest helper: %v\n%s", err, out))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type harness struct {
	t     *testing.T
	cp    *testcp.Server
	st    *store.Store
	id    *store.Identity
	agent *Agent
	errc  chan error
	stop  context.CancelFunc
}

func quietLogger() *slog.Logger {
	if os.Getenv("OWNSAND_TEST_VERBOSE") != "" {
		return slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func enrollDevice(t *testing.T, cp *testcp.Server) (*store.Store, *store.Identity) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	client, err := cpclient.New(cp.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Enroll(context.Background(), st, client, "ose_wrong", "host", quietLogger()); err == nil {
		t.Fatal("wrong token enrolled")
	}
	id, err := Enroll(context.Background(), st, client, testcp.Token, "host", quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Enroll(context.Background(), st, client, testcp.Token, "host", quietLogger()); !errors.Is(err, ErrAlreadyEnrolled) {
		t.Fatalf("second enroll: %v", err)
	}
	return st, id
}

func start(t *testing.T, cp *testcp.Server, st *store.Store, id *store.Identity) *harness {
	t.Helper()
	return startWith(t, cp, st, id, nil)
}

// startWith starts the agent like start, after tweak changed its options.
func startWith(t *testing.T, cp *testcp.Server, st *store.Store, id *store.Identity, tweak func(*Options)) *harness {
	t.Helper()
	a := newAgent(t, cp, st, id, tweak)
	ctx, cancel := context.WithCancel(context.Background())
	h := &harness{t: t, cp: cp, st: st, id: id, agent: a, errc: make(chan error, 1), stop: cancel}
	go func() { h.errc <- a.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-h.errc:
		case <-time.After(15 * time.Second):
			t.Error("agent did not stop")
		}
	})
	return h
}

// newAgent builds the agent like start, after tweak changed its options,
// and does not run it.
func newAgent(t *testing.T, cp *testcp.Server, st *store.Store, id *store.Identity, tweak func(*Options)) *Agent {
	t.Helper()
	logger := quietLogger()
	rt, err := processrt.New(guestBinary, logger)
	if err != nil {
		t.Fatal(err)
	}
	client, _ := cpclient.New(cp.URL, nil)
	opts := Options{
		Store: st, Identity: id, Control: client, Runtime: rt, Logger: logger,
		HeartbeatInterval: 50 * time.Millisecond, AckTimeout: 500 * time.Millisecond,
		BackoffMin: 20 * time.Millisecond, BackoffMax: 100 * time.Millisecond,
		Update: update.Options{Platform: "linux-amd64", Running: "1.0.0", Installer: update.ExecutableInstaller{Path: filepath.Join(t.TempDir(), "exe")}},
	}
	if tweak != nil {
		tweak(&opts)
	}
	a, err := New(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func (h *harness) wait(from int, what string, match func(*ownsandv1.AgentEnvelope) bool) (*ownsandv1.AgentEnvelope, int) {
	h.t.Helper()
	env, i := h.cp.WaitFor(10*time.Second, from, match)
	if env == nil {
		h.t.Fatalf("no %s from the agent", what)
	}
	return env, i
}

func (h *harness) command(cmd *ownsandv1.AgentCommand) *ownsandv1.AgentCommandResult {
	h.t.Helper()
	cmd.CommandId, _ = uuidv7.NewString()
	if !h.cp.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_Command{Command: cmd}}) {
		h.t.Fatal("no channel")
	}
	res := h.cp.CommandResult(30*time.Second, cmd.GetCommandId())
	if res == nil {
		h.t.Fatal("no command result")
	}
	return res
}

var limits = &ownsandv1.DeviceLimits{VcpuCount: 4, MemoryMib: 4096, DiskGib: 4, VcpuOvercommitPercent: 100}

// createCommand creates a sandbox of the guest OS that the process runtime
// runs on this host.
func createCommand(id string) *ownsandv1.AgentCommand {
	now := time.Now()
	guestOS, baseImage := processrt.HostGuest()
	return &ownsandv1.AgentCommand{Command: &ownsandv1.AgentCommand_CreateSandbox{CreateSandbox: &ownsandv1.CreateSandboxCommand{
		SandboxId:   id,
		Source:      &ownsandv1.CreateSandboxCommand_BaseImage{BaseImage: baseImage},
		Resources:   &ownsandv1.Resources{VcpuCount: 1, MemoryMib: 256, DiskGib: 1},
		NetworkMode: ownsandv1.NetworkMode_NETWORK_MODE_NONE,
		GuestOs:     guestOS,
		IdleTtl:     durationpb.New(time.Hour),
		MaxTtl:      durationpb.New(time.Hour),
		CreateTime:  timestamppb.New(now),
		MaxDeadline: timestamppb.New(now.Add(time.Hour)),
		Limits:      limits,
	}}}
}

func TestChannelLifecycleCommandsAndRemoval(t *testing.T) {
	cp := testcp.New(t)
	st, id := enrollDevice(t, cp)
	h := start(t, cp, st, id)

	capEnv, _ := h.wait(0, "capability report", func(e *ownsandv1.AgentEnvelope) bool { return e.GetCapabilityReport() != nil })
	caps := capEnv.GetCapabilityReport()
	if caps.GetHostOs() == 0 || caps.GetCapabilities().GetLogicalCpuCount() < 1 || len(caps.GetCapabilities().GetGuestOses()) == 0 {
		t.Fatalf("capability report %v", caps)
	}
	report, _ := h.wait(0, "state report", func(e *ownsandv1.AgentEnvelope) bool { return e.GetStateReport() != nil })
	if report.GetStateReport().GetStateEpoch() == "" {
		t.Fatal("state report without epoch")
	}
	h.wait(0, "heartbeat", func(e *ownsandv1.AgentEnvelope) bool { return e.GetHeartbeat().GetSequence() > 2 })
	cp.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_Configuration{Configuration: &ownsandv1.AgentConfiguration{StunUrls: []string{"stun:127.0.0.1:3478"}, Limits: limits}}})

	sandboxID, _ := uuidv7.NewString()
	res := h.command(createCommand(sandboxID))
	if res.GetError() != nil || res.GetCreateSandbox().GetSandbox().GetState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		t.Fatalf("create result %v", res)
	}
	h.wait(0, "running update", func(e *ownsandv1.AgentEnvelope) bool {
		return e.GetSandboxUpdate().GetSandbox().GetState() == ownsandv1.SandboxState_SANDBOX_STATE_RUNNING
	})
	res = h.command(&ownsandv1.AgentCommand{Command: &ownsandv1.AgentCommand_PauseSandbox{PauseSandbox: &ownsandv1.PauseSandboxCommand{SandboxId: sandboxID}}})
	if res.GetSandbox().GetSandbox().GetState() != ownsandv1.SandboxState_SANDBOX_STATE_PAUSED {
		t.Fatalf("pause result %v", res)
	}
	res = h.command(&ownsandv1.AgentCommand{Command: &ownsandv1.AgentCommand_ResumeSandbox{ResumeSandbox: &ownsandv1.ResumeSandboxCommand{SandboxId: sandboxID}}})
	if res.GetSandbox().GetSandbox().GetState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		t.Fatalf("resume result %v", res)
	}
	snapID, _ := uuidv7.NewString()
	res = h.command(&ownsandv1.AgentCommand{Command: &ownsandv1.AgentCommand_CreateSnapshot{CreateSnapshot: &ownsandv1.CreateSnapshotCommand{SnapshotId: snapID, SandboxId: sandboxID}}})
	if res.GetCreateSnapshot().GetSnapshot().GetState() != ownsandv1.SnapshotState_SNAPSHOT_STATE_READY {
		t.Fatalf("snapshot result %v", res)
	}
	// Revocations are acknowledged also for unknown sessions.
	cp.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_SessionRevocation{SessionRevocation: &ownsandv1.SessionRevocation{RevocationId: "r1", Target: &ownsandv1.SessionRevocation_SessionId{SessionId: "s"}}}})
	h.wait(0, "revocation ack", func(e *ownsandv1.AgentEnvelope) bool { return e.GetRevocationAck().GetRevocationId() == "r1" })

	// Without heartbeat acknowledgements the agent reconnects and sends a
	// new state report. The agent sends a state report only when a channel
	// opens, so every report at or after this cursor belongs to the
	// reconnect. Sample the cursor before the trigger: sampling it after the
	// reconnect can skip past the new report when a heartbeat follows it.
	channels := cp.Channels()
	cursor := cp.Received()
	cp.SetAck(false, true)
	deadline := time.Now().Add(10 * time.Second)
	for cp.Channels() == channels {
		if time.Now().After(deadline) {
			t.Fatal("agent did not reconnect without heartbeat acknowledgements")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cp.SetAck(true, true)
	h.wait(cursor, "state report after reconnect", func(e *ownsandv1.AgentEnvelope) bool { return e.GetStateReport() != nil })

	res = h.command(&ownsandv1.AgentCommand{Command: &ownsandv1.AgentCommand_DeleteSandbox{DeleteSandbox: &ownsandv1.DeleteSandboxCommand{SandboxId: sandboxID}}})
	if res.GetSandbox().GetSandbox().GetState() != ownsandv1.SandboxState_SANDBOX_STATE_DELETED {
		t.Fatalf("delete result %v", res)
	}

	// Removal: the control plane marks the device removed and sends the
	// clean-up command.
	cp.MarkRemoved(id.DeviceID)
	cmdID, _ := uuidv7.NewString()
	cp.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_Command{Command: &ownsandv1.AgentCommand{CommandId: cmdID, Command: &ownsandv1.AgentCommand_CleanUp{CleanUp: &ownsandv1.CleanUpCommand{}}}}})
	select {
	case err := <-h.errc:
		if !errors.Is(err, ErrRemoved) {
			t.Fatalf("run returned %v", err)
		}
		h.errc <- err
	case <-time.After(20 * time.Second):
		t.Fatal("agent did not finish the removal")
	}
	if cp.Confirms() != 1 {
		t.Fatal("removal not confirmed")
	}
	if _, err := st.LoadIdentity(); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("device key kept after removal")
	}
	requireEmptyStateDir(t, st)
}

// requireEmptyStateDir fails when a finished removal left any entry in the
// state directory, for example a status.json that a status write created
// after the delete.
func requireEmptyStateDir(t *testing.T, st *store.Store) {
	t.Helper()
	entries, err := os.ReadDir(st.Path())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Errorf("state directory keeps %q after the removal", e.Name())
	}
}

// TestRemovalFencesInFlightStatusWrite pauses a status write inside the
// writer, starts the removal and releases the write: the removal waits for
// the write, deletes the state directory afterwards, and no status write
// (statusLoop or setConnected) re-creates a file.
func TestRemovalFencesInFlightStatusWrite(t *testing.T) {
	cp := testcp.New(t)
	st, id := enrollDevice(t, cp)
	a := newAgent(t, cp, st, id, nil)
	t.Cleanup(a.shutdown)

	// Normal renewal is unchanged: the writer creates the status file.
	a.writeStatus()
	var status Status
	if err := st.LoadStatus(&status); err != nil || status.DeviceID != id.DeviceID {
		t.Fatalf("status file before the removal: %+v, %v", status, err)
	}

	cp.MarkRemoved(id.DeviceID)
	entered := make(chan struct{})
	release := make(chan struct{})
	a.statusWriteHook = func() {
		close(entered)
		<-release
	}
	written := make(chan struct{})
	go func() {
		defer close(written)
		a.writeStatus()
	}()
	<-entered

	removed := make(chan error, 1)
	go func() { removed <- a.removeDevice(context.Background()) }()
	select {
	case err := <-removed:
		t.Fatalf("removal finished while a status write was in flight: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-removed:
		if !errors.Is(err, ErrRemoved) {
			t.Fatalf("removal returned %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("removal did not finish")
	}
	<-written
	requireEmptyStateDir(t, st)

	// The writer stays fenced: later writes do nothing.
	a.writeStatus()
	a.setConnected(true)
	a.setConnected(false)
	requireEmptyStateDir(t, st)
}

// TestStatusRecordsCapabilities checks that the status file holds the
// capabilities of the capability report that the agent sent, so that
// `ownsand-agent status` can print the hypervisor checks.
func TestStatusRecordsCapabilities(t *testing.T) {
	cp := testcp.New(t)
	st, id := enrollDevice(t, cp)
	h := start(t, cp, st, id)
	env, _ := h.wait(0, "capability report", func(e *ownsandv1.AgentEnvelope) bool { return e.GetCapabilityReport() != nil })
	caps := env.GetCapabilityReport().GetCapabilities()
	var status Status
	deadline := time.Now().Add(10 * time.Second)
	for {
		status = Status{}
		if err := st.LoadStatus(&status); err == nil && !status.CapabilitiesAt.IsZero() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the status file has no capabilities: %+v", status)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(status.Checks) == 0 || len(status.Checks) != len(caps.GetHypervisorChecks()) {
		t.Fatalf("status checks %+v, report %v", status.Checks, caps.GetHypervisorChecks())
	}
	for i, ch := range caps.GetHypervisorChecks() {
		if got := status.Checks[i]; got.ID != ch.GetCheckId() || got.Passed != ch.GetPassed() || got.Detail != ch.GetDetail() {
			t.Errorf("status check %d = %+v, want %v", i, got, ch)
		}
	}
	if len(status.GuestOSes) != len(caps.GetGuestOses()) || status.GuestOSes[0] != caps.GetGuestOses()[0].String() {
		t.Errorf("status guest OSes %v, report %v", status.GuestOSes, caps.GetGuestOses())
	}
	if len(status.BaseImages) != len(caps.GetBuiltBaseImages()) {
		t.Errorf("status base images %v, report %v", status.BaseImages, caps.GetBuiltBaseImages())
	}
}

// TestFailedRemovalResumesStatusWrites checks that a removal that fails
// keeps the agent enrolled and lifts the status fence, so the status file
// renews while the removal retries.
func TestFailedRemovalResumesStatusWrites(t *testing.T) {
	cp := testcp.New(t)
	st, id := enrollDevice(t, cp)
	a := newAgent(t, cp, st, id, nil)
	t.Cleanup(a.shutdown)

	// A canceled context makes the removal fail before it deletes the
	// device key.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.removeDevice(ctx); err == nil || errors.Is(err, ErrRemoved) {
		t.Fatalf("removal with a canceled context returned %v", err)
	}
	if _, err := st.LoadIdentity(); err != nil {
		t.Fatalf("device key after a failed removal: %v", err)
	}
	a.writeStatus()
	var status Status
	if err := st.LoadStatus(&status); err != nil || status.DeviceID != id.DeviceID {
		t.Fatalf("status file after a failed removal: %+v, %v", status, err)
	}
}

func TestRemovalNoticeAtAuthenticate(t *testing.T) {
	cp := testcp.New(t)
	st, id := enrollDevice(t, cp)
	cp.MarkRemoved(id.DeviceID)
	h := start(t, cp, st, id)
	select {
	case err := <-h.errc:
		if !errors.Is(err, ErrRemoved) {
			t.Fatalf("run returned %v", err)
		}
		h.errc <- err
	case <-time.After(20 * time.Second):
		t.Fatal("agent did not finish the removal")
	}
	if cp.Confirms() != 1 {
		t.Fatal("removal not confirmed")
	}
	requireEmptyStateDir(t, st)
}

// hostCleanerRuntime is a runtime with host resources outside the state
// directory, like the Linux Firecracker runtime: it records the CleanupHost
// calls and the sandboxes that still ran at each call.
type hostCleanerRuntime struct {
	vmruntime.Runtime
	mu      sync.Mutex
	calls   int
	running []string
	err     error
}

func (r *hostCleanerRuntime) CleanupHost() error {
	ids, _ := r.Runtime.Running(context.Background())
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.running = append(r.running, ids...)
	return r.err
}

// TestRemovalCleansUpHostNetwork checks that the device removal deletes the
// host network resources of the runtime after it deleted every sandbox: a
// removed agent exits for good and never runs the clean-up of the next
// start. A failed host clean-up is logged and does not stop the removal.
func TestRemovalCleansUpHostNetwork(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{{"success", nil}, {"failure", errors.New("delete nftables table inet ownsand: simulated")}} {
		t.Run(tc.name, func(t *testing.T) {
			cp := testcp.New(t)
			st, id := enrollDevice(t, cp)
			var rt *hostCleanerRuntime
			h := startWith(t, cp, st, id, func(o *Options) {
				rt = &hostCleanerRuntime{Runtime: o.Runtime, err: tc.err}
				o.Runtime = rt
			})
			h.wait(0, "state report", func(e *ownsandv1.AgentEnvelope) bool { return e.GetStateReport() != nil })
			cp.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_Configuration{Configuration: &ownsandv1.AgentConfiguration{Limits: limits}}})
			sandboxID, _ := uuidv7.NewString()
			if res := h.command(createCommand(sandboxID)); res.GetCreateSandbox().GetSandbox().GetState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
				t.Fatalf("create result %v", res)
			}
			cp.MarkRemoved(id.DeviceID)
			cmdID, _ := uuidv7.NewString()
			cp.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_Command{Command: &ownsandv1.AgentCommand{CommandId: cmdID, Command: &ownsandv1.AgentCommand_CleanUp{CleanUp: &ownsandv1.CleanUpCommand{}}}}})
			select {
			case err := <-h.errc:
				if !errors.Is(err, ErrRemoved) {
					t.Fatalf("run returned %v", err)
				}
				h.errc <- err
			case <-time.After(20 * time.Second):
				t.Fatal("agent did not finish the removal")
			}
			rt.mu.Lock()
			calls, running := rt.calls, rt.running
			rt.mu.Unlock()
			if calls != 1 {
				t.Fatalf("CleanupHost calls = %d, want 1", calls)
			}
			if len(running) != 0 {
				t.Fatalf("CleanupHost ran while sandboxes ran: %v", running)
			}
			if cp.Confirms() != 1 {
				t.Fatal("removal not confirmed")
			}
			requireEmptyStateDir(t, st)
		})
	}
}

// failFirstDeleteRuntime fails the first Delete call and counts all calls.
type failFirstDeleteRuntime struct {
	vmruntime.Runtime
	mu          sync.Mutex
	deleteCalls int
}

func (r *failFirstDeleteRuntime) Delete(ctx context.Context, id string) error {
	r.mu.Lock()
	r.deleteCalls++
	first := r.deleteCalls == 1
	r.mu.Unlock()
	if first {
		return errors.New("injected stop failure")
	}
	return r.Runtime.Delete(ctx, id)
}

// TestRemovalRetriesFailedSandboxDeletion checks that a runtime deletion
// that fails during the device removal keeps the clean-up pending, with the
// sandbox record on disk and no removal confirmation, and that the retry of
// the removal (the interrupted-removal path of the next agent start) deletes
// the sandbox again and confirms the removal only then.
func TestRemovalRetriesFailedSandboxDeletion(t *testing.T) {
	cp := testcp.New(t)
	st, id := enrollDevice(t, cp)
	var rt *failFirstDeleteRuntime
	h := startWith(t, cp, st, id, func(o *Options) {
		rt = &failFirstDeleteRuntime{Runtime: o.Runtime}
		o.Runtime = rt
	})
	h.wait(0, "state report", func(e *ownsandv1.AgentEnvelope) bool { return e.GetStateReport() != nil })
	cp.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_Configuration{Configuration: &ownsandv1.AgentConfiguration{Limits: limits}}})
	sandboxID, _ := uuidv7.NewString()
	if res := h.command(createCommand(sandboxID)); res.GetCreateSandbox().GetSandbox().GetState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		t.Fatalf("create result %v", res)
	}
	cp.MarkRemoved(id.DeviceID)
	cmdID, _ := uuidv7.NewString()
	cp.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_Command{Command: &ownsandv1.AgentCommand{CommandId: cmdID, Command: &ownsandv1.AgentCommand_CleanUp{CleanUp: &ownsandv1.CleanUpCommand{}}}}})
	select {
	case err := <-h.errc:
		if err == nil || errors.Is(err, ErrRemoved) {
			t.Fatalf("run returned %v, want the failed deletion", err)
		}
		h.errc <- err
	case <-time.After(20 * time.Second):
		t.Fatal("agent did not finish the first removal attempt")
	}
	rt.mu.Lock()
	calls := rt.deleteCalls
	rt.mu.Unlock()
	if calls != 1 {
		t.Fatalf("Delete calls after the first attempt = %d, want 1", calls)
	}
	if cp.Confirms() != 0 {
		t.Fatal("removal confirmed although the clean-up failed")
	}
	if state, err := st.LoadAgent(); err != nil || !state.Removal.NoticeReceived || state.Removal.CleanupDone {
		t.Fatalf("removal state after the failed clean-up: %+v, %v", state.Removal, err)
	}
	if _, err := os.Stat(st.Path(store.SandboxesDir, sandboxID+".json")); err != nil {
		t.Fatalf("sandbox record after the failed clean-up: %v", err)
	}

	// The retry: the next agent start continues the interrupted removal.
	h2 := start(t, cp, st, id)
	select {
	case err := <-h2.errc:
		if !errors.Is(err, ErrRemoved) {
			t.Fatalf("retry returned %v", err)
		}
		h2.errc <- err
	case <-time.After(20 * time.Second):
		t.Fatal("agent did not finish the retried removal")
	}
	if cp.Confirms() != 1 {
		t.Fatalf("removal confirmations = %d, want 1", cp.Confirms())
	}
	requireEmptyStateDir(t, st)
}

func TestInterruptedRemovalTreatsUnknownDeviceAsConfirmed(t *testing.T) {
	cp := testcp.New(t)
	st, id := enrollDevice(t, cp)
	// An earlier process finished the clean-up and its confirmation
	// succeeded, but the response was lost.
	st.UpdateAgent(func(a *store.AgentState) error {
		a.Removal = store.RemovalRecord{NoticeReceived: true, CleanupDone: true}
		return nil
	})
	cp.Forget(id.DeviceID)
	h := start(t, cp, st, id)
	select {
	case err := <-h.errc:
		if !errors.Is(err, ErrRemoved) {
			t.Fatalf("run returned %v", err)
		}
		h.errc <- err
	case <-time.After(20 * time.Second):
		t.Fatal("agent did not finish the removal")
	}
}

func TestUnknownDeviceKeepsData(t *testing.T) {
	cp := testcp.New(t)
	st, id := enrollDevice(t, cp)
	cp.Forget(id.DeviceID)
	h := start(t, cp, st, id)
	time.Sleep(300 * time.Millisecond)
	select {
	case err := <-h.errc:
		t.Fatalf("agent stopped: %v", err)
	default:
	}
	if _, err := st.LoadIdentity(); err != nil {
		t.Fatal("identity deleted for an unknown device without a removal notice")
	}
}

// bucket is a fake R2 bucket: PUT stores, GET returns.
func bucket(t *testing.T) *httptest.Server {
	return bucketWithHook(t, nil)
}

// bucketWithHook is bucket with a hook that sees every request before the
// bucket serves it, without holding the bucket lock, so that it can block.
// When the hook returns a non-zero status, the bucket answers with it and
// does not serve the request.
func bucketWithHook(t *testing.T, hook func(r *http.Request) int) *httptest.Server {
	var mu sync.Mutex
	objects := map[string][]byte{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hook != nil {
			if status := hook(r); status != 0 {
				http.Error(w, "injected failure", status)
				return
			}
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPut:
			b, _ := io.ReadAll(r.Body)
			objects[r.URL.Path] = b
		case http.MethodGet:
			b, ok := objects[r.URL.Path]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Write(b)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSnapshotExportImport(t *testing.T) {
	cp := testcp.New(t)
	b := bucket(t)
	cp.SetSignedURLBase(b.URL)
	st, id := enrollDevice(t, cp)
	h := start(t, cp, st, id)
	h.wait(0, "state report", func(e *ownsandv1.AgentEnvelope) bool { return e.GetStateReport() != nil })
	sandboxID, _ := uuidv7.NewString()
	if res := h.command(createCommand(sandboxID)); res.GetError() != nil {
		t.Fatal(res.GetError())
	}
	snapID, _ := uuidv7.NewString()
	res := h.command(&ownsandv1.AgentCommand{Command: &ownsandv1.AgentCommand_CreateSnapshot{CreateSnapshot: &ownsandv1.CreateSnapshotCommand{SnapshotId: snapID, SandboxId: sandboxID}}})
	snap := res.GetCreateSnapshot().GetSnapshot()
	res = h.command(&ownsandv1.AgentCommand{Command: &ownsandv1.AgentCommand_ExportSnapshot{ExportSnapshot: &ownsandv1.ExportSnapshotCommand{SnapshotId: snapID, PartSizeBytes: 512}}})
	if res.GetError() != nil || len(res.GetExportSnapshot().GetParts()) == 0 {
		t.Fatalf("export %v", res)
	}
	parts := res.GetExportSnapshot().GetParts()
	// Import under a new ID, as another device would.
	imported, _ := uuidv7.NewString()
	b2 := proto.Clone(snap).(*ownsandv1.SnapshotStateEntry)
	b2.SnapshotId = imported
	// The fake bucket keys objects by snapshot ID; copy the parts.
	for _, p := range parts {
		resp, err := http.Get(fmt.Sprintf("%s/%s/%d", b.URL, snapID, p.GetPartNumber()))
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		req, _ := http.NewRequest(http.MethodPut, fmt.Sprintf("%s/%s/%d", b.URL, imported, p.GetPartNumber()), bytes.NewReader(data))
		if _, err := http.DefaultClient.Do(req); err != nil {
			t.Fatal(err)
		}
	}
	res = h.command(&ownsandv1.AgentCommand{Command: &ownsandv1.AgentCommand_ImportSnapshot{ImportSnapshot: &ownsandv1.ImportSnapshotCommand{Snapshot: b2, Parts: parts}}})
	if res.GetError() != nil || res.GetImportSnapshot().GetSnapshot().GetState() != ownsandv1.SnapshotState_SNAPSHOT_STATE_READY {
		t.Fatalf("import %v", res)
	}
	// A sandbox from the imported snapshot.
	fromSnap, _ := uuidv7.NewString()
	cmd := createCommand(fromSnap)
	cmd.GetCreateSandbox().Source = &ownsandv1.CreateSandboxCommand_SnapshotId{SnapshotId: imported}
	if res := h.command(cmd); res.GetCreateSandbox().GetSandbox().GetRestorePath() != ownsandv1.SandboxRestorePath_SANDBOX_RESTORE_PATH_DISK_COLD_BOOT {
		t.Fatalf("create from imported snapshot %v", res)
	}
	// A tampered hash fails the import.
	parts[0].Sha256[0] ^= 1
	bad, _ := uuidv7.NewString()
	b3 := proto.Clone(snap).(*ownsandv1.SnapshotStateEntry)
	b3.SnapshotId = bad
	if res := h.command(&ownsandv1.AgentCommand{Command: &ownsandv1.AgentCommand_ImportSnapshot{ImportSnapshot: &ownsandv1.ImportSnapshotCommand{Snapshot: b3, Parts: parts}}}); res.GetError() == nil {
		t.Fatal("tampered import accepted")
	}
}

// sendCommand sends a command without waiting for its result.
func (h *harness) sendCommand(cmd *ownsandv1.AgentCommand) string {
	h.t.Helper()
	cmd.CommandId, _ = uuidv7.NewString()
	if !h.cp.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_Command{Command: cmd}}) {
		h.t.Fatal("no channel")
	}
	return cmd.GetCommandId()
}

func (h *harness) result(commandID string) *ownsandv1.AgentCommandResult {
	h.t.Helper()
	res := h.cp.CommandResult(30*time.Second, commandID)
	if res == nil {
		h.t.Fatal("no command result")
	}
	return res
}

// importFixture holds a snapshot that was exported from the agent and whose
// parts the fake bucket serves under other snapshot IDs.
type importFixture struct {
	h         *harness
	b         *httptest.Server
	sandboxID string
	snap      *ownsandv1.SnapshotStateEntry
	parts     []*ownsandv1.SnapshotPart
}

func newImportFixture(t *testing.T, hook func(r *http.Request) int) *importFixture {
	t.Helper()
	return newImportFixtureWith(t, hook, nil)
}

// newImportFixtureWith is newImportFixture with options that tweak the agent.
func newImportFixtureWith(t *testing.T, hook func(r *http.Request) int, tweak func(*Options)) *importFixture {
	t.Helper()
	cp := testcp.New(t)
	b := bucketWithHook(t, hook)
	cp.SetSignedURLBase(b.URL)
	st, id := enrollDevice(t, cp)
	h := startWith(t, cp, st, id, tweak)
	h.wait(0, "state report", func(e *ownsandv1.AgentEnvelope) bool { return e.GetStateReport() != nil })
	sandboxID, _ := uuidv7.NewString()
	if res := h.command(createCommand(sandboxID)); res.GetError() != nil {
		t.Fatal(res.GetError())
	}
	snapID, _ := uuidv7.NewString()
	res := h.command(&ownsandv1.AgentCommand{Command: &ownsandv1.AgentCommand_CreateSnapshot{CreateSnapshot: &ownsandv1.CreateSnapshotCommand{SnapshotId: snapID, SandboxId: sandboxID}}})
	snap := res.GetCreateSnapshot().GetSnapshot()
	res = h.command(&ownsandv1.AgentCommand{Command: &ownsandv1.AgentCommand_ExportSnapshot{ExportSnapshot: &ownsandv1.ExportSnapshotCommand{SnapshotId: snapID, PartSizeBytes: 512}}})
	if res.GetError() != nil || len(res.GetExportSnapshot().GetParts()) == 0 {
		t.Fatalf("export %v", res)
	}
	return &importFixture{h: h, b: b, sandboxID: sandboxID, snap: snap, parts: res.GetExportSnapshot().GetParts()}
}

// stage copies the exported parts to the bucket under a new snapshot ID and
// returns an import command for it. With badHash the command carries a
// wrong hash for the first part, so the import fails its hash check.
func (f *importFixture) stage(t *testing.T, id string, badHash bool) *ownsandv1.AgentCommand {
	t.Helper()
	src := f.snap.GetSnapshotId()
	for _, p := range f.parts {
		resp, err := http.Get(fmt.Sprintf("%s/%s/%d", f.b.URL, src, p.GetPartNumber()))
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		req, _ := http.NewRequest(http.MethodPut, fmt.Sprintf("%s/%s/%d", f.b.URL, id, p.GetPartNumber()), bytes.NewReader(data))
		resp2, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp2.Body.Close()
	}
	return f.command(id, badHash)
}

func (f *importFixture) command(id string, badHash bool) *ownsandv1.AgentCommand {
	entry := proto.Clone(f.snap).(*ownsandv1.SnapshotStateEntry)
	entry.SnapshotId = id
	parts := make([]*ownsandv1.SnapshotPart, len(f.parts))
	for i, p := range f.parts {
		parts[i] = proto.Clone(p).(*ownsandv1.SnapshotPart)
	}
	if badHash {
		parts[0].Sha256[0] ^= 1
	}
	return &ownsandv1.AgentCommand{Command: &ownsandv1.AgentCommand_ImportSnapshot{ImportSnapshot: &ownsandv1.ImportSnapshotCommand{Snapshot: entry, Parts: parts}}}
}

// requireRestorable checks that the snapshot content is on disk and that a
// sandbox restores from it.
func (f *importFixture) requireRestorable(t *testing.T, id string) {
	t.Helper()
	dir, err := f.h.st.SnapshotDir(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "content")); err != nil {
		t.Fatalf("snapshot content after the import: %v", err)
	}
	fromSnap, _ := uuidv7.NewString()
	cmd := createCommand(fromSnap)
	cmd.GetCreateSandbox().Source = &ownsandv1.CreateSandboxCommand_SnapshotId{SnapshotId: id}
	if res := f.h.command(cmd); res.GetError() != nil || res.GetCreateSandbox().GetSandbox().GetRestorePath() != ownsandv1.SandboxRestorePath_SANDBOX_RESTORE_PATH_DISK_COLD_BOOT {
		t.Fatalf("create from the imported snapshot %v", res)
	}
	// Delete the restored sandbox, so that a test can check several
	// snapshots: the process runtime runs macOS guests on a macOS host, and
	// the agent runs at most two macOS guests at a time.
	if res := f.h.command(&ownsandv1.AgentCommand{Command: &ownsandv1.AgentCommand_DeleteSandbox{DeleteSandbox: &ownsandv1.DeleteSandboxCommand{SandboxId: fromSnap}}}); res.GetError() != nil {
		t.Fatalf("delete the sandbox restored from the imported snapshot %v", res)
	}
}

// getGate counts the part downloads of one snapshot ID and blocks the first
// of them until release is closed.
type getGate struct {
	id      string
	mu      sync.Mutex
	gets    int
	started chan struct{}
	release chan struct{}
}

func newGetGate(id string) *getGate {
	return &getGate{id: id, started: make(chan struct{}), release: make(chan struct{})}
}

func (g *getGate) hook(r *http.Request) int {
	if r.Method != http.MethodGet || !strings.HasPrefix(r.URL.Path, "/"+g.id+"/") {
		return 0
	}
	g.mu.Lock()
	g.gets++
	first := g.gets == 1
	g.mu.Unlock()
	if first {
		close(g.started)
		<-g.release
	}
	return 0
}

func (g *getGate) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.gets
}

func waitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// settle gives a command that waits for a lock time to reach it. The
// assertions below hold whether or not it arrived, so the delay only makes
// the contended path likely and never makes the test flaky.
func settle() { time.Sleep(300 * time.Millisecond) }

// TestConcurrentImportsOfOneSnapshotDownloadOnce checks that two imports of
// the same absent snapshot return the same ready entry after one download.
func TestConcurrentImportsOfOneSnapshotDownloadOnce(t *testing.T) {
	id, _ := uuidv7.NewString()
	gate := newGetGate(id)
	f := newImportFixture(t, gate.hook)
	first := f.h.sendCommand(f.stage(t, id, false))
	waitClosed(t, gate.started, "the first download")
	second := f.h.sendCommand(f.command(id, false))
	settle()
	close(gate.release)
	r1, r2 := f.h.result(first), f.h.result(second)
	for _, r := range []*ownsandv1.AgentCommandResult{r1, r2} {
		if r.GetError() != nil || r.GetImportSnapshot().GetSnapshot().GetState() != ownsandv1.SnapshotState_SNAPSHOT_STATE_READY {
			t.Fatalf("import %v", r)
		}
	}
	if !proto.Equal(r1.GetImportSnapshot().GetSnapshot(), r2.GetImportSnapshot().GetSnapshot()) {
		t.Fatalf("entries differ: %v and %v", r1.GetImportSnapshot().GetSnapshot(), r2.GetImportSnapshot().GetSnapshot())
	}
	if got := gate.count(); got != len(f.parts) {
		t.Fatalf("part downloads = %d, want one download of %d parts", got, len(f.parts))
	}
	f.requireRestorable(t, id)
}

// TestFailedImportDoesNotDeleteAPublishedCopy checks that a failing import
// of a snapshot neither blocks the waiting import of the same snapshot from
// downloading and publishing it nor deletes the copy it publishes, and that
// a later import reuses the copy.
func TestFailedImportDoesNotDeleteAPublishedCopy(t *testing.T) {
	id, _ := uuidv7.NewString()
	gate := newGetGate(id)
	f := newImportFixture(t, gate.hook)
	bad := f.h.sendCommand(f.stage(t, id, true))
	waitClosed(t, gate.started, "the first download")
	good := f.h.sendCommand(f.command(id, false))
	// Without the per-snapshot lock the good import publishes the copy while
	// the bad one still waits at its gate, and the bad one then deletes it.
	settle()
	close(gate.release)
	badRes, goodRes := f.h.result(bad), f.h.result(good)
	if badRes.GetError().GetCode() != ownsandv1.ErrorCode_ERROR_CODE_DATA_LOSS {
		t.Fatalf("bad import = %v", badRes)
	}
	if goodRes.GetError() != nil || goodRes.GetImportSnapshot().GetSnapshot().GetState() != ownsandv1.SnapshotState_SNAPSHOT_STATE_READY {
		t.Fatalf("good import = %v", goodRes)
	}
	f.requireRestorable(t, id)
	// A later import reuses the copy: even one that would fail its hash check
	// returns the ready entry and leaves the content alone.
	if res := f.h.command(f.command(id, true)); res.GetError() != nil || res.GetImportSnapshot().GetSnapshot().GetState() != ownsandv1.SnapshotState_SNAPSHOT_STATE_READY {
		t.Fatalf("later import = %v", res)
	}
	f.requireRestorable(t, id)
}

// TestImportStopsWithItsChannelContext checks that an import whose channel
// context ends during a download publishes nothing: no ready snapshot, no
// content directory and no leftover files, and that an import of the same
// snapshot afterwards still succeeds.
func TestImportStopsWithItsChannelContext(t *testing.T) {
	id, _ := uuidv7.NewString()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var downloads atomic.Int32
	f := newImportFixture(t, func(r *http.Request) int {
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/"+id+"/") && downloads.Add(1) == 2 {
			// The channel closes while the import still downloads.
			cancel()
		}
		return 0
	})
	cmd := f.stage(t, id, false).GetImportSnapshot()
	if len(cmd.GetParts()) < 3 {
		t.Fatalf("the fixture needs at least 3 parts, has %d", len(cmd.GetParts()))
	}
	if _, err := f.h.agent.importSnapshot(ctx, cmd); err == nil {
		t.Fatal("import succeeded with a closed channel context")
	}
	if _, _, ok := f.h.agent.Sandboxes.SnapshotContent(id); ok {
		t.Fatal("a cancelled import left a ready snapshot")
	}
	dir, err := f.h.st.SnapshotDir(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("snapshot directory after a cancelled import: %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(f.h.st.Path(store.SnapshotsDir), ".import-*")); len(left) != 0 {
		t.Fatalf("temporary archives left: %v", left)
	}
	if res := f.h.command(f.command(id, false)); res.GetError() != nil {
		t.Fatalf("import after the cancelled one = %v", res)
	}
	f.requireRestorable(t, id)
}

// TestImportWithCanceledContextAbortsEvenWithFreeLockAndReadyCopy checks that
// an import whose channel context ended before the lock was free to take
// returns ABORTED and not the ready entry, whether or not a ready copy of the
// snapshot exists, and that the aborted imports leave no lock token behind, so
// a later import with a live context reuses the ready copy.
func TestImportWithCanceledContextAbortsEvenWithFreeLockAndReadyCopy(t *testing.T) {
	id, _ := uuidv7.NewString()
	f := newImportFixture(t, nil)
	cmd := f.stage(t, id, false).GetImportSnapshot()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	abortedBefore := func(label string) {
		t.Helper()
		// A free lock and a canceled context are both ready; the lock case
		// must not win, whatever the random choice of select is.
		for i := 0; i < 100; i++ {
			_, err := f.h.agent.importSnapshot(ctx, cmd)
			var ce *sandboxes.CommandError
			if !errors.As(err, &ce) || ce.Code != ownsandv1.ErrorCode_ERROR_CODE_ABORTED {
				t.Fatalf("%s, iteration %d: canceled import = %v, want ABORTED", label, i, err)
			}
		}
	}
	// No ready copy: the canceled import publishes nothing and downloads nothing.
	abortedBefore("without a ready copy")
	if _, _, ok := f.h.agent.Sandboxes.SnapshotContent(id); ok {
		t.Fatal("a canceled import published a ready snapshot")
	}
	dir, err := f.h.st.SnapshotDir(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("snapshot directory after canceled imports: %v", err)
	}
	// A live import publishes the copy; a canceled one must still abort and
	// leave the copy alone.
	if res := f.h.command(f.command(id, false)); res.GetError() != nil {
		t.Fatalf("import with a live context = %v", res)
	}
	abortedBefore("with a ready copy")
	f.requireRestorable(t, id)
	// The aborted imports left no token: a live import reuses the ready copy,
	// even one that would fail its hash check.
	if res := f.h.command(f.command(id, true)); res.GetError() != nil || res.GetImportSnapshot().GetSnapshot().GetState() != ownsandv1.SnapshotState_SNAPSHOT_STATE_READY {
		t.Fatalf("later import = %v", res)
	}
	f.requireRestorable(t, id)
}

// TestImportsOfDifferentSnapshotsStayConcurrent checks that an import that
// waits on its download does not block the import of another snapshot.
func TestImportsOfDifferentSnapshotsStayConcurrent(t *testing.T) {
	slow, _ := uuidv7.NewString()
	fast, _ := uuidv7.NewString()
	gate := newGetGate(slow)
	f := newImportFixture(t, gate.hook)
	slowID := f.h.sendCommand(f.stage(t, slow, false))
	waitClosed(t, gate.started, "the first download")
	fastCmd := f.stage(t, fast, false)
	// The fast import finishes while the slow one still waits.
	if res := f.h.command(fastCmd); res.GetError() != nil || res.GetImportSnapshot().GetSnapshot().GetState() != ownsandv1.SnapshotState_SNAPSHOT_STATE_READY {
		t.Fatalf("fast import = %v", res)
	}
	close(gate.release)
	if res := f.h.result(slowID); res.GetError() != nil {
		t.Fatalf("slow import = %v", res)
	}
	f.requireRestorable(t, fast)
	f.requireRestorable(t, slow)
}

// TestEnrollRetriesAfterOriginWriteFailure checks that a failed agent.json
// write leaves only the pending-key identity, so a retry with the same token
// and key completes the enrollment.
func TestEnrollRetriesAfterOriginWriteFailure(t *testing.T) {
	cp := testcp.New(t)
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	client, err := cpclient.New(cp.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A directory at agent.json makes the atomic rename fail.
	blocker := st.Path(store.AgentFile)
	if err := os.MkdirAll(filepath.Join(blocker, "keep"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Enroll(context.Background(), st, client, testcp.Token, "host", quietLogger()); err == nil {
		t.Fatal("enroll succeeded with a blocked agent.json")
	}
	pending, err := st.LoadIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if pending.DeviceID != "" || pending.PrivateKeySeed == nil {
		t.Fatalf("pending identity: %+v", pending)
	}
	if err := os.RemoveAll(blocker); err != nil {
		t.Fatal(err)
	}
	id, err := Enroll(context.Background(), st, client, testcp.Token, "host", quietLogger())
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if id.DeviceID == "" || string(id.PrivateKeySeed) != string(pending.PrivateKeySeed) {
		t.Fatalf("retry identity: %+v", id)
	}
	loaded, err := st.LoadIdentity()
	if err != nil || loaded.DeviceID != id.DeviceID {
		t.Fatalf("loaded identity: %+v, %v", loaded, err)
	}
	a, err := st.LoadAgent()
	if err != nil || a.APIURL != client.APIURL {
		t.Fatalf("agent state: %+v, %v", a, err)
	}
	if _, err := Enroll(context.Background(), st, client, testcp.Token, "host", quietLogger()); !errors.Is(err, ErrAlreadyEnrolled) {
		t.Fatalf("third enroll: %v", err)
	}
}

// TestWithdrawAgentUpdateCommand checks that the agent answers the withdrawal
// command with success when no update is queued, and keeps reporting the
// UP_TO_DATE update state.
func TestWithdrawAgentUpdateCommand(t *testing.T) {
	cp := testcp.New(t)
	st, id := enrollDevice(t, cp)
	h := start(t, cp, st, id)
	h.wait(0, "capability report", func(e *ownsandv1.AgentEnvelope) bool { return e.GetCapabilityReport() != nil })
	res := h.command(&ownsandv1.AgentCommand{Command: &ownsandv1.AgentCommand_WithdrawAgentUpdate{WithdrawAgentUpdate: &ownsandv1.WithdrawAgentUpdateCommand{}}})
	if res.GetError() != nil {
		t.Fatalf("withdrawal failed: %v", res.GetError())
	}
	if state, target := h.agent.updater.State(); state != ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_UP_TO_DATE || target != "" {
		t.Fatalf("update state %v %q", state, target)
	}
}

// updateEnv serves a signed release and counts installer calls, so that a test
// can stage a verified update on the agent. The installer blocks in Install
// until unblock is closed when block is set.
type updateEnv struct {
	installs atomic.Int32
	entered  chan struct{}
	unblock  chan struct{}
	block    bool
	keys     []ed25519.PublicKey
	manifest string
}

func (e *updateEnv) Install(string) error {
	if e.installs.Add(1) == 1 {
		close(e.entered)
	}
	if e.block {
		<-e.unblock
	}
	return nil
}

func newUpdateEnv(t *testing.T, block bool) *updateEnv {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	e := &updateEnv{entered: make(chan struct{}), unblock: make(chan struct{}), block: block, keys: []ed25519.PublicKey{pub}}
	artifact := []byte("new agent binary")
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	sum := sha256.Sum256(artifact)
	manifest, _ := json.Marshal(update.Manifest{Schema: 1, Releases: []update.Release{{Version: "1.1.0", Default: true, Artifacts: []update.Artifact{{Platform: "linux-amd64", URL: srv.URL + "/agent", SHA256: hex.EncodeToString(sum[:])}}}}})
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(priv, manifest))
	mux.HandleFunc("/manifest.json", func(w http.ResponseWriter, _ *http.Request) { w.Write(manifest) })
	mux.HandleFunc("/manifest.json.sig", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(sig)) })
	mux.HandleFunc("/agent", func(w http.ResponseWriter, _ *http.Request) { w.Write(artifact) })
	e.manifest = srv.URL + "/manifest.json"
	return e
}

func (e *updateEnv) tweak(o *Options) {
	o.Update.Keys = e.keys
	o.Update.ManifestURL = e.manifest
	o.Update.Installer = e
}

// stageUpdate sends an update command, so the agent holds a verified
// artifact, and deletes the fixture sandbox, so that only transfers and the
// installer decide whether the device is idle.
func (f *importFixture) stageUpdate(t *testing.T) {
	t.Helper()
	if res := f.h.command(&ownsandv1.AgentCommand{Command: &ownsandv1.AgentCommand_UpdateAgent{UpdateAgent: &ownsandv1.UpdateAgentCommand{TargetVersion: "1.1.0"}}}); res.GetError() != nil {
		t.Fatalf("update command: %v", res.GetError())
	}
	if res := f.h.command(&ownsandv1.AgentCommand{Command: &ownsandv1.AgentCommand_DeleteSandbox{DeleteSandbox: &ownsandv1.DeleteSandboxCommand{SandboxId: f.sandboxID}}}); res.GetError() != nil {
		t.Fatalf("delete sandbox: %v", res.GetError())
	}
}

func waitInstalls(t *testing.T, e *updateEnv, want int32) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for e.installs.Load() != want {
		if time.Now().After(deadline) {
			t.Fatalf("installer calls = %d, want %d", e.installs.Load(), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestUpdateWaitsForRunningImport checks that a pending verified update does
// not call the installer while an import holds its reservation and installs
// once after the import ended.
func TestUpdateWaitsForRunningImport(t *testing.T) {
	id, _ := uuidv7.NewString()
	gate := newGetGate(id)
	env := newUpdateEnv(t, false)
	f := newImportFixtureWith(t, gate.hook, env.tweak)
	f.stageUpdate(t)
	cmdID := f.h.sendCommand(f.stage(t, id, false))
	waitClosed(t, gate.started, "the import download")
	// A failed assertion must not leave the import blocked: the agent cleanup
	// waits for its commands.
	t.Cleanup(func() {
		select {
		case <-gate.release:
		default:
			close(gate.release)
		}
	})
	if ok, err := f.h.agent.updater.TryInstall(); ok || err != nil {
		t.Fatalf("install during an import: %v %v", ok, err)
	}
	settle()
	if n := env.installs.Load(); n != 0 {
		t.Fatalf("installer calls during an import = %d", n)
	}
	close(gate.release)
	if res := f.h.result(cmdID); res.GetError() != nil {
		t.Fatalf("import = %v", res)
	}
	// The release wakes the installation loop.
	waitInstalls(t, env, 1)
}

// TestFailedImportReleasesTransferReservation checks that an import that
// fails its hash check does not keep the update from installing.
func TestFailedImportReleasesTransferReservation(t *testing.T) {
	id, _ := uuidv7.NewString()
	env := newUpdateEnv(t, false)
	f := newImportFixtureWith(t, nil, env.tweak)
	f.stageUpdate(t)
	// The command runs the transfer before it fails, and the update waits for
	// it only until it released the reservation.
	if res := f.h.command(f.stage(t, id, true)); res.GetError().GetCode() != ownsandv1.ErrorCode_ERROR_CODE_DATA_LOSS {
		t.Fatalf("bad import = %v", res)
	}
	waitInstalls(t, env, 1)
}

// TestTransfersRefusedWhileUpdateInstalls checks that export and uncached
// import commands fail with UNAVAILABLE before any transfer I/O once the
// installation reserved the device, and that an import of a ready copy still
// returns it.
func TestTransfersRefusedWhileUpdateInstalls(t *testing.T) {
	ready, _ := uuidv7.NewString()
	fresh, _ := uuidv7.NewString()
	gate := newGetGate(fresh)
	env := newUpdateEnv(t, true)
	f := newImportFixtureWith(t, gate.hook, env.tweak)
	// Cleanups run last in, first out: release the installer before the agent
	// cleanup waits for Run to end.
	t.Cleanup(func() { close(env.unblock) })
	// A copy that exists before the installation starts.
	if res := f.h.command(f.stage(t, ready, false)); res.GetError() != nil {
		t.Fatalf("import = %v", res)
	}
	staged := f.stage(t, fresh, false)
	f.stageUpdate(t)
	// The installation loop only ticks every 10 seconds; start it now. The
	// blocked installer keeps the installing flag set.
	go f.h.agent.updater.TryInstall()
	waitClosed(t, env.entered, "the installer")
	export := &ownsandv1.AgentCommand{Command: &ownsandv1.AgentCommand_ExportSnapshot{ExportSnapshot: &ownsandv1.ExportSnapshotCommand{SnapshotId: f.snap.GetSnapshotId(), PartSizeBytes: 512}}}
	if res := f.h.command(export); res.GetError().GetCode() != ownsandv1.ErrorCode_ERROR_CODE_UNAVAILABLE {
		t.Fatalf("export during the installation = %v", res)
	}
	if res := f.h.command(staged); res.GetError().GetCode() != ownsandv1.ErrorCode_ERROR_CODE_UNAVAILABLE {
		t.Fatalf("import during the installation = %v", res)
	}
	if n := gate.count(); n != 0 {
		t.Fatalf("part downloads of the refused import = %d", n)
	}
	dir, err := f.h.st.SnapshotDir(fresh)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refused import changed the snapshot directory: %v", err)
	}
	if res := f.h.command(f.command(ready, false)); res.GetError() != nil || res.GetImportSnapshot().GetSnapshot().GetState() != ownsandv1.SnapshotState_SNAPSHOT_STATE_READY {
		t.Fatalf("cached import during the installation = %v", res)
	}
	if n := env.installs.Load(); n != 1 {
		t.Fatalf("installer calls = %d, want 1", n)
	}
}

// getCounter counts the part downloads of one snapshot ID and never blocks.
type getCounter struct {
	id string
	n  atomic.Int32
}

func (c *getCounter) hook(r *http.Request) int {
	if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/"+c.id+"/") {
		c.n.Add(1)
	}
	return 0
}

// removalScript scripts the removal of stored snapshot copies through the
// removal hook of the sandbox manager: it fails the removal, holds it until
// the test releases it, or lets it run.
type removalScript struct {
	mu      sync.Mutex
	fail    bool
	pause   bool
	calls   int
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newRemovalScript() *removalScript {
	return &removalScript{entered: make(chan struct{}), release: make(chan struct{})}
}

func (r *removalScript) set(fail, pause bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fail, r.pause = fail, pause
}

func (r *removalScript) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func (r *removalScript) hook(string) error {
	r.mu.Lock()
	r.calls++
	fail, pause := r.fail, r.pause
	r.mu.Unlock()
	if fail {
		return errors.New("injected removal failure")
	}
	if pause {
		r.once.Do(func() { close(r.entered) })
		<-r.release
	}
	return nil
}

// markedCopy imports id, then deletes it while a reader holds it and lets the
// last reader fail the removal, so the device keeps a marked copy without a
// reader. It returns the command that imports the copy again. The removal
// keeps failing until the test calls script.set.
func (f *importFixture) markedCopy(t *testing.T, id string, script *removalScript) *ownsandv1.AgentCommand {
	t.Helper()
	f.h.agent.Sandboxes.SetSnapshotRemovalHookForTest(script.hook)
	t.Cleanup(func() { f.h.agent.Sandboxes.SetSnapshotRemovalHookForTest(nil) })
	staged := f.stage(t, id, false)
	if res := f.h.command(staged); res.GetError() != nil {
		t.Fatalf("import = %v", res)
	}
	script.set(true, false)
	_, release, ok := f.h.agent.Sandboxes.AcquireSnapshot(id)
	if !ok {
		t.Fatal("imported copy is not readable")
	}
	del := &ownsandv1.AgentCommand{Command: &ownsandv1.AgentCommand_DeleteSnapshot{DeleteSnapshot: &ownsandv1.DeleteSnapshotCommand{SnapshotId: id}}}
	if res := f.h.command(del); res.GetError() != nil {
		t.Fatalf("delete = %v", res)
	}
	release() // The removal fails here; the entry stays, marked.
	if !f.h.agent.Sandboxes.HasSnapshot(id) {
		t.Fatal("marked copy left the manager")
	}
	if _, _, ok := f.h.agent.Sandboxes.SnapshotContent(id); ok {
		t.Fatal("marked copy is still usable")
	}
	if script.count() != 1 {
		t.Fatalf("removal attempts after the mark = %d, want 1", script.count())
	}
	return staged
}

// TestMarkedImportRefusedWhileUpdateInstalls checks that an import of a copy
// that a delete marked returns UNAVAILABLE once the installation started,
// before it removes the copy, touches its directory or downloads a part.
func TestMarkedImportRefusedWhileUpdateInstalls(t *testing.T) {
	id, _ := uuidv7.NewString()
	counter := &getCounter{id: id}
	script := newRemovalScript()
	env := newUpdateEnv(t, true)
	f := newImportFixtureWith(t, counter.hook, env.tweak)
	t.Cleanup(func() { close(env.unblock) })
	staged := f.markedCopy(t, id, script)
	// Let a removal that the import wrongly started succeed, so that the
	// assertions below see its effect.
	script.set(false, false)
	downloads := counter.n.Load()
	f.stageUpdate(t)
	go f.h.agent.updater.TryInstall()
	waitClosed(t, env.entered, "the installer")
	if res := f.h.command(staged); res.GetError().GetCode() != ownsandv1.ErrorCode_ERROR_CODE_UNAVAILABLE {
		t.Fatalf("import of a marked copy during the installation = %v", res)
	}
	if n := script.count(); n != 1 {
		t.Fatalf("removal attempts = %d, want only the one of the mark", n)
	}
	if n := counter.n.Load(); n != downloads {
		t.Fatalf("part downloads of the refused import = %d", n-downloads)
	}
	if !f.h.agent.Sandboxes.HasSnapshot(id) {
		t.Fatal("refused import removed the manager entry")
	}
	dir, err := f.h.st.SnapshotDir(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("refused import changed the snapshot directory: %v", err)
	}
}

// TestUpdateWaitsForStaleCopyRemovalOfImport checks that an import that took
// the reservation first holds the installation back through the removal of
// the marked copy and through the import itself.
func TestUpdateWaitsForStaleCopyRemovalOfImport(t *testing.T) {
	id, _ := uuidv7.NewString()
	script := newRemovalScript()
	env := newUpdateEnv(t, false)
	f := newImportFixtureWith(t, nil, env.tweak)
	staged := f.markedCopy(t, id, script)
	script.set(false, true)
	// A failed assertion must not leave the removal blocked: the agent
	// cleanup waits for its commands.
	t.Cleanup(func() {
		select {
		case <-script.release:
		default:
			close(script.release)
		}
	})
	f.stageUpdate(t)
	cmdID := f.h.sendCommand(staged)
	waitClosed(t, script.entered, "the removal of the marked copy")
	if ok, err := f.h.agent.updater.TryInstall(); ok || err != nil {
		t.Fatalf("install during the removal: %v %v", ok, err)
	}
	settle()
	if n := env.installs.Load(); n != 0 {
		t.Fatalf("installer calls during the removal = %d", n)
	}
	close(script.release)
	res := f.h.result(cmdID)
	if res.GetError() != nil || res.GetImportSnapshot().GetSnapshot().GetState() != ownsandv1.SnapshotState_SNAPSHOT_STATE_READY {
		t.Fatalf("import = %v", res)
	}
	// The release of the reservation wakes the installation loop.
	waitInstalls(t, env, 1)
}

// TestImportStopsWhenStaleCopyRemovalFails checks that a failed removal of
// the marked copy still stops the import, with the copy left in place, and
// that the import releases its reservation.
func TestImportStopsWhenStaleCopyRemovalFails(t *testing.T) {
	id, _ := uuidv7.NewString()
	counter := &getCounter{id: id}
	script := newRemovalScript()
	env := newUpdateEnv(t, false)
	f := newImportFixtureWith(t, counter.hook, env.tweak)
	staged := f.markedCopy(t, id, script)
	downloads := counter.n.Load()
	f.stageUpdate(t)
	if res := f.h.command(staged); res.GetError().GetCode() != ownsandv1.ErrorCode_ERROR_CODE_INTERNAL {
		t.Fatalf("import with a failing removal = %v", res)
	}
	if n := counter.n.Load(); n != downloads {
		t.Fatalf("part downloads after the failed removal = %d", n-downloads)
	}
	if !f.h.agent.Sandboxes.HasSnapshot(id) {
		t.Fatal("failed removal dropped the manager entry")
	}
	waitInstalls(t, env, 1)
}

// TestUpdateDefersToSandboxDuringTransfers checks that a live sandbox still
// defers the installation after the transfers ended.
func TestUpdateDefersToSandboxDuringTransfers(t *testing.T) {
	env := newUpdateEnv(t, false)
	f := newImportFixtureWith(t, nil, env.tweak)
	if res := f.h.command(&ownsandv1.AgentCommand{Command: &ownsandv1.AgentCommand_UpdateAgent{UpdateAgent: &ownsandv1.UpdateAgentCommand{TargetVersion: "1.1.0"}}}); res.GetError() != nil {
		t.Fatalf("update command: %v", res.GetError())
	}
	id, _ := uuidv7.NewString()
	if res := f.h.command(f.stage(t, id, false)); res.GetError() != nil {
		t.Fatalf("import = %v", res)
	}
	if ok, err := f.h.agent.updater.TryInstall(); ok || err != nil || env.installs.Load() != 0 {
		t.Fatalf("installed with a running sandbox: %v %v", ok, err)
	}
}

// gateRuntime holds Create until the test releases it, and reports whether
// the context of Create ended first.
type gateRuntime struct {
	vmruntime.Runtime
	started  chan struct{}
	release  chan struct{}
	canceled chan struct{}
	once     sync.Once
}

func newGateRuntime(r vmruntime.Runtime) *gateRuntime {
	return &gateRuntime{Runtime: r, started: make(chan struct{}), release: make(chan struct{}), canceled: make(chan struct{})}
}

func (g *gateRuntime) Create(ctx context.Context, spec vmruntime.Spec) (vmruntime.VM, error) {
	g.once.Do(func() { close(g.started) })
	select {
	case <-g.release:
		return g.Runtime.Create(ctx, spec)
	case <-ctx.Done():
		close(g.canceled)
		return nil, ctx.Err()
	}
}

func (g *gateRuntime) isCanceled() bool {
	select {
	case <-g.canceled:
		return true
	default:
		return false
	}
}

// startGated starts an agent with a gated runtime, waits for its first state
// report and sends a create command that blocks in the runtime.
func startGated(t *testing.T) (*harness, *gateRuntime, string, *testcp.Server) {
	t.Helper()
	cp := testcp.New(t)
	st, id := enrollDevice(t, cp)
	var gate *gateRuntime
	h := startWith(t, cp, st, id, func(o *Options) {
		gate = newGateRuntime(o.Runtime)
		o.Runtime = gate
	})
	h.wait(0, "state report", func(e *ownsandv1.AgentEnvelope) bool { return e.GetStateReport() != nil })
	cp.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_Configuration{Configuration: &ownsandv1.AgentConfiguration{Limits: limits}}})

	sandboxID, _ := uuidv7.NewString()
	cmd := createCommand(sandboxID)
	cmd.CommandId, _ = uuidv7.NewString()
	if !cp.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_Command{Command: cmd}}) {
		t.Fatal("no channel")
	}
	select {
	case <-gate.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the create did not reach the runtime")
	}
	return h, gate, sandboxID, cp
}

// startBlockedCreate extends startGated: it closes the agent channel and
// waits until the agent reconnected and reported the sandbox as creating.
func startBlockedCreate(t *testing.T) (*harness, *gateRuntime, string, *testcp.Server) {
	t.Helper()
	h, gate, sandboxID, cp := startGated(t)
	channels := cp.Channels()
	from := cp.Received()
	cp.Current().Close()
	deadline := time.Now().Add(10 * time.Second)
	for cp.Channels() == channels {
		if time.Now().After(deadline) {
			t.Fatal("agent did not reconnect")
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.wait(from, "state report listing the create", func(e *ownsandv1.AgentEnvelope) bool {
		for _, s := range e.GetStateReport().GetSandboxes() {
			if s.GetSandboxId() == sandboxID && s.GetState() == ownsandv1.SandboxState_SANDBOX_STATE_CREATING {
				return true
			}
		}
		return false
	})
	return h, gate, sandboxID, cp
}

func reportsSandboxState(e *ownsandv1.AgentEnvelope, sandboxID string, match func(*ownsandv1.SandboxStateEntry) bool) bool {
	if u := e.GetSandboxUpdate().GetSandbox(); u != nil && u.GetSandboxId() == sandboxID && match(u) {
		return true
	}
	for _, s := range e.GetStateReport().GetSandboxes() {
		if s.GetSandboxId() == sandboxID && match(s) {
			return true
		}
	}
	return false
}

func TestCreateSurvivesAgentChannelLoss(t *testing.T) {
	h, gate, sandboxID, cp := startBlockedCreate(t)
	close(gate.release)

	deadline := time.Now().Add(10 * time.Second)
	for {
		if state, _ := h.agent.Sandboxes.State(sandboxID); state == ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the sandbox did not reach RUNNING")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if gate.isCanceled() {
		t.Fatal("the channel loss canceled the create")
	}
	h.wait(0, "running report", func(e *ownsandv1.AgentEnvelope) bool {
		return reportsSandboxState(e, sandboxID, func(s *ownsandv1.SandboxStateEntry) bool {
			return s.GetState() == ownsandv1.SandboxState_SANDBOX_STATE_RUNNING
		})
	})
	if env, _ := cp.WaitFor(300*time.Millisecond, 0, func(e *ownsandv1.AgentEnvelope) bool {
		return reportsSandboxState(e, sandboxID, func(s *ownsandv1.SandboxStateEntry) bool {
			return s.GetState() == ownsandv1.SandboxState_SANDBOX_STATE_FAILED || s.GetEndReason() == ownsandv1.SandboxEndReason_SANDBOX_END_REASON_CREATE_FAILED
		})
	}); env != nil {
		t.Fatalf("the agent reported a failed create: %v", env)
	}
}

func TestDeleteAfterChannelLossStillCancelsCreate(t *testing.T) {
	h, gate, sandboxID, cp := startBlockedCreate(t)
	res := h.command(&ownsandv1.AgentCommand{Command: &ownsandv1.AgentCommand_DeleteSandbox{DeleteSandbox: &ownsandv1.DeleteSandboxCommand{SandboxId: sandboxID}}})
	if res.GetSandbox().GetSandbox().GetState() != ownsandv1.SandboxState_SANDBOX_STATE_DELETED {
		t.Fatalf("delete result %v", res)
	}
	select {
	case <-gate.canceled:
	case <-time.After(10 * time.Second):
		t.Fatal("the delete did not cancel the create")
	}
	if env, _ := cp.WaitFor(300*time.Millisecond, 0, func(e *ownsandv1.AgentEnvelope) bool {
		return reportsSandboxState(e, sandboxID, func(s *ownsandv1.SandboxStateEntry) bool {
			return s.GetState() == ownsandv1.SandboxState_SANDBOX_STATE_RUNNING
		})
	}); env != nil {
		t.Fatalf("the agent reported the canceled sandbox as running: %v", env)
	}
}

func TestShutdownCancelsRunningCreate(t *testing.T) {
	h, gate, _, _ := startGated(t)
	h.stop()
	select {
	case <-gate.canceled:
	case <-time.After(10 * time.Second):
		t.Fatal("agent shutdown did not cancel the create")
	}
}

// recordingHandler keeps every log record, with the attributes of
// Logger.With, for assertions.
type recordingHandler struct {
	mu      *sync.Mutex
	records *[]slog.Record
	attrs   []slog.Attr
}

func newRecordingHandler() *recordingHandler {
	return &recordingHandler{mu: &sync.Mutex{}, records: &[]slog.Record{}}
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	r = r.Clone()
	r.AddAttrs(h.attrs...)
	h.mu.Lock()
	*h.records = append(*h.records, r)
	h.mu.Unlock()
	return nil
}
func (h *recordingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &recordingHandler{mu: h.mu, records: h.records, attrs: append(append([]slog.Attr{}, h.attrs...), attrs...)}
}
func (h *recordingHandler) WithGroup(string) slog.Handler { return h }

func TestCommandResultWithoutChannelIsLogged(t *testing.T) {
	cp := testcp.New(t)
	st, id := enrollDevice(t, cp)
	rec := newRecordingHandler()
	rt, err := processrt.New(guestBinary, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	client, _ := cpclient.New(cp.URL, nil)
	a, err := New(context.Background(), Options{
		Store: st, Identity: id, Control: client, Runtime: rt, Logger: slog.New(rec),
		Update: update.Options{Platform: "linux-amd64", Running: "1.0.0", Installer: update.ExecutableInstaller{Path: filepath.Join(t.TempDir(), "exe")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.shutdown)

	commandID, _ := uuidv7.NewString()
	snapshotID, _ := uuidv7.NewString()
	ctx := context.Background()
	a.handleCommand(ctx, ctx, &ownsandv1.AgentCommand{CommandId: commandID, Command: &ownsandv1.AgentCommand_DeleteSnapshot{DeleteSnapshot: &ownsandv1.DeleteSnapshotCommand{SnapshotId: snapshotID}}}, 0)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	var found int
	for _, r := range *rec.records {
		if r.Message != "command result not sent" {
			continue
		}
		found++
		attrs := map[string]string{}
		r.Attrs(func(a slog.Attr) bool { attrs[a.Key] = a.Value.String(); return true })
		if r.Level != slog.LevelWarn || attrs["command_id"] != commandID || attrs["command"] != "delete_snapshot" {
			t.Fatalf("record %v attrs %v", r, attrs)
		}
	}
	if found != 1 {
		t.Fatalf("got %d records, want 1", found)
	}
}

// snapshotCtxRuntime records the context error that Snapshot sees.
type snapshotCtxRuntime struct {
	vmruntime.Runtime
	ctxErr chan error
}

func (r *snapshotCtxRuntime) Snapshot(ctx context.Context, vm vmruntime.VM, dir string) (vmruntime.SnapshotInfo, error) {
	r.ctxErr <- ctx.Err()
	return r.Runtime.Snapshot(ctx, vm, dir)
}

// TestSnapshotCaptureEndsWithItsChannel checks that a snapshot capture stays
// bound to the channel context: a pending capture is not in the state report,
// so the control plane fails it after a reconnect, and nothing reports a late
// completion. Remove this test together with that limit.
func TestSnapshotCaptureEndsWithItsChannel(t *testing.T) {
	cp := testcp.New(t)
	st, id := enrollDevice(t, cp)
	rt := &snapshotCtxRuntime{ctxErr: make(chan error, 1)}
	h := startWith(t, cp, st, id, func(o *Options) {
		rt.Runtime = o.Runtime
		o.Runtime = rt
	})
	h.wait(0, "state report", func(e *ownsandv1.AgentEnvelope) bool { return e.GetStateReport() != nil })
	cp.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_Configuration{Configuration: &ownsandv1.AgentConfiguration{Limits: limits}}})
	sandboxID, _ := uuidv7.NewString()
	if res := h.command(createCommand(sandboxID)); res.GetError() != nil {
		t.Fatalf("create %v", res)
	}
	snapID, _ := uuidv7.NewString()
	chanCtx, cancel := context.WithCancel(context.Background())
	cancel()
	h.agent.handleCommand(chanCtx, context.Background(), &ownsandv1.AgentCommand{CommandId: snapID, Command: &ownsandv1.AgentCommand_CreateSnapshot{CreateSnapshot: &ownsandv1.CreateSnapshotCommand{SnapshotId: snapID, SandboxId: sandboxID}}}, 0)
	if err := <-rt.ctxErr; err == nil {
		t.Fatal("snapshot capture ran on a context that outlives its channel")
	}
	// The runtime completed the capture although its channel ended. The
	// snapshot must not become known or ready.
	if h.agent.Sandboxes.HasSnapshot(snapID) {
		t.Fatal("snapshot captured after its channel ended is known to the manager")
	}
	for _, s := range h.agent.Sandboxes.StateReport(2, nil).GetSnapshots() {
		t.Fatalf("state report holds the snapshot %v after its channel ended", s)
	}
}

// retainedCreateRuntime models a runtime whose create failed and whose stop
// of the failed create failed too: Create starts a real VM through the
// wrapped runtime and then fails without stopping it, and the first
// failDeletes Delete calls fail before the wrapped Delete stops the VM and
// removes its files.
type retainedCreateRuntime struct {
	vmruntime.Runtime
	mu          sync.Mutex
	failDeletes int
	deleteCalls int
}

func (r *retainedCreateRuntime) Create(ctx context.Context, spec vmruntime.Spec) (vmruntime.VM, error) {
	if _, err := r.Runtime.Create(ctx, spec); err != nil {
		return nil, err
	}
	return nil, errors.New("injected create failure after the VM started")
}

func (r *retainedCreateRuntime) Delete(ctx context.Context, id string) error {
	r.mu.Lock()
	r.deleteCalls++
	fail := r.deleteCalls <= r.failDeletes
	r.mu.Unlock()
	if fail {
		return errors.New("injected stop failure")
	}
	return r.Runtime.Delete(ctx, id)
}

func (r *retainedCreateRuntime) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.deleteCalls
}

// TestFailedCreateRetainedVMIsStoppedByRunningAgent checks that a running
// agent stops the VM that a failed create kept after a failed stop, without
// a restart: the VM and its directory stay while the stop fails, and the
// retry that succeeds stops the VM and removes the directory.
func TestFailedCreateRetainedVMIsStoppedByRunningAgent(t *testing.T) {
	cp := testcp.New(t)
	st, id := enrollDevice(t, cp)
	var rt *retainedCreateRuntime
	h := startWith(t, cp, st, id, func(o *Options) {
		rt = &retainedCreateRuntime{Runtime: o.Runtime, failDeletes: 2}
		o.Runtime = rt
	})
	h.wait(0, "state report", func(e *ownsandv1.AgentEnvelope) bool { return e.GetStateReport() != nil })
	cp.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_Configuration{Configuration: &ownsandv1.AgentConfiguration{Limits: limits}}})
	sandboxID, _ := uuidv7.NewString()
	res := h.command(createCommand(sandboxID))
	if res.GetError() == nil {
		t.Fatalf("create result %v, want a failure", res)
	}
	vmDir := st.Path(store.VMsDir, sandboxID)
	running, err := rt.Runtime.Running(context.Background())
	if err != nil || len(running) != 1 || running[0] != sandboxID {
		t.Fatalf("VMs after the failed create: %v, %v; want the retained VM", running, err)
	}

	deadline := time.Now().Add(20 * time.Second)
	for rt.calls() < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("the agent retried the stop %d times, want 3", rt.calls())
		}
		time.Sleep(20 * time.Millisecond)
	}
	for {
		running, err = rt.Runtime.Running(context.Background())
		_, statErr := os.Stat(vmDir)
		if err == nil && len(running) == 0 && os.IsNotExist(statErr) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("retained VM not released after the successful stop: running %v, dir stat %v", running, statErr)
		}
		time.Sleep(20 * time.Millisecond)
	}
	calls := rt.calls()
	time.Sleep(2500 * time.Millisecond)
	if got := rt.calls(); got != calls {
		t.Fatalf("the agent kept stopping after success: %d Delete calls, want %d", got, calls)
	}
}
