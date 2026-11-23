// SPDX-License-Identifier: FSL-1.1-ALv2

package fcrt_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
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
	return e.createSized(snapshotID, memMiB, 1, memoryRestore)
}

func (e *env) createSized(snapshotID string, memMiB, diskGiB int64, memoryRestore bool) *ownsandv1.SandboxStateEntry {
	e.t.Helper()
	id, _ := uuidv7.NewString()
	now := time.Now()
	cmd := &ownsandv1.CreateSandboxCommand{
		SandboxId:            id,
		Source:               &ownsandv1.CreateSandboxCommand_BaseImage{BaseImage: ownsandv1.BaseImage_BASE_IMAGE_LINUX},
		Resources:            &ownsandv1.Resources{VcpuCount: 1, MemoryMib: memMiB, DiskGib: diskGiB},
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

func fieldOr(fields []string, i int) string {
	if i < len(fields) {
		return fields[i]
	}
	return ""
}

// collectExec joins the output of an exec stream and returns its ExecExited
// event. A stream that has no exited event (it ended on a failure, on the end
// of the sandbox, on a pause or without any terminal event) and a stream with
// an error frame are errors: a nil *ExecExited would read as exit code 0.
func collectExec(frames []*ownsandv1.DataFrame) (string, *ownsandv1.ExecExited, error) {
	var out strings.Builder
	var exited *ownsandv1.ExecExited
	for _, f := range frames {
		if f.GetError() != nil {
			return out.String(), nil, fmt.Errorf("exec error frame: %v", f.GetError())
		}
		ev := f.GetExecEvent()
		out.Write(ev.GetStdout().GetData())
		out.Write(ev.GetStderr().GetData())
		if failed := ev.GetFailed(); failed != nil {
			return out.String(), nil, fmt.Errorf("exec failed: %v", failed)
		}
		if ended := ev.GetSandboxEnded(); ended != nil {
			return out.String(), nil, fmt.Errorf("sandbox ended during the exec: %v", ended)
		}
		if ex := ev.GetExited(); ex != nil {
			exited = ex
		}
	}
	if exited == nil {
		return out.String(), nil, fmt.Errorf("exec stream of %d frames ended without an exited event", len(frames))
	}
	return out.String(), exited, nil
}

// exec runs cmd in the sandbox and returns its output and its exited event,
// which is never nil: a stream without a terminal exited event fails the test.
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
	s.mu.Lock()
	defer s.mu.Unlock()
	out, exited, err := collectExec(s.frames)
	if err != nil {
		e.t.Fatalf("exec %v: %v (output %q)", cmd, err, out)
	}
	return out, exited
}

func execFrame(ev *ownsandv1.ExecEvent) *ownsandv1.DataFrame {
	return &ownsandv1.DataFrame{Payload: &ownsandv1.DataFrame_ExecEvent{ExecEvent: ev}}
}

func stdoutEvent(data string) *ownsandv1.ExecEvent {
	return &ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Stdout{Stdout: &ownsandv1.ExecOutput{Data: []byte(data)}}}
}

func exitedEvent(code int64) *ownsandv1.ExecEvent {
	return &ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Exited{Exited: &ownsandv1.ExecExited{Status: &ownsandv1.ExecExited_ExitCode{ExitCode: code}}}}
}

// collectExec needs no microVM: it must not read a stream without a terminal
// exited event as a success.
func TestCollectExec(t *testing.T) {
	t.Run("output with the exited event passes", func(t *testing.T) {
		out, ex, err := collectExec([]*ownsandv1.DataFrame{execFrame(stdoutEvent("hel")), execFrame(stdoutEvent("lo\n")), execFrame(exitedEvent(3))})
		if err != nil || ex == nil || ex.GetExitCode() != 3 || out != "hello\n" {
			t.Fatalf("out %q exited %v err %v", out, ex, err)
		}
	})
	t.Run("output that ends without a terminal event fails", func(t *testing.T) {
		if _, ex, err := collectExec([]*ownsandv1.DataFrame{execFrame(stdoutEvent("hello\n"))}); err == nil || ex != nil {
			t.Fatalf("stream without an exited event passed: %v %v", ex, err)
		}
	})
	t.Run("an empty stream fails", func(t *testing.T) {
		if _, _, err := collectExec(nil); err == nil {
			t.Fatal("empty stream passed")
		}
	})
	t.Run("a non-exited terminal event fails", func(t *testing.T) {
		for name, ev := range map[string]*ownsandv1.ExecEvent{
			"failed":        {Event: &ownsandv1.ExecEvent_Failed{Failed: &ownsandv1.ExecFailed{Message: "boom"}}},
			"sandbox ended": {Event: &ownsandv1.ExecEvent_SandboxEnded{SandboxEnded: &ownsandv1.ExecSandboxEnded{}}},
		} {
			if _, _, err := collectExec([]*ownsandv1.DataFrame{execFrame(stdoutEvent("x")), execFrame(ev)}); err == nil {
				t.Fatalf("%s stream passed", name)
			}
		}
	})
	t.Run("an error frame fails even after the exited event", func(t *testing.T) {
		errFrame := &ownsandv1.DataFrame{Payload: &ownsandv1.DataFrame_Error{Error: &ownsandv1.ErrorDetail{}}}
		if _, _, err := collectExec([]*ownsandv1.DataFrame{execFrame(exitedEvent(0)), errFrame}); err == nil {
			t.Fatal("stream with an error frame passed")
		}
	})
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
	if ex == nil || ex.GetExitCode() != 0 || !strings.Contains(out, "hello\nLinux\n") {
		t.Fatalf("exec output %q exited %v", out, ex)
	}
	t.Logf("guest df: %s", strings.TrimSpace(out))
	if _, err := m.Pause(context.Background(), &ownsandv1.PauseSandboxCommand{SandboxId: sb.GetSandboxId()}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Resume(context.Background(), &ownsandv1.ResumeSandboxCommand{SandboxId: sb.GetSandboxId()}); err != nil {
		t.Fatal(err)
	}
	if out, ex := e.exec(sb.GetSandboxId(), "/bin/cat", "/root/marker"); ex == nil || ex.GetExitCode() != 0 || out != "state\n" {
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
	if out, ex := e.exec(restored.GetSandboxId(), "/bin/cat", "/root/marker"); ex == nil || ex.GetExitCode() != 0 || out != "state\n" {
		t.Fatalf("restored sandbox %q", out)
	}
	cold := e.create(snapID, 512, true)
	if cold.GetRestorePath() != ownsandv1.SandboxRestorePath_SANDBOX_RESTORE_PATH_DISK_COLD_BOOT {
		t.Fatalf("cold restore path %v", cold.GetRestorePath())
	}
	if out, ex := e.exec(cold.GetSandboxId(), "/bin/cat", "/root/marker"); ex == nil || ex.GetExitCode() != 0 || out != "state\n" {
		t.Fatalf("cold-booted sandbox %q", out)
	}
	// A larger disk never restores the memory state: the sandbox boots the
	// snapshot disk cold and the guest sees the requested capacity.
	grown := e.createSized(snapID, 256, 2, true)
	if grown.GetRestorePath() != ownsandv1.SandboxRestorePath_SANDBOX_RESTORE_PATH_DISK_COLD_BOOT {
		t.Fatalf("larger-disk restore path %v", grown.GetRestorePath())
	}
	if out, _ := e.exec(grown.GetSandboxId(), "/bin/cat", "/root/marker"); out != "state\n" {
		t.Fatalf("larger-disk sandbox %q", out)
	}
	// The root file system of the 2 GiB disk is larger than the 1 GiB snapshot
	// disk could hold (df reports 1 KiB blocks in the second field).
	dfOut, _ := e.exec(grown.GetSandboxId(), "/bin/sh", "-c", "df -k / | tail -1")
	fields := strings.Fields(dfOut)
	if blocks, err := strconv.ParseInt(fieldOr(fields, 1), 10, 64); err != nil || blocks < 3<<19 {
		t.Fatalf("larger-disk root file system %q", dfOut)
	}
	// A base-image create with a larger disk grows the image file on the host
	// only; the guest helper grows the root file system online at boot.
	baseGrown := e.createSized("", 256, 2, false)
	if baseGrown.GetRestorePath() != ownsandv1.SandboxRestorePath_SANDBOX_RESTORE_PATH_BASE_IMAGE {
		t.Fatalf("larger-disk base-image restore path %v", baseGrown.GetRestorePath())
	}
	baseDf, _ := e.exec(baseGrown.GetSandboxId(), "/bin/sh", "-c", "df -k / | tail -1")
	baseFields := strings.Fields(baseDf)
	if blocks, err := strconv.ParseInt(fieldOr(baseFields, 1), 10, 64); err != nil || blocks < 3<<19 {
		t.Fatalf("larger-disk base-image root file system %q", baseDf)
	}
	for _, id := range []string{sb.GetSandboxId(), restored.GetSandboxId(), cold.GetSandboxId(), grown.GetSandboxId(), baseGrown.GetSandboxId()} {
		if en, err := m.Delete(context.Background(), &ownsandv1.DeleteSandboxCommand{SandboxId: id}); err != nil || en.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_DELETED {
			t.Fatalf("delete %v %v", en, err)
		}
	}
	if ids, _ := rt.Running(context.Background()); len(ids) != 0 {
		t.Fatalf("microVMs still run: %v", ids)
	}
}
