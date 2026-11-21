// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build linux || darwin

package agent

import (
	"bytes"
	"context"
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
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/testcp"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/update"
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

func createCommand(id string) *ownsandv1.AgentCommand {
	now := time.Now()
	return &ownsandv1.AgentCommand{Command: &ownsandv1.AgentCommand_CreateSandbox{CreateSandbox: &ownsandv1.CreateSandboxCommand{
		SandboxId:   id,
		Source:      &ownsandv1.CreateSandboxCommand_BaseImage{BaseImage: ownsandv1.BaseImage_BASE_IMAGE_LINUX},
		Resources:   &ownsandv1.Resources{VcpuCount: 1, MemoryMib: 256, DiskGib: 1},
		NetworkMode: ownsandv1.NetworkMode_NETWORK_MODE_NONE,
		GuestOs:     ownsandv1.GuestOs_GUEST_OS_LINUX,
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
	// new state report.
	channels := cp.Channels()
	cp.SetAck(false, true)
	deadline := time.Now().Add(10 * time.Second)
	for cp.Channels() == channels {
		if time.Now().After(deadline) {
			t.Fatal("agent did not reconnect without heartbeat acknowledgements")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cp.SetAck(true, true)
	n := cp.Received()
	h.wait(n-1, "state report after reconnect", func(e *ownsandv1.AgentEnvelope) bool { return e.GetStateReport() != nil })

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
	h     *harness
	b     *httptest.Server
	snap  *ownsandv1.SnapshotStateEntry
	parts []*ownsandv1.SnapshotPart
}

func newImportFixture(t *testing.T, hook func(r *http.Request) int) *importFixture {
	t.Helper()
	cp := testcp.New(t)
	b := bucketWithHook(t, hook)
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
	return &importFixture{h: h, b: b, snap: snap, parts: res.GetExportSnapshot().GetParts()}
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
