// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build darwin && cgo

package vzrt_test

import (
	"context"
	"crypto/rand"
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
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime/vzrt"
	"github.com/delinoio/mono/go/ownsand/datapath"
	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// Environment of the opt-in integration test. It boots real
// Virtualization.framework VMs, so the test binary must be signed with the
// com.apple.security.virtualization entitlement:
//
//	go test -c -o vzrt.test ./cmds/ownsand-agent/internal/vmruntime/vzrt
//	codesign --sign - --force --entitlements cmds/ownsand-agent/entitlements.plist vzrt.test
//	OWNSAND_VZ_TEST_KERNEL=... OWNSAND_VZ_TEST_ROOTFS=... ./vzrt.test -test.v
//
// The kernel and the root file system come from
// cmds/ownsand-agent/images/linux/build-image.sh for the host architecture.
// OWNSAND_VZ_TEST_OUTBOUND=1 also runs the OUTBOUND network policy checks,
// which need internet access.
const (
	envKernel   = "OWNSAND_VZ_TEST_KERNEL"
	envRootfs   = "OWNSAND_VZ_TEST_ROOTFS"
	envOutbound = "OWNSAND_VZ_TEST_OUTBOUND"
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

// invalidateMemoryState corrupts a disposable snapshot without clearing its
// memory-state flag, so create attempts native restore before falling back.
func (e *env) invalidateMemoryState(snapshotID string) {
	e.t.Helper()
	dir, err := e.m.ImportDir(snapshotID)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "content", "vm.state"), []byte("invalid machine state"), 0o600); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) create(snapshotID string, memMiB int64, network ownsandv1.NetworkMode, memoryRestore bool) *ownsandv1.SandboxStateEntry {
	e.t.Helper()
	id, _ := uuidv7.NewString()
	now := time.Now()
	cmd := &ownsandv1.CreateSandboxCommand{
		SandboxId:            id,
		Source:               &ownsandv1.CreateSandboxCommand_BaseImage{BaseImage: ownsandv1.BaseImage_BASE_IMAGE_LINUX},
		Resources:            &ownsandv1.Resources{VcpuCount: 2, MemoryMib: memMiB, DiskGib: 2},
		NetworkMode:          network,
		GuestOs:              ownsandv1.GuestOs_GUEST_OS_LINUX,
		IdleTtl:              durationpb.New(time.Hour),
		MaxTtl:               durationpb.New(time.Hour),
		CreateTime:           timestamppb.New(now),
		MaxDeadline:          timestamppb.New(now.Add(time.Hour)),
		Limits:               &ownsandv1.DeviceLimits{VcpuCount: 8, MemoryMib: 8192, DiskGib: 16, VcpuOvercommitPercent: 100},
		MemoryRestoreAllowed: memoryRestore,
	}
	if snapshotID != "" {
		cmd.Source = &ownsandv1.CreateSandboxCommand_SnapshotId{SnapshotId: snapshotID}
	}
	entry, err := e.m.Create(context.Background(), cmd)
	if err != nil {
		e.t.Fatalf("create: %v", err)
	}
	if entry.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		e.t.Fatalf("create: sandbox %v", entry)
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

func execSucceeded(exited *ownsandv1.ExecExited) bool {
	code, ok := exited.GetStatus().(*ownsandv1.ExecExited_ExitCode)
	return ok && code != nil && code.ExitCode == 0
}

// waitFor polls cond until it holds or 30 seconds passed.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// hostServerMarker is the response body of the host server of the
// OUTBOUND checks.
const hostServerMarker = "OWNSAND-HOST-SERVER-MARKER"

// startHostServer serves hostServerMarker on every IPv4 address of the host
// on a free port until the test ends and returns the port. The OUTBOUND
// checks fetch it from the guest at the host's own addresses, so they fail
// when the guest reaches a host service; a fetch of port 80, where usually
// nothing listens on a host, is refused whether the policy holds or not. It
// first checks that the host itself reaches the server at each of addrs
// (empty addresses are skipped).
func startHostServer(t *testing.T, addrs ...string) string {
	t.Helper()
	l, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{
		Handler:           http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, hostServerMarker) }),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	port := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	client := &http.Client{Timeout: 3 * time.Second}
	for _, a := range addrs {
		if a == "" {
			continue
		}
		resp, err := client.Get("http://" + net.JoinHostPort(a, port) + "/")
		if err != nil {
			t.Fatalf("the host cannot reach its own test server at %s: %v", a, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(body) != hostServerMarker {
			t.Fatalf("the host test server at %s answered %q", a, body)
		}
	}
	return port
}

// hostLANAddress returns an IPv4 address of a host interface that is not
// loopback, which the OUTBOUND policy must block.
func hostLANAddress() string {
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLoopback() && !ipn.IP.IsLinkLocalUnicast() {
			return ipn.IP.String()
		}
	}
	return ""
}

func TestVirtualizationIntegration(t *testing.T) {
	kernel, rootfs := os.Getenv(envKernel), os.Getenv(envRootfs)
	if kernel == "" || rootfs == "" {
		t.Skipf("set %s and %s and sign the test binary to boot real VMs", envKernel, envRootfs)
	}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	if os.Getenv("OWNSAND_TEST_VERBOSE") != "" {
		logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rt, err := vzrt.New(vzrt.Options{Kernel: kernel, Rootfs: rootfs, VMsDir: st.Path(store.VMsDir), Logger: logger})
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
	sb := e.create("", 512, ownsandv1.NetworkMode_NETWORK_MODE_NONE, false)
	t.Logf("cold boot took %v", time.Since(start))
	if sb.GetRestorePath() != ownsandv1.SandboxRestorePath_SANDBOX_RESTORE_PATH_BASE_IMAGE {
		t.Fatalf("sandbox %v", sb)
	}
	out, ex := e.exec(sb.GetSandboxId(), "/bin/sh", "-c", "echo hello; uname -sm; echo state > /root/marker; df -k / | tail -1; ls /sys/class/net")
	if !execSucceeded(ex) || !strings.Contains(out, "hello\nLinux aarch64\n") && !strings.Contains(out, "hello\nLinux x86_64\n") {
		t.Fatalf("exec output %q exited %q", out, ex)
	}
	t.Logf("guest: %s", strings.TrimSpace(out))
	// The root file system grew to the 2 GiB disk at boot.
	fields := strings.Fields(strings.Split(out, "\n")[2])
	if len(fields) < 2 || len(fields[1]) < 7 {
		t.Fatalf("root file system did not grow: %q", out)
	}
	// A NONE sandbox has no network device.
	if strings.Contains(out, "eth0") {
		t.Fatalf("NONE sandbox has a network device: %q", out)
	}
	if _, err := m.Pause(context.Background(), &ownsandv1.PauseSandboxCommand{SandboxId: sb.GetSandboxId()}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Resume(context.Background(), &ownsandv1.ResumeSandboxCommand{SandboxId: sb.GetSandboxId()}); err != nil {
		t.Fatal(err)
	}
	if out, _ := e.exec(sb.GetSandboxId(), "/bin/cat", "/root/marker"); out != "state\n" {
		t.Fatalf("after resume %q", out)
	}

	// The guest console log is capped at 1 MiB per sandbox. A console that
	// stays below the cap is recorded; a guest that floods /dev/hvc0 neither
	// blocks nor grows console.log past the cap, and the sandbox keeps
	// answering exec requests.
	consoleLog := st.Path(store.VMsDir, sb.GetSandboxId(), "console.log")
	const marker = "OWNSAND-CONSOLE-MARKER"
	if out, ex := e.exec(sb.GetSandboxId(), "/bin/sh", "-c", "echo "+marker+" > /dev/hvc0"); !execSucceeded(ex) {
		t.Fatalf("console write %q exited %q", out, ex)
	}
	waitFor(t, "the marker in console.log", func() bool {
		data, _ := os.ReadFile(consoleLog)
		return strings.Contains(string(data), marker)
	})
	if out, ex := e.exec(sb.GetSandboxId(), "/bin/sh", "-c", "head -c 3145728 /dev/zero | tr '\\0' x > /dev/hvc0"); !execSucceeded(ex) {
		t.Fatalf("console flood %q exited %q", out, ex)
	}
	waitFor(t, "console.log at the cap", func() bool {
		fi, err := os.Stat(consoleLog)
		return err == nil && fi.Size() == vmruntime.ConsoleLogLimit
	})
	if out, ex := e.exec(sb.GetSandboxId(), "/bin/cat", "/root/marker"); !execSucceeded(ex) || out != "state\n" {
		t.Fatalf("exec after the console flood %q exited %q", out, ex)
	}
	if fi, err := os.Stat(consoleLog); err != nil || fi.Size() != vmruntime.ConsoleLogLimit {
		t.Fatalf("console.log %v %v, want exactly %d bytes", fi, err, vmruntime.ConsoleLogLimit)
	}
	snapID, _ := uuidv7.NewString()
	snap, err := m.CreateSnapshot(context.Background(), &ownsandv1.CreateSnapshotCommand{SnapshotId: snapID, SandboxId: sb.GetSandboxId()})
	if err != nil {
		t.Fatalf("snapshot %v %v", snap, err)
	}
	t.Logf("snapshot has memory state: %v", snap.GetHasMemoryState())
	start = time.Now()
	restored := e.create(snapID, 512, ownsandv1.NetworkMode_NETWORK_MODE_NONE, true)
	t.Logf("restore from snapshot took %v (%v)", time.Since(start), restored.GetRestorePath())
	if snap.GetHasMemoryState() && restored.GetRestorePath() != ownsandv1.SandboxRestorePath_SANDBOX_RESTORE_PATH_MEMORY_STATE {
		t.Fatalf("restore path %v", restored.GetRestorePath())
	}
	if out, _ := e.exec(restored.GetSandboxId(), "/bin/cat", "/root/marker"); out != "state\n" {
		t.Fatalf("restored sandbox %q", out)
	}
	cold := e.create(snapID, 1024, ownsandv1.NetworkMode_NETWORK_MODE_NONE, true)
	if cold.GetRestorePath() != ownsandv1.SandboxRestorePath_SANDBOX_RESTORE_PATH_DISK_COLD_BOOT {
		t.Fatalf("cold restore path %v", cold.GetRestorePath())
	}
	if out, _ := e.exec(cold.GetSandboxId(), "/bin/cat", "/root/marker"); out != "state\n" {
		t.Fatalf("cold-booted sandbox %q", out)
	}
	if en, err := m.Delete(context.Background(), &ownsandv1.DeleteSandboxCommand{SandboxId: restored.GetSandboxId()}); err != nil || en.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_DELETED {
		t.Fatalf("delete restored sandbox %v %v", en, err)
	}
	ids := []string{sb.GetSandboxId(), cold.GetSandboxId()}

	if snap.GetHasMemoryState() {
		e.invalidateMemoryState(snapID)
		fallback := e.create(snapID, 512, ownsandv1.NetworkMode_NETWORK_MODE_NONE, true)
		if fallback.GetRestorePath() != ownsandv1.SandboxRestorePath_SANDBOX_RESTORE_PATH_DISK_COLD_BOOT {
			t.Fatalf("restore path after invalid memory state %v", fallback.GetRestorePath())
		}
		if out, ex := e.exec(fallback.GetSandboxId(), "/bin/cat", "/root/marker"); out != "state\n" || ex.GetExitCode() != 0 {
			t.Fatalf("sandbox after invalid memory state %q %v", out, ex)
		}
		if en, err := m.Delete(context.Background(), &ownsandv1.DeleteSandboxCommand{SandboxId: fallback.GetSandboxId()}); err != nil || en.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_DELETED {
			t.Fatalf("delete fallback sandbox %v %v", en, err)
		}
	}

	if os.Getenv(envOutbound) != "" {
		ob := e.create("", 512, ownsandv1.NetworkMode_NETWORK_MODE_OUTBOUND, false)
		ids = append(ids, ob.GetSandboxId())
		out, ex := e.exec(ob.GetSandboxId(), "/bin/sh", "-c", "cat /etc/resolv.conf; wget -q -T 10 -O /dev/null http://example.com/ && echo INTERNET_OK")
		if !strings.Contains(out, "INTERNET_OK") {
			t.Fatalf("OUTBOUND sandbox cannot reach the internet: %q %v", out, ex)
		}
		lan := hostLANAddress()
		port := startHostServer(t, lan, "127.0.0.1")
		for _, target := range []string{lan, "127.0.0.1", "169.254.169.254", "10.0.0.1"} {
			if target == "" {
				continue
			}
			out, _ := e.exec(ob.GetSandboxId(), "/bin/sh", "-c", "wget -q -T 3 -O /dev/null http://"+target+"/ && echo REACHED || echo BLOCKED")
			if !strings.Contains(out, "BLOCKED") {
				t.Fatalf("OUTBOUND sandbox reached %s: %q", target, out)
			}
		}
		// The host server answers on the host's own addresses and on no
		// other: the guest must not reach it there, nor through its
		// gateway, which serves only DNS.
		for _, target := range []string{lan, "127.0.0.1", "$gw"} {
			if target == "" {
				continue
			}
			out, _ := e.exec(ob.GetSandboxId(), "/bin/sh", "-c",
				"gw=$(ip route | awk '/^default/ {print $3}'); wget -q -T 3 -O - http://"+target+":"+port+"/ 2>&1; echo gateway=$gw")
			if strings.Contains(out, hostServerMarker) || !strings.Contains(out, "gateway=172.") {
				t.Fatalf("OUTBOUND sandbox reached the host server at %s:%s (or has no gateway): %q", target, port, out)
			}
		}
	}

	for _, id := range ids {
		if en, err := m.Delete(context.Background(), &ownsandv1.DeleteSandboxCommand{SandboxId: id}); err != nil || en.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_DELETED {
			t.Fatalf("delete %v %v", en, err)
		}
	}
	if ids, _ := rt.Running(context.Background()); len(ids) != 0 {
		t.Fatalf("VMs still run: %v", ids)
	}
}
