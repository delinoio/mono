// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build darwin && cgo && arm64

package vzrt_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/images"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/ops"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/sandboxes"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime/vzrt"
	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// envMacOSImage names a macOS base image directory (images/macos of a state
// directory after `ownsand-agent image build`) for the opt-in macOS guest
// integration test. Sign the test binary as for TestVirtualizationIntegration.
// OWNSAND_VZ_TEST_OUTBOUND=1 also runs the OUTBOUND checks.
const envMacOSImage = "OWNSAND_VZ_TEST_MACOS_IMAGE"

// macOS guest resources of the test: the restore image minimum is 2 vCPUs
// and 4 GiB; the disk is larger than the 64 GiB image to cover the growth.
const (
	macVCPUs   = 2
	macMemMiB  = 4096
	macDiskGiB = 72
)

func (e *env) createMacOS(snapshotID string, network ownsandv1.NetworkMode, memoryRestore bool) (*ownsandv1.SandboxStateEntry, error) {
	e.t.Helper()
	id, _ := uuidv7.NewString()
	now := time.Now()
	cmd := &ownsandv1.CreateSandboxCommand{
		SandboxId:            id,
		Source:               &ownsandv1.CreateSandboxCommand_BaseImage{BaseImage: ownsandv1.BaseImage_BASE_IMAGE_MACOS},
		Resources:            &ownsandv1.Resources{VcpuCount: macVCPUs, MemoryMib: macMemMiB, DiskGib: macDiskGiB},
		NetworkMode:          network,
		GuestOs:              ownsandv1.GuestOs_GUEST_OS_MACOS,
		IdleTtl:              durationpb.New(time.Hour),
		MaxTtl:               durationpb.New(time.Hour),
		CreateTime:           timestamppb.New(now),
		MaxDeadline:          timestamppb.New(now.Add(time.Hour)),
		Limits:               &ownsandv1.DeviceLimits{VcpuCount: 16, MemoryMib: 32768, DiskGib: 512, VcpuOvercommitPercent: 100},
		MemoryRestoreAllowed: memoryRestore,
	}
	if snapshotID != "" {
		cmd.Source = &ownsandv1.CreateSandboxCommand_SnapshotId{SnapshotId: snapshotID}
	}
	return e.m.Create(context.Background(), cmd)
}

func (e *env) mustCreateMacOS(snapshotID string, network ownsandv1.NetworkMode, memoryRestore bool) *ownsandv1.SandboxStateEntry {
	e.t.Helper()
	start := time.Now()
	entry, err := e.createMacOS(snapshotID, network, memoryRestore)
	if err != nil || entry.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		e.t.Fatalf("create macOS sandbox: %v %v", entry, err)
	}
	e.t.Logf("macOS sandbox %s ready in %v (%v)", entry.GetSandboxId(), time.Since(start), entry.GetRestorePath())
	return entry
}

// waitOutput runs cmd until its output contains want or the timeout passes.
func (e *env) waitOutput(sandboxID, want string, timeout time.Duration, cmd ...string) string {
	e.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		out, _ := e.exec(sandboxID, cmd...)
		if strings.Contains(out, want) || time.Now().After(deadline) {
			return out
		}
		time.Sleep(2 * time.Second)
	}
}

// newMacOSEnv starts a runtime, a sandbox manager and an operation
// dispatcher on the macOS image directory.
func newMacOSEnv(t *testing.T, image string) (*env, vmruntime.Runtime) {
	t.Helper()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	if os.Getenv("OWNSAND_TEST_VERBOSE") != "" {
		logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, err := vzrt.New(vzrt.Options{MacOSImage: image, VMsDir: st.Path(store.VMsDir), Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	caps := r.Capabilities(context.Background())
	hasImage := false
	for _, b := range caps.BaseImages {
		hasImage = hasImage || b == ownsandv1.BaseImage_BASE_IMAGE_MACOS
	}
	if !caps.MacOSGuestsSupported || !hasImage || len(caps.MacOSHardwareModels) != 1 {
		t.Fatalf("capabilities %+v %+v", caps, caps.Checks)
	}
	m, err := sandboxes.New(context.Background(), sandboxes.Options{Store: st, Runtime: r, Logger: logger})
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
		r.Close()
	})
	e := &env{t: t, m: m, d: d, st: st}
	return e, r
}

// cloneMacOSImageForIntegration keeps the supplied image intact while the
// test replaces and removes its local copy of the metadata.
func cloneMacOSImageForIntegration(t *testing.T, source string) (string, images.Metadata) {
	t.Helper()
	meta, err := images.ReadMetadata(source)
	if err != nil {
		t.Fatal(err)
	}
	image := t.TempDir()
	for _, name := range []string{images.MacOSDisk, images.MacOSAuxStorage, images.MacOSHardwareModel} {
		if err := unix.Clonefile(filepath.Join(source, name), filepath.Join(image, name), 0); err != nil {
			t.Fatalf("clone base image file %s (set TMPDIR to the image's APFS volume): %v", name, err)
		}
	}
	if err := images.WriteMetadata(image, meta); err != nil {
		t.Fatal(err)
	}
	return image, meta
}

func TestVirtualizationMacOSIntegration(t *testing.T) {
	image := os.Getenv(envMacOSImage)
	if image == "" {
		t.Skipf("set %s to a built macOS image directory and sign the test binary", envMacOSImage)
	}
	image, original := cloneMacOSImageForIntegration(t, image)
	e, r := newMacOSEnv(t, image)
	m := e.m
	deleteSandbox := func(id string) {
		t.Helper()
		if en, err := m.Delete(context.Background(), &ownsandv1.DeleteSandboxCommand{SandboxId: id}); err != nil || en.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_DELETED {
			t.Fatalf("delete %v %v", en, err)
		}
	}

	sb := e.mustCreateMacOS("", ownsandv1.NetworkMode_NETWORK_MODE_NONE, false)
	if sb.GetRestorePath() != ownsandv1.SandboxRestorePath_SANDBOX_RESTORE_PATH_BASE_IMAGE {
		t.Fatalf("sandbox %v", sb)
	}
	out, ex := e.exec(sb.GetSandboxId(), "/bin/sh", "-c", "sw_vers -productName; uname -sm; id -u; echo state > /var/root/marker")
	if !execSucceeded(ex) || !strings.Contains(out, "macOS\nDarwin arm64\n0\n") {
		t.Fatalf("exec output %q exited %q", out, ex)
	}
	// The guest helper grows the APFS container to the 72 GiB disk after
	// the boot; the container size is in bytes.
	out = e.waitOutput(sb.GetSandboxId(), "grown", 3*time.Minute, "/bin/sh", "-c",
		"s=$(diskutil info -plist / | plutil -extract APFSContainerSize raw -); [ \"$s\" -gt "+strconv.FormatInt(70<<30, 10)+" ] && echo grown $s || echo small $s")
	if !strings.Contains(out, "grown") {
		t.Fatalf("APFS container did not grow: %q", out)
	}
	// A NONE sandbox has no virtio network device, which would carry the
	// macOS guest MAC address.
	if out, _ := e.exec(sb.GetSandboxId(), "/sbin/ifconfig", "-a"); strings.Contains(out, vzrt.MacOSGuestMAC.String()) {
		t.Fatalf("NONE sandbox has a network device: %q", out)
	}
	if _, err := m.Pause(context.Background(), &ownsandv1.PauseSandboxCommand{SandboxId: sb.GetSandboxId()}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Resume(context.Background(), &ownsandv1.ResumeSandboxCommand{SandboxId: sb.GetSandboxId()}); err != nil {
		t.Fatal(err)
	}
	if out, _ := e.exec(sb.GetSandboxId(), "/bin/cat", "/var/root/marker"); out != "state\n" {
		t.Fatalf("after resume %q", out)
	}
	snapID, _ := uuidv7.NewString()
	start := time.Now()
	snap, err := m.CreateSnapshot(context.Background(), &ownsandv1.CreateSnapshotCommand{SnapshotId: snapID, SandboxId: sb.GetSandboxId()})
	if err != nil {
		t.Fatalf("snapshot %v %v", snap, err)
	}
	t.Logf("snapshot took %v, memory state %v", time.Since(start), snap.GetHasMemoryState())
	if len(snap.GetMacosHardwareModel()) == 0 {
		t.Fatal("macOS snapshot has no hardware model")
	}
	if snap.GetHasMemoryState() {
		e.requireCanceledSnapshot(sb.GetSandboxId(), "vm.state", images.MacOSDisk)
	}
	e.requireCanceledSnapshot(sb.GetSandboxId(), images.MacOSDisk, images.MacOSAuxStorage)
	if out, ex := e.exec(sb.GetSandboxId(), "/bin/cat", "/var/root/marker"); out != "state\n" || ex.GetExitCode() != 0 {
		t.Fatalf("source did not resume after cancellation: %q %v", out, ex)
	}

	// The source still runs with the snapshot's machine identifier, so a
	// create from the snapshot boots its disk cold with a new identifier.
	cold := e.mustCreateMacOS(snapID, ownsandv1.NetworkMode_NETWORK_MODE_NONE, true)
	if cold.GetRestorePath() != ownsandv1.SandboxRestorePath_SANDBOX_RESTORE_PATH_DISK_COLD_BOOT {
		t.Fatalf("restore path while the source runs %v", cold.GetRestorePath())
	}
	if out, _ := e.exec(cold.GetSandboxId(), "/bin/cat", "/var/root/marker"); out != "state\n" {
		t.Fatalf("cold-booted sandbox %q", out)
	}
	// Two macOS guests run now; the manager rejects a third.
	if entry, err := e.createMacOS("", ownsandv1.NetworkMode_NETWORK_MODE_NONE, false); err == nil {
		t.Fatalf("third macOS guest started: %v", entry)
	}
	for _, id := range []string{sb.GetSandboxId(), cold.GetSandboxId()} {
		deleteSandbox(id)
	}
	t.Run("cancel-during-restored-readiness", func(t *testing.T) {
		e.requireCanceledRestore(t, snapID, ownsandv1.GuestOs_GUEST_OS_MACOS)
	})
	// With the source gone, the memory state restores with the snapshot's
	// identifier.
	coldPath := ownsandv1.SandboxRestorePath_SANDBOX_RESTORE_PATH_DISK_COLD_BOOT
	eligiblePath := coldPath
	if snap.GetHasMemoryState() {
		eligiblePath = ownsandv1.SandboxRestorePath_SANDBOX_RESTORE_PATH_MEMORY_STATE
	}
	checkRestore := func(memoryRestore bool, want ownsandv1.SandboxRestorePath) {
		t.Helper()
		restored := e.mustCreateMacOS(snapID, ownsandv1.NetworkMode_NETWORK_MODE_NONE, memoryRestore)
		if got := restored.GetRestorePath(); got != want {
			t.Fatalf("restore path %v, want %v", got, want)
		}
		if out, ex := e.exec(restored.GetSandboxId(), "/bin/cat", "/var/root/marker"); out != "state\n" || !execSucceeded(ex) {
			t.Fatalf("restored sandbox output %q, exit %v", out, ex)
		}
		deleteSandbox(restored.GetSandboxId())
	}
	t.Log("restore with original base image metadata")
	checkRestore(false, coldPath)
	checkRestore(true, eligiblePath)

	id, _ := uuidv7.NewString()
	spec := vmruntime.Spec{
		SandboxID:   id,
		Dir:         filepath.Join(t.TempDir(), id),
		Resources:   &ownsandv1.Resources{VcpuCount: macVCPUs, MemoryMib: macMemMiB, DiskGib: macDiskGiB},
		NetworkMode: ownsandv1.NetworkMode_NETWORK_MODE_NONE,
		GuestOS:     ownsandv1.GuestOs_GUEST_OS_MACOS,
		BaseImage:   ownsandv1.BaseImage_BASE_IMAGE_MACOS,
	}
	for _, tc := range []struct {
		name   string
		cpu    int32
		memory int64
	}{
		{name: "CPU", cpu: macVCPUs + 1, memory: macMemMiB},
		{name: "memory", cpu: macVCPUs, memory: macMemMiB + 1},
	} {
		meta := original
		meta.MinimumCPUCount, meta.MinimumMemoryMiB = tc.cpu, tc.memory
		if err := images.WriteMetadata(image, meta); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Create(context.Background(), spec); !errors.Is(err, vmruntime.ErrUnsupported) || !strings.Contains(err.Error(), "macOS guests need at least") {
			t.Fatalf("base image create below %s minimum: %v", tc.name, err)
		}
	}
	t.Log("base image create rejected below each recorded minimum")

	replacement := original
	replacement.MinimumCPUCount, replacement.MinimumMemoryMiB = macVCPUs+1, macMemMiB+1
	if err := images.WriteMetadata(image, replacement); err != nil {
		t.Fatal(err)
	}
	t.Log("restore with higher base image minimums")
	checkRestore(false, coldPath)
	checkRestore(true, eligiblePath)

	if err := os.Remove(filepath.Join(image, images.MetadataFile)); err != nil {
		t.Fatal(err)
	}
	t.Log("restore without base image metadata")
	checkRestore(false, coldPath)
	checkRestore(true, eligiblePath)

	invalidSnapshot := t.TempDir()
	if err := os.WriteFile(filepath.Join(invalidSnapshot, images.MacOSHardwareModel), []byte("invalid model"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec.BaseImage = ownsandv1.BaseImage_BASE_IMAGE_UNSPECIFIED
	spec.Snapshot = &vmruntime.SnapshotSource{Dir: invalidSnapshot}
	if _, err := r.Create(context.Background(), spec); !errors.Is(err, vmruntime.ErrUnsupported) || !strings.Contains(err.Error(), "does not support the hardware model") {
		t.Fatalf("snapshot create with unsupported hardware model: %v", err)
	}
	if _, err := os.Stat(spec.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsupported hardware model reached disk preparation: %v", err)
	}
	t.Log("snapshot hardware model rejected before disk preparation")

	snapshotDir, _, ok := m.SnapshotContent(snapID)
	if !ok {
		t.Fatal("snapshot content missing")
	}
	spec.Snapshot.Dir = snapshotDir
	spec.Resources.VcpuCount = math.MaxInt32
	if _, err := r.Create(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "invalid macOS VM configuration") {
		t.Fatalf("snapshot create with invalid native configuration: %v", err)
	}
	t.Log("invalid snapshot VM configuration rejected by Virtualization.framework")

	if snap.GetHasMemoryState() {
		t.Log("restore after invalidating memory state")
		e.invalidateMemoryState(snapID)
		checkRestore(true, coldPath)
	}

	if ids, _ := r.Running(context.Background()); len(ids) != 0 {
		t.Fatalf("VMs still run: %v", ids)
	}
}

// TestVirtualizationMacOSOutbound checks that an OUTBOUND macOS guest
// reaches the internet through the agent's network stack and cannot reach
// the host LAN, the link-local range or a private network.
func TestVirtualizationMacOSOutbound(t *testing.T) {
	image := os.Getenv(envMacOSImage)
	if image == "" || os.Getenv(envOutbound) == "" {
		t.Skipf("set %s and %s=1 and sign the test binary", envMacOSImage, envOutbound)
	}
	e, _ := newMacOSEnv(t, image)
	ob := e.mustCreateMacOS("", ownsandv1.NetworkMode_NETWORK_MODE_OUTBOUND, false)
	// The guest configures its interface with DHCP from the agent's stack.
	out := e.waitOutput(ob.GetSandboxId(), "INTERNET_OK", 2*time.Minute, "/bin/sh", "-c",
		"/usr/bin/curl -s -m 10 -o /dev/null http://example.com/ && echo INTERNET_OK")
	if !strings.Contains(out, "INTERNET_OK") {
		diag, _ := e.exec(ob.GetSandboxId(), "/bin/sh", "-c",
			"/sbin/ifconfig -a; /usr/sbin/networksetup -listallhardwareports; /usr/sbin/ipconfig getpacket en0; /usr/sbin/scutil --dns | head -20; /usr/bin/curl -sS -m 10 -o /dev/null http://example.com/ 2>&1; /usr/bin/curl -sS -m 10 -o /dev/null http://93.184.215.14/ 2>&1")
		t.Fatalf("OUTBOUND macOS sandbox cannot reach the internet: %q\n%s", out, diag)
	}
	lan := hostLANAddress()
	port := startHostServer(t, lan, "127.0.0.1")
	for _, target := range []string{lan, "169.254.169.254", "10.0.0.1"} {
		if target == "" {
			continue
		}
		out, _ := e.exec(ob.GetSandboxId(), "/bin/sh", "-c", "/usr/bin/curl -s -m 3 -o /dev/null http://"+target+"/ && echo REACHED || echo BLOCKED")
		if !strings.Contains(out, "BLOCKED") {
			t.Fatalf("OUTBOUND macOS sandbox reached %s: %q", target, out)
		}
	}
	// The host server answers on the host's own addresses: the guest must
	// not reach it there (127.0.0.1 in the guest is the guest itself).
	if lan != "" {
		out, _ := e.exec(ob.GetSandboxId(), "/bin/sh", "-c", "/usr/bin/curl -s -m 3 http://"+lan+":"+port+"/")
		if strings.Contains(out, hostServerMarker) {
			t.Fatalf("OUTBOUND macOS sandbox reached the host server at %s:%s: %q", lan, port, out)
		}
	}
	if en, err := e.m.Delete(context.Background(), &ownsandv1.DeleteSandboxCommand{SandboxId: ob.GetSandboxId()}); err != nil || en.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_DELETED {
		t.Fatalf("delete %v %v", en, err)
	}
}
