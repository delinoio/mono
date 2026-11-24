// SPDX-License-Identifier: FSL-1.1-ALv2

package hcsrt_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/images"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/ops"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/sandboxes"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime/hcsrt"
	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// envWindowsImage names a Windows image directory (disk.vhdx and
// image.json, as `ownsand-agent image build --iso` writes to
// images/windows) for the opt-in Windows guest integration test:
//
//	go test -c -o hcsrt.test.exe ./cmds/ownsand-agent/internal/vmruntime/hcsrt
//	$env:OWNSAND_HV_TEST_WINDOWS_IMAGE = "<state>\images\windows"
//	.\hcsrt.test.exe -test.v -test.run TestHyperVWindowsIntegration -test.timeout 60m
//
// OWNSAND_HV_TEST_STATE_DIR keeps the VM directories, as for the Linux test.
const envWindowsImage = "OWNSAND_HV_TEST_WINDOWS_IMAGE"

// windowsDiskGiB is larger than the default 32 GiB image, so the test sees
// the disk growth.
const windowsDiskGiB = 40

func windowsCommand(sandboxID, snapshotID string, vcpus int32, memMiB int64, network ownsandv1.NetworkMode) *ownsandv1.CreateSandboxCommand {
	now := time.Now()
	cmd := &ownsandv1.CreateSandboxCommand{
		SandboxId:            sandboxID,
		Source:               &ownsandv1.CreateSandboxCommand_BaseImage{BaseImage: ownsandv1.BaseImage_BASE_IMAGE_WINDOWS},
		Resources:            &ownsandv1.Resources{VcpuCount: vcpus, MemoryMib: memMiB, DiskGib: windowsDiskGiB},
		NetworkMode:          network,
		GuestOs:              ownsandv1.GuestOs_GUEST_OS_WINDOWS,
		IdleTtl:              durationpb.New(time.Hour),
		MaxTtl:               durationpb.New(time.Hour),
		CreateTime:           timestamppb.New(now),
		MaxDeadline:          timestamppb.New(now.Add(time.Hour)),
		Limits:               &ownsandv1.DeviceLimits{VcpuCount: 8, MemoryMib: 16384, DiskGib: 200, VcpuOvercommitPercent: 100},
		MemoryRestoreAllowed: true,
	}
	if snapshotID != "" {
		cmd.Source = &ownsandv1.CreateSandboxCommand_SnapshotId{SnapshotId: snapshotID}
	}
	return cmd
}

func TestHyperVWindowsIntegration(t *testing.T) {
	imageDir := os.Getenv(envWindowsImage)
	if imageDir == "" {
		t.Skipf("set %s and run the test binary elevated to boot real Windows VMs", envWindowsImage)
	}
	meta, err := images.ReadMetadata(imageDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("image %s, %d bytes, minimum %d vCPUs %d MiB", meta.Source, meta.DiskBytes, meta.MinimumCPUCount, meta.MinimumMemoryMiB)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	if os.Getenv("OWNSAND_TEST_VERBOSE") != "" {
		logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	stateDir := os.Getenv(envStateDir)
	if stateDir == "" {
		stateDir = t.TempDir()
	} else if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	vmsDir := st.Path(store.VMsDir)
	rt, err := hcsrt.New(hcsrt.Options{WindowsImage: imageDir, VMsDir: vmsDir, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	caps := rt.Capabilities(context.Background())
	windowsCheck := false
	for _, c := range caps.Checks {
		t.Logf("check %s passed=%v: %s", c.GetCheckId(), c.GetPassed(), c.GetDetail())
		if c.GetCheckId() == "windows-base-image" && c.GetPassed() {
			windowsCheck = true
		}
	}
	if !windowsCheck || len(caps.BaseImages) != 1 || caps.BaseImages[0] != ownsandv1.BaseImage_BASE_IMAGE_WINDOWS {
		t.Fatalf("capabilities %v %v", caps.BaseImages, caps.Checks)
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
		if t.Failed() {
			consoleTail(t, vmsDir)
		}
		d.Close()
		m.Close()
		rt.Close()
	})
	e := &env{t: t, m: m, d: d}
	vcpus, mem := max(meta.MinimumCPUCount, 2), max(meta.MinimumMemoryMiB, 4096)
	create := func(snapshotID string, network ownsandv1.NetworkMode) (*ownsandv1.SandboxStateEntry, error) {
		id, _ := uuidv7.NewString()
		return m.Create(context.Background(), windowsCommand(id, snapshotID, vcpus, mem, network))
	}

	// OUTBOUND Windows sandboxes and resources below the image minimums are
	// rejected before any VM starts.
	if _, err := create("", ownsandv1.NetworkMode_NETWORK_MODE_OUTBOUND); err == nil || !strings.Contains(err.Error(), "could not start") {
		t.Fatalf("OUTBOUND Windows create: %v", err)
	}
	id, _ := uuidv7.NewString()
	if _, err := m.Create(context.Background(), windowsCommand(id, "", vcpus, meta.MinimumMemoryMiB-2, ownsandv1.NetworkMode_NETWORK_MODE_NONE)); err == nil {
		t.Fatal("a create below the memory minimum succeeded")
	}
	// A disk request below the virtual size of the image fails as a failed
	// precondition before any VM starts, and its VM directory is removed.
	vmDirsBefore, _ := os.ReadDir(vmsDir)
	id, _ = uuidv7.NewString()
	small := windowsCommand(id, "", vcpus, mem, ownsandv1.NetworkMode_NETWORK_MODE_NONE)
	small.Resources.DiskGib = 1
	if sb, _ := m.Create(context.Background(), small); sb.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_FAILED {
		t.Fatalf("a create below the image disk size: %v", sb)
	}
	if vmDirsAfter, _ := os.ReadDir(vmsDir); len(vmDirsAfter) != len(vmDirsBefore) {
		t.Fatalf("the failed create left a VM directory: %d entries before, %d after", len(vmDirsBefore), len(vmDirsAfter))
	}

	start := time.Now()
	sb, err := create("", ownsandv1.NetworkMode_NETWORK_MODE_NONE)
	if err != nil || sb.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		t.Fatalf("create %v %v", sb, err)
	}
	t.Logf("cold boot took %v", time.Since(start))
	if sb.GetRestorePath() != ownsandv1.SandboxRestorePath_SANDBOX_RESTORE_PATH_BASE_IMAGE {
		t.Fatalf("sandbox %v", sb)
	}
	checkDiskSize := func(sandboxID string) {
		t.Helper()
		out, ex := e.exec(sandboxID, "powershell.exe", "-NoProfile", "-Command", "(Get-Disk -Number (Get-Partition -DriveLetter C).DiskNumber).Size")
		size, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
		if ex == nil || ex.GetExitCode() != 0 || err != nil || size != windowsDiskGiB<<30 {
			t.Fatalf("sandbox %s disk bytes = %q, want %d", sandboxID, out, windowsDiskGiB<<30)
		}
	}
	checkDiskSize(sb.GetSandboxId())
	out, ex := e.exec(sb.GetSandboxId(), "cmd.exe", "/c", "echo hello")
	if !execSucceeded(ex) || out != "hello\r\n" {
		t.Fatalf("exec output %q exited %q", out, ex)
	}
	if _, ex := e.exec(sb.GetSandboxId(), "cmd.exe", "/c", "exit 7"); ex.GetExitCode() != 7 {
		t.Fatalf("exit code %v", ex)
	}
	out, _ = e.exec(sb.GetSandboxId(), "cmd.exe", "/c", "ver & hostname & whoami")
	t.Logf("guest: %s", strings.TrimSpace(out))
	if !strings.Contains(strings.ToLower(out), "sandbox") || !strings.Contains(strings.ToLower(out), `nt authority\system`) {
		t.Fatalf("guest identity %q", out)
	}
	// The answer file with the administrator password is gone, and the
	// guest helper runs as the registered auto-start LocalSystem service.
	if out, _ := e.exec(sb.GetSandboxId(), "cmd.exe", "/c", `if exist C:\Windows\Panther\unattend.xml (echo present) else (echo absent)`); out != "absent\r\n" {
		t.Fatalf("answer file %q", out)
	}
	if out, _ := e.exec(sb.GetSandboxId(), "sc.exe", "qc", "ownsand-guest"); !strings.Contains(out, "AUTO_START") || !strings.Contains(out, "LocalSystem") {
		t.Fatalf("service configuration %q", out)
	}
	if out, ex := e.exec(sb.GetSandboxId(), "cmd.exe", "/c", `echo state> C:\marker.txt`); !execSucceeded(ex) {
		t.Fatalf("write marker %q exited %q", out, ex)
	}
	if out, _ := e.exec(sb.GetSandboxId(), "cmd.exe", "/c", `type C:\marker.txt`); out != "state\r\n" {
		t.Fatalf("read marker %q", out)
	}
	// The guest helper extends C: to the grown disk at start.
	var size int64
	deadline := time.Now().Add(5 * time.Minute)
	for {
		out, _ := e.exec(sb.GetSandboxId(), "powershell.exe", "-NoProfile", "-Command", "(Get-Partition -DriveLetter C).Size")
		size, _ = strconv.ParseInt(strings.TrimSpace(out), 10, 64)
		if size > (windowsDiskGiB-2)<<30 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Second)
	}
	if size <= (windowsDiskGiB-2)<<30 {
		t.Fatalf("C: did not grow to the %d GiB disk: %d bytes", windowsDiskGiB, size)
	}
	t.Logf("C: is %d bytes after %v", size, time.Since(start))
	// No network adapter in a NONE sandbox.
	if out, _ := e.exec(sb.GetSandboxId(), "powershell.exe", "-NoProfile", "-Command", "@(Get-NetAdapter).Count"); strings.TrimSpace(out) != "0" {
		t.Fatalf("NONE Windows sandbox has network adapters: %q", out)
	}

	start = time.Now()
	if _, err := m.Pause(context.Background(), &ownsandv1.PauseSandboxCommand{SandboxId: sb.GetSandboxId()}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Resume(context.Background(), &ownsandv1.ResumeSandboxCommand{SandboxId: sb.GetSandboxId()}); err != nil {
		t.Fatal(err)
	}
	t.Logf("pause and resume took %v", time.Since(start))
	if out, _ := e.exec(sb.GetSandboxId(), "cmd.exe", "/c", `type C:\marker.txt`); out != "state\r\n" {
		t.Fatalf("after resume %q", out)
	}
	snapID, _ := uuidv7.NewString()
	start = time.Now()
	snap, err := m.CreateSnapshot(context.Background(), &ownsandv1.CreateSnapshotCommand{SnapshotId: snapID, SandboxId: sb.GetSandboxId()})
	if err != nil {
		t.Fatalf("snapshot %v %v", snap, err)
	}
	t.Logf("snapshot took %v (memory state: %v, %d bytes)", time.Since(start), snap.GetHasMemoryState(), snap.GetSizeBytes())
	if snap.GetHasMemoryState() {
		t.Fatal("Hyper-V snapshots are disk-only in this version")
	}
	if out, _ := e.exec(sb.GetSandboxId(), "cmd.exe", "/c", `type C:\marker.txt`); out != "state\r\n" {
		t.Fatalf("after snapshot %q", out)
	}
	start = time.Now()
	cold, err := create(snapID, ownsandv1.NetworkMode_NETWORK_MODE_NONE)
	if err != nil {
		t.Fatalf("create from snapshot: %v", err)
	}
	t.Logf("create from snapshot took %v (%v)", time.Since(start), cold.GetRestorePath())
	if cold.GetRestorePath() != ownsandv1.SandboxRestorePath_SANDBOX_RESTORE_PATH_DISK_COLD_BOOT {
		t.Fatalf("restore path %v", cold.GetRestorePath())
	}
	checkDiskSize(cold.GetSandboxId())
	if out, _ := e.exec(cold.GetSandboxId(), "cmd.exe", "/c", `type C:\marker.txt`); out != "state\r\n" {
		t.Fatalf("sandbox from snapshot %q", out)
	}

	for _, id := range []string{sb.GetSandboxId(), cold.GetSandboxId()} {
		if en, err := m.Delete(context.Background(), &ownsandv1.DeleteSandboxCommand{SandboxId: id}); err != nil || en.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_DELETED {
			t.Fatalf("delete %v %v", en, err)
		}
	}
	// The inventory must succeed: a failed enumeration is not proof that no VM runs.
	ids, err := rt.Running(context.Background())
	if err != nil {
		t.Fatalf("list running VMs: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("VMs still run: %v", ids)
	}
}
