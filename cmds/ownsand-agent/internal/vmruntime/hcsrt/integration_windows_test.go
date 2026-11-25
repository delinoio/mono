// SPDX-License-Identifier: FSL-1.1-ALv2

package hcsrt_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
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
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime/hcsrt"
	"github.com/delinoio/mono/go/ownsand/datapath"
	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// Environment of the opt-in integration test. It boots real Hyper-V VMs,
// so the test binary must run elevated on a host with Hyper-V enabled:
//
//	go test -c -o hcsrt.test.exe ./cmds/ownsand-agent/internal/vmruntime/hcsrt
//	$env:OWNSAND_HV_TEST_KERNEL = "...\vmlinux"
//	$env:OWNSAND_HV_TEST_ROOTFS = "...\rootfs.ext4"
//	.\hcsrt.test.exe -test.v -test.run TestHyperVIntegration
//
// The kernel and the root file system come from
// cmds/ownsand-agent/images/linux/build-image.sh for amd64.
// OWNSAND_HV_TEST_OUTBOUND=1 also runs the OUTBOUND network policy checks,
// which need internet access. OWNSAND_HV_TEST_LAN_ADDR names the host LAN
// address to probe (default: the first non-loopback IPv4 address).
// OWNSAND_HV_TEST_STATE_DIR keeps the agent state (VM directories with
// console.log) in a directory instead of a temporary one.
const (
	envKernel   = "OWNSAND_HV_TEST_KERNEL"
	envRootfs   = "OWNSAND_HV_TEST_ROOTFS"
	envOutbound = "OWNSAND_HV_TEST_OUTBOUND"
	envLANAddr  = "OWNSAND_HV_TEST_LAN_ADDR"
	envStateDir = "OWNSAND_HV_TEST_STATE_DIR"
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

func (e *env) create(snapshotID string, memMiB int64, network ownsandv1.NetworkMode) *ownsandv1.SandboxStateEntry {
	e.t.Helper()
	entry, err := e.m.Create(context.Background(), createCommand(snapshotID, memMiB, 2, network))
	if err != nil {
		e.t.Fatalf("create: %v", err)
	}
	if entry.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		e.t.Fatalf("create: sandbox %v", entry)
	}
	return entry
}

func createCommand(snapshotID string, memMiB, diskGiB int64, network ownsandv1.NetworkMode) *ownsandv1.CreateSandboxCommand {
	id, _ := uuidv7.NewString()
	now := time.Now()
	cmd := &ownsandv1.CreateSandboxCommand{
		SandboxId:            id,
		Source:               &ownsandv1.CreateSandboxCommand_BaseImage{BaseImage: ownsandv1.BaseImage_BASE_IMAGE_LINUX},
		Resources:            &ownsandv1.Resources{VcpuCount: 2, MemoryMib: memMiB, DiskGib: diskGiB},
		NetworkMode:          network,
		GuestOs:              ownsandv1.GuestOs_GUEST_OS_LINUX,
		IdleTtl:              durationpb.New(time.Hour),
		MaxTtl:               durationpb.New(time.Hour),
		CreateTime:           timestamppb.New(now),
		MaxDeadline:          timestamppb.New(now.Add(time.Hour)),
		Limits:               &ownsandv1.DeviceLimits{VcpuCount: 8, MemoryMib: 8192, DiskGib: 16, VcpuOvercommitPercent: 100},
		MemoryRestoreAllowed: true,
	}
	if snapshotID != "" {
		cmd.Source = &ownsandv1.CreateSandboxCommand_SnapshotId{SnapshotId: snapshotID}
	}
	return cmd
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

func execSucceeded(exited *ownsandv1.ExecExited) bool {
	code, ok := exited.GetStatus().(*ownsandv1.ExecExited_ExitCode)
	return ok && code != nil && code.ExitCode == 0
}

// hostLANAddress returns an IPv4 address of a host interface that is not
// loopback or link-local, which the OUTBOUND policy must block.
func hostLANAddress() string {
	if a := os.Getenv(envLANAddr); a != "" {
		return a
	}
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLoopback() && !ipn.IP.IsLinkLocalUnicast() {
			return ipn.IP.String()
		}
	}
	return ""
}

// logFailedVMConsoles logs the end of each VM console after a test failure.
// Defer it so it runs before the resource cleanups, even if setup calls Fatal.
func logFailedVMConsoles(t *testing.T, vmsDir string) {
	if !t.Failed() {
		return
	}
	logs, _ := filepath.Glob(filepath.Join(vmsDir, "*", "console.log"))
	for _, p := range logs {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if len(b) > 4096 {
			b = b[len(b)-4096:]
		}
		t.Logf("%s:\n%s", p, b)
	}
}

func TestHyperVIntegration(t *testing.T) {
	testHyperVIntegration(t, false)
}

func TestHyperVLinuxBaseDiskSizing(t *testing.T) {
	testHyperVIntegration(t, true)
}

func testHyperVIntegration(t *testing.T, diskSizing bool) {
	kernel, rootfs := os.Getenv(envKernel), os.Getenv(envRootfs)
	if kernel == "" || rootfs == "" {
		t.Skipf("set %s and %s and run the test binary elevated to boot real VMs", envKernel, envRootfs)
	}
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
	rt, err := hcsrt.New(hcsrt.Options{Kernel: kernel, Rootfs: rootfs, VMsDir: vmsDir, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rt.Close() })
	defer logFailedVMConsoles(t, vmsDir)
	caps := rt.Capabilities(context.Background())
	for _, c := range caps.Checks {
		t.Logf("check %s passed=%v: %s", c.GetCheckId(), c.GetPassed(), c.GetDetail())
	}
	if len(caps.BaseImages) != 1 || caps.BaseImages[0] != ownsandv1.BaseImage_BASE_IMAGE_LINUX || len(caps.GuestOSes) != 2 {
		t.Fatalf("capabilities %+v", caps.Checks)
	}
	m, err := sandboxes.New(context.Background(), sandboxes.Options{Store: st, Runtime: rt, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	d, err := ops.New(m, st, ops.Options{Logger: logger})
	// A persistence failure can return both a dispatcher and an error.
	if d != nil {
		t.Cleanup(d.Close)
	}
	if err != nil {
		t.Fatal(err)
	}
	m.SetExecTracker(d)
	e := &env{t: t, m: m, d: d}
	if diskSizing {
		e.checkBaseDiskSizing(rt, rootfs, vmsDir)
		return
	}
	checkDiskSize := func(sandboxID string) {
		t.Helper()
		out, ex := e.exec(sandboxID, "/bin/cat", "/sys/block/sda/size")
		sectors, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
		if ex == nil || ex.GetExitCode() != 0 || err != nil || sectors != (2<<30)/512 {
			t.Fatalf("sandbox %s disk sectors = %q, want %d", sandboxID, out, (2<<30)/512)
		}
	}

	start := time.Now()
	sb := e.create("", 512, ownsandv1.NetworkMode_NETWORK_MODE_NONE)
	t.Logf("cold boot took %v", time.Since(start))
	if sb.GetRestorePath() != ownsandv1.SandboxRestorePath_SANDBOX_RESTORE_PATH_BASE_IMAGE {
		t.Fatalf("sandbox %v", sb)
	}
	checkDiskSize(sb.GetSandboxId())
	out, ex := e.exec(sb.GetSandboxId(), "/bin/sh", "-c", "echo hello; uname -sm; echo state > /root/marker; df -k / | tail -1; ls /sys/class/net")
	if !execSucceeded(ex) || !strings.Contains(out, "hello\nLinux x86_64\n") {
		t.Fatalf("exec output %q exited %q", out, ex)
	}
	t.Logf("guest: %s", strings.TrimSpace(out))
	// The runtime copies the COM1 output (the kernel console) of the VM to
	// console.log as a client of the port's named pipe.
	if b, err := os.ReadFile(filepath.Join(vmsDir, sb.GetSandboxId(), "console.log")); err != nil || !strings.Contains(string(b), "Linux version") {
		t.Fatalf("console.log has no kernel output (%d bytes): %v", len(b), err)
	}
	// The root file system grew to the 2 GiB disk at boot (the base image
	// is 512 MiB): more than 1.5 GiB in 1 KiB blocks.
	fields := strings.Fields(strings.Split(out, "\n")[2])
	if len(fields) < 2 {
		t.Fatalf("df output %q", out)
	}
	if blocks, err := strconv.ParseInt(fields[1], 10, 64); err != nil || blocks < 1536*1024 {
		t.Fatalf("root file system did not grow: %q", out)
	}
	// A NONE sandbox has no network device.
	if strings.Contains(out, "eth0") {
		t.Fatalf("NONE sandbox has a network device: %q", out)
	}
	start = time.Now()
	if _, err := m.Pause(context.Background(), &ownsandv1.PauseSandboxCommand{SandboxId: sb.GetSandboxId()}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Resume(context.Background(), &ownsandv1.ResumeSandboxCommand{SandboxId: sb.GetSandboxId()}); err != nil {
		t.Fatal(err)
	}
	t.Logf("pause and resume took %v", time.Since(start))
	if out, _ := e.exec(sb.GetSandboxId(), "/bin/cat", "/root/marker"); out != "state\n" {
		t.Fatalf("after resume %q", out)
	}
	snapID, _ := uuidv7.NewString()
	start = time.Now()
	snap, err := m.CreateSnapshot(context.Background(), &ownsandv1.CreateSnapshotCommand{SnapshotId: snapID, SandboxId: sb.GetSandboxId()})
	if err != nil {
		t.Fatalf("snapshot %v %v", snap, err)
	}
	t.Logf("snapshot took %v (memory state: %v)", time.Since(start), snap.GetHasMemoryState())
	if snap.GetHasMemoryState() {
		t.Fatal("Hyper-V snapshots are disk-only in this version")
	}
	// The source sandbox still runs after the snapshot.
	if out, _ := e.exec(sb.GetSandboxId(), "/bin/cat", "/root/marker"); out != "state\n" {
		t.Fatalf("after snapshot %q", out)
	}
	start = time.Now()
	cold := e.create(snapID, 1024, ownsandv1.NetworkMode_NETWORK_MODE_NONE)
	t.Logf("create from snapshot took %v (%v)", time.Since(start), cold.GetRestorePath())
	if cold.GetRestorePath() != ownsandv1.SandboxRestorePath_SANDBOX_RESTORE_PATH_DISK_COLD_BOOT {
		t.Fatalf("restore path %v", cold.GetRestorePath())
	}
	checkDiskSize(cold.GetSandboxId())
	if out, _ := e.exec(cold.GetSandboxId(), "/bin/cat", "/root/marker"); out != "state\n" {
		t.Fatalf("sandbox from snapshot %q", out)
	}
	ids := []string{sb.GetSandboxId(), cold.GetSandboxId()}

	if os.Getenv(envOutbound) != "" {
		// A host HTTP server on every interface: a probe that reaches it
		// prints REACHED, so BLOCKED shows the policy, not a closed port.
		ln, err := net.Listen("tcp4", "0.0.0.0:0")
		if err != nil {
			t.Fatal(err)
		}
		srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "host") })}
		go srv.Serve(ln)
		t.Cleanup(func() { srv.Close() })
		port := ln.Addr().(*net.TCPAddr).Port
		lan := hostLANAddress()
		if lan != "" {
			if c, err := net.DialTimeout("tcp", net.JoinHostPort(lan, strconv.Itoa(port)), 3*time.Second); err != nil {
				t.Fatalf("the host itself cannot reach its server on %s: %v", lan, err)
			} else {
				c.Close()
			}
		}
		start = time.Now()
		ob := e.create("", 512, ownsandv1.NetworkMode_NETWORK_MODE_OUTBOUND)
		t.Logf("OUTBOUND cold boot took %v", time.Since(start))
		ids = append(ids, ob.GetSandboxId())
		out, ex := e.exec(ob.GetSandboxId(), "/bin/sh", "-c", "ls /sys/class/net; cat /etc/resolv.conf; wget -q -T 10 -O /dev/null http://example.com/ && echo INTERNET_OK")
		if !strings.Contains(out, "INTERNET_OK") {
			t.Fatalf("OUTBOUND sandbox cannot reach the internet: %q %v", out, ex)
		}
		t.Logf("OUTBOUND guest: %s", strings.TrimSpace(out))
		for _, target := range []string{
			net.JoinHostPort(lan, strconv.Itoa(port)),
			net.JoinHostPort("127.0.0.1", strconv.Itoa(port)),
			"169.254.169.254:80",
			"10.0.0.1:80",
		} {
			if strings.HasPrefix(target, ":") {
				continue
			}
			out, _ := e.exec(ob.GetSandboxId(), "/bin/sh", "-c", fmt.Sprintf("wget -q -T 3 -O /dev/null http://%s/ && echo REACHED || echo BLOCKED", target))
			if !strings.Contains(out, "BLOCKED") {
				t.Fatalf("OUTBOUND sandbox reached %s: %q", target, out)
			}
			t.Logf("OUTBOUND sandbox is blocked from %s", target)
		}
	}

	for _, id := range ids {
		if en, err := m.Delete(context.Background(), &ownsandv1.DeleteSandboxCommand{SandboxId: id}); err != nil || en.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_DELETED {
			t.Fatalf("delete %v %v", en, err)
		}
	}
	// The inventory must succeed: a failed enumeration is not proof that no VM runs.
	ids, err = rt.Running(context.Background())
	if err != nil {
		t.Fatalf("list running VMs: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("VMs still run: %v", ids)
	}
}

func (e *env) checkBaseDiskSizing(rt vmruntime.Runtime, rootfs, vmsDir string) {
	e.t.Helper()
	digest := func() [sha256.Size]byte {
		file, err := os.Open(rootfs)
		if err != nil {
			e.t.Fatal(err)
		}
		defer file.Close()
		hash := sha256.New()
		n, err := io.Copy(hash, file)
		if err != nil || n != 2<<30 {
			e.t.Fatalf("read base image: %d bytes, want 2 GiB, error %v", n, err)
		}
		return [sha256.Size]byte(hash.Sum(nil))
	}
	original := digest()
	ctx := e.t.Context()
	cmd := createCommand("", 512, 1, ownsandv1.NetworkMode_NETWORK_MODE_NONE)
	cmd.Limits.DiskGib = 1
	entry, err := e.m.Create(ctx, cmd)
	var commandErr *sandboxes.CommandError
	if !errors.As(err, &commandErr) || commandErr.Code != ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION ||
		entry.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_FAILED ||
		entry.GetEndReason() != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_CREATE_FAILED {
		e.t.Fatalf("undersized create: entry %v, error %v", entry, err)
	}
	if e.m.Busy() {
		e.t.Fatal("undersized create left an active sandbox")
	}
	if ids, err := rt.Running(ctx); err != nil || len(ids) != 0 {
		e.t.Fatalf("running VMs after undersized create: %v, error %v", ids, err)
	}
	if _, err := os.Stat(filepath.Join(vmsDir, cmd.GetSandboxId())); !errors.Is(err, os.ErrNotExist) {
		e.t.Fatalf("undersized create left a VM directory: %v", err)
	}
	for _, diskGiB := range []int64{2, 3} {
		cmd := createCommand("", 512, diskGiB, ownsandv1.NetworkMode_NETWORK_MODE_NONE)
		cmd.Limits.DiskGib = diskGiB
		entry, err := e.m.Create(ctx, cmd)
		if err != nil || entry.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
			e.t.Fatalf("create with %d GiB: entry %v, error %v", diskGiB, entry, err)
		}
		id := cmd.GetSandboxId()
		out, ex := e.exec(id, "/bin/cat", "/sys/block/sda/size")
		want := strconv.FormatInt((diskGiB<<30)/512, 10)
		if !execSucceeded(ex) || strings.TrimSpace(out) != want {
			e.t.Fatalf("guest disk sectors = %q, want %s, exit %v", out, want, ex)
		}
		if entry, err := e.m.Delete(ctx, &ownsandv1.DeleteSandboxCommand{SandboxId: id}); err != nil ||
			entry.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_DELETED {
			e.t.Fatalf("delete: entry %v, error %v", entry, err)
		}
	}
	if digest() != original {
		e.t.Fatal("the base image changed")
	}
}
