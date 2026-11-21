// SPDX-License-Identifier: FSL-1.1-ALv2

package fcrt_test

import (
	"context"
	"crypto/rand"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/ops"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/sandboxes"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime/fcrt"
	"github.com/delinoio/mono/go/ownsand/datapath"
	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// Environment of the opt-in integration test. It boots real microVMs and
// needs /dev/kvm access, a firecracker binary, a guest kernel and a root
// file system from cmds/ownsand-agent/images/linux/. It uses NONE network
// sandboxes, so it needs no root.
const (
	envBinary = "OWNSAND_FC_TEST_BINARY"
	envKernel = "OWNSAND_FC_TEST_KERNEL"
	envRootfs = "OWNSAND_FC_TEST_ROOTFS"
)

const orgID = "0192f4b6-7c3a-7def-8abc-0123456789ab"

type sink struct {
	mu     sync.Mutex
	frames []*ownsandv1.DataFrame
}

func (s *sink) Send(_ context.Context, f *ownsandv1.DataFrame) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frames = append(s.frames, proto.Clone(f).(*ownsandv1.DataFrame))
	return nil
}

type env struct {
	t *testing.T
	m *sandboxes.Manager
	d *ops.Dispatcher
}

func (e *env) create(snapshotID string, memMiB int64, memoryRestore bool) *ownsandv1.SandboxStateEntry {
	e.t.Helper()
	id, _ := uuidv7.NewString()
	now := time.Now()
	cmd := &ownsandv1.CreateSandboxCommand{
		SandboxId:            id,
		Source:               &ownsandv1.CreateSandboxCommand_BaseImage{BaseImage: ownsandv1.BaseImage_BASE_IMAGE_LINUX},
		Resources:            &ownsandv1.Resources{VcpuCount: 1, MemoryMib: memMiB, DiskGib: 1},
		NetworkMode:          ownsandv1.NetworkMode_NETWORK_MODE_NONE,
		GuestOs:              ownsandv1.GuestOs_GUEST_OS_LINUX,
		IdleTtl:              durationpb.New(time.Hour),
		MaxTtl:               durationpb.New(time.Hour),
		CreateTime:           timestamppb.New(now),
		MaxDeadline:          timestamppb.New(now.Add(time.Hour)),
		Limits:               &ownsandv1.DeviceLimits{VcpuCount: 8, MemoryMib: 8192, DiskGib: 8, VcpuOvercommitPercent: 100},
		MemoryRestoreAllowed: memoryRestore,
	}
	if snapshotID != "" {
		cmd.Source = &ownsandv1.CreateSandboxCommand_SnapshotId{SnapshotId: snapshotID}
	}
	entry, err := e.m.Create(context.Background(), cmd)
	if err != nil {
		e.t.Fatalf("create: %v", err)
	}
	return entry
}

func (e *env) exec(sandboxID string, cmd ...string) (string, *ownsandv1.ExecExited) {
	e.t.Helper()
	id, _ := uuidv7.NewString()
	salt := make([]byte, 32)
	rand.Read(salt)
	s := &sink{}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	e.d.Handle(ctx, ops.Request{Frame: datapath.MustNewFrame(1, sandboxID, &ownsandv1.ExecRequest{
		OrganizationId: orgID, SandboxId: sandboxID, ExecId: id, Command: cmd, BindingSalt: salt,
	}), SandboxID: sandboxID}, s)
	var out strings.Builder
	var exited *ownsandv1.ExecExited
	for _, f := range s.frames {
		out.Write(f.GetExecEvent().GetStdout().GetData())
		out.Write(f.GetExecEvent().GetStderr().GetData())
		if ex := f.GetExecEvent().GetExited(); ex != nil {
			exited = ex
		}
		if f.GetError() != nil {
			e.t.Fatalf("exec error %v", f.GetError())
		}
	}
	return out.String(), exited
}

func TestFirecrackerIntegration(t *testing.T) {
	binary, kernel, rootfs := os.Getenv(envBinary), os.Getenv(envKernel), os.Getenv(envRootfs)
	if binary == "" || kernel == "" || rootfs == "" {
		t.Skipf("set %s, %s and %s to boot real microVMs", envBinary, envKernel, envRootfs)
	}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	if os.Getenv("OWNSAND_TEST_VERBOSE") != "" {
		logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rt, err := fcrt.New(fcrt.Options{Binary: binary, Kernel: kernel, Rootfs: rootfs, VMsDir: st.Path(store.VMsDir), Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	caps := rt.Capabilities(context.Background())
	if len(caps.BaseImages) != 1 {
		t.Fatalf("capabilities %+v", caps.Checks)
	}
	m, err := sandboxes.New(context.Background(), sandboxes.Options{Store: st, Runtime: rt, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	d, err := ops.New(m, st, ops.Options{Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	m.SetExecTracker(d)
	t.Cleanup(func() {
		d.Close()
		m.Close()
		rt.Close()
	})
	e := &env{t: t, m: m, d: d}

	start := time.Now()
	sb := e.create("", 256, false)
	t.Logf("cold boot took %v", time.Since(start))
	if sb.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING || sb.GetRestorePath() != ownsandv1.SandboxRestorePath_SANDBOX_RESTORE_PATH_BASE_IMAGE {
		t.Fatalf("sandbox %v", sb)
	}
	out, ex := e.exec(sb.GetSandboxId(), "/bin/sh", "-c", "echo hello; uname -s; echo state > /root/marker; df -k / | tail -1")
	if ex.GetExitCode() != 0 || !strings.Contains(out, "hello\nLinux\n") {
		t.Fatalf("exec output %q exited %v", out, ex)
	}
	t.Logf("guest df: %s", strings.TrimSpace(out))
	if _, err := m.Pause(context.Background(), &ownsandv1.PauseSandboxCommand{SandboxId: sb.GetSandboxId()}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Resume(context.Background(), &ownsandv1.ResumeSandboxCommand{SandboxId: sb.GetSandboxId()}); err != nil {
		t.Fatal(err)
	}
	if out, _ := e.exec(sb.GetSandboxId(), "/bin/cat", "/root/marker"); out != "state\n" {
		t.Fatalf("after resume %q", out)
	}
	snapID, _ := uuidv7.NewString()
	snap, err := m.CreateSnapshot(context.Background(), &ownsandv1.CreateSnapshotCommand{SnapshotId: snapID, SandboxId: sb.GetSandboxId()})
	if err != nil || !snap.GetHasMemoryState() {
		t.Fatalf("snapshot %v %v", snap, err)
	}
	start = time.Now()
	restored := e.create(snapID, 256, true)
	t.Logf("memory restore took %v", time.Since(start))
	if restored.GetRestorePath() != ownsandv1.SandboxRestorePath_SANDBOX_RESTORE_PATH_MEMORY_STATE {
		t.Fatalf("restore path %v", restored.GetRestorePath())
	}
	if out, _ := e.exec(restored.GetSandboxId(), "/bin/cat", "/root/marker"); out != "state\n" {
		t.Fatalf("restored sandbox %q", out)
	}
	cold := e.create(snapID, 512, true)
	if cold.GetRestorePath() != ownsandv1.SandboxRestorePath_SANDBOX_RESTORE_PATH_DISK_COLD_BOOT {
		t.Fatalf("cold restore path %v", cold.GetRestorePath())
	}
	if out, _ := e.exec(cold.GetSandboxId(), "/bin/cat", "/root/marker"); out != "state\n" {
		t.Fatalf("cold-booted sandbox %q", out)
	}
	for _, id := range []string{sb.GetSandboxId(), restored.GetSandboxId(), cold.GetSandboxId()} {
		if en, err := m.Delete(context.Background(), &ownsandv1.DeleteSandboxCommand{SandboxId: id}); err != nil || en.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_DELETED {
			t.Fatalf("delete %v %v", en, err)
		}
	}
	if ids, _ := rt.Running(context.Background()); len(ids) != 0 {
		t.Fatalf("microVMs still run: %v", ids)
	}
}
