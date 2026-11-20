//go:build linux

package firecracker

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/guestconn"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/runtime"
	"github.com/delinoio/mono/packages/ownsand-sdk-go/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// TestRealVMAcceptance boots a real Firecracker microVM. It runs only when
// OWNSAND_FC_ACCEPTANCE_DIR names a directory with `firecracker` (the pinned
// release), `vmlinux` and `rootfs.ext4` (a root file system whose init
// starts ownsand-guest), and it needs read and write access to /dev/kvm.
// It runs without the network policy (NONE network mode), so it needs no
// CAP_NET_ADMIN.
func TestRealVMAcceptance(t *testing.T) {
	src := os.Getenv("OWNSAND_FC_ACCEPTANCE_DIR")
	if src == "" {
		t.Skip("set OWNSAND_FC_ACCEPTANCE_DIR to run the real VM acceptance test")
	}
	state := t.TempDir()
	img := filepath.Join(state, "images", "linux")
	if err := os.MkdirAll(img, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{KernelFile, RootfsFile} {
		if err := os.Symlink(filepath.Join(src, f), filepath.Join(img, f)); err != nil {
			t.Fatal(err)
		}
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	rt, err := New(Config{StateDir: state, Logger: log, Binary: filepath.Join(src, "firecracker"), AllowUnsignedImage: true, SkipNetwork: true})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	caps := rt.Capabilities(ctx)
	t.Logf("hypervisor checks: %v", caps.HypervisorChecks)
	if len(caps.GuestOSes) == 0 || len(caps.BuiltBaseImages) == 0 {
		t.Fatalf("runtime reports no Linux guest: %v", caps.HypervisorChecks)
	}

	id := datapath.NewID()
	spec := runtime.VMSpec{SandboxID: id, GuestOS: ownsandv1.GuestOs_GUEST_OS_LINUX, Source: runtime.Source{BaseImage: ownsandv1.BaseImage_BASE_IMAGE_LINUX},
		VCPUs: 1, MemoryMiB: 256, DiskGiB: 1, Network: ownsandv1.NetworkMode_NETWORK_MODE_NONE}
	if err := rt.Create(ctx, spec); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	path, err := rt.Start(ctx, id)
	if err != nil {
		dumpConsole(t, rt, id)
		t.Fatal(err)
	}
	gc := waitGuest(t, ctx, rt, id)
	t.Logf("cold boot to guest ready: %v (restore path %v)", time.Since(start), path)
	if out := execEcho(t, ctx, gc, "hello from the guest"); out != "hello from the guest\n" {
		t.Fatalf("exec output %q", out)
	}
	if vms, _ := rt.List(ctx); len(vms) != 1 || vms[0].SandboxID != id {
		t.Fatalf("list %v", vms)
	}
	if err := rt.Pause(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := rt.Resume(ctx, id); err != nil {
		t.Fatal(err)
	}
	snapID := datapath.NewID()
	info, err := rt.Snapshot(ctx, id, snapID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("snapshot size %d bytes, memory state %v", info.SizeBytes, info.HasMemoryState)
	_ = gc.Close()

	// Restore the memory state in a new VM directory.
	id2 := datapath.NewID()
	spec2 := spec
	spec2.SandboxID, spec2.Source = id2, runtime.Source{SnapshotID: snapID}
	spec2.TryMemoryRestore = true
	if err := rt.Create(ctx, spec2); err != nil {
		t.Fatal(err)
	}
	start = time.Now()
	path, err = rt.Start(ctx, id2)
	if err != nil {
		dumpConsole(t, rt, id2)
		t.Fatal(err)
	}
	t.Logf("snapshot start: %v, restore path %v", time.Since(start), path)
	gc2 := waitGuest(t, ctx, rt, id2)
	if out := execEcho(t, ctx, gc2, "restored"); out != "restored\n" {
		t.Fatalf("exec output after restore %q", out)
	}
	_ = gc2.Close()
	for _, v := range []string{id, id2} {
		if err := rt.Delete(ctx, v); err != nil {
			t.Fatal(err)
		}
	}
	if vms, _ := rt.List(ctx); len(vms) != 0 {
		t.Fatalf("VMs left after delete: %v", vms)
	}
}

func dumpConsole(t *testing.T, rt *Runtime, id string) {
	data, _ := os.ReadFile(filepath.Join(rt.vmsDir(), id, consoleLog))
	if len(data) > 8192 {
		data = data[len(data)-8192:]
	}
	t.Logf("console tail:\n%s", data)
}

func waitGuest(t *testing.T, ctx context.Context, rt *Runtime, id string) *guestconn.Client {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		rwc, err := rt.DialGuest(ctx, id)
		if err == nil {
			gc := guestconn.New(rwc, slog.New(slog.NewTextHandler(io.Discard, nil)))
			rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			_, err = gc.Ready(rctx)
			cancel()
			if err == nil {
				_ = gc.SetClock(time.Now())
				return gc
			}
			_ = gc.Close()
		}
		time.Sleep(200 * time.Millisecond)
	}
	dumpConsole(t, rt, id)
	t.Fatal("the guest helper did not become ready")
	return nil
}

func execEcho(t *testing.T, ctx context.Context, gc *guestconn.Client, msg string) string {
	t.Helper()
	s, err := gc.Open(datapath.NewFrame(0, "sb", func(f *ownsandv1.DataFrame) {
		f.Payload = &ownsandv1.DataFrame_ExecRequest{ExecRequest: &ownsandv1.ExecRequest{
			ExecId: datapath.NewID(), Command: []string{"/bin/echo", msg}, BindingSalt: datapath.NewBindingSalt(),
		}}
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var out bytes.Buffer
	for {
		f, err := s.Recv(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if e := f.GetError(); e != nil {
			t.Fatalf("exec error %s", e.GetCode())
		}
		ev := f.GetExecEvent()
		if o := ev.GetOutput(); o != nil {
			out.Write(o.GetData())
		}
		if ev.GetExited() != nil || ev.GetFailed() != nil {
			if ev.GetFailed() != nil {
				t.Fatalf("exec failed: %s", ev.GetFailed().GetReason())
			}
			return out.String()
		}
	}
}

// TestRealVMNetworkAcceptance boots an OUTBOUND guest with the network
// policy. Besides OWNSAND_FC_ACCEPTANCE_DIR it needs
// OWNSAND_FC_NETWORK_ACCEPTANCE=1, root privileges (CAP_NET_ADMIN, IPv4
// forwarding), nft and ip. OWNSAND_FC_BLOCKED_TARGETS lists addresses that
// the guest must not reach (for example, the host and its LAN gateway), and
// OWNSAND_FC_INTERNET_TARGET an address that it must reach.
func TestRealVMNetworkAcceptance(t *testing.T) {
	src := os.Getenv("OWNSAND_FC_ACCEPTANCE_DIR")
	if src == "" || os.Getenv("OWNSAND_FC_NETWORK_ACCEPTANCE") != "1" {
		t.Skip("set OWNSAND_FC_ACCEPTANCE_DIR and OWNSAND_FC_NETWORK_ACCEPTANCE=1 to run the network acceptance test")
	}
	state := t.TempDir()
	img := filepath.Join(state, "images", "linux")
	_ = os.MkdirAll(img, 0o700)
	for _, f := range []string{KernelFile, RootfsFile} {
		if err := os.Symlink(filepath.Join(src, f), filepath.Join(img, f)); err != nil {
			t.Fatal(err)
		}
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	rt, err := New(Config{StateDir: state, Logger: log, Binary: filepath.Join(src, "firecracker"), AllowUnsignedImage: true})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := rt.net.status(); err != nil {
		t.Fatalf("network policy: %v", err)
	}
	run := func(id string, gc *guestconn.Client, cmd ...string) (string, int64) {
		s, err := gc.Open(datapath.NewFrame(0, "sb", func(f *ownsandv1.DataFrame) {
			f.Payload = &ownsandv1.DataFrame_ExecRequest{ExecRequest: &ownsandv1.ExecRequest{ExecId: datapath.NewID(), Command: cmd, BindingSalt: datapath.NewBindingSalt()}}
		}))
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		var out bytes.Buffer
		for {
			f, err := s.Recv(ctx)
			if err != nil {
				t.Fatal(err)
			}
			ev := f.GetExecEvent()
			if o := ev.GetOutput(); o != nil {
				out.Write(o.GetData())
			}
			if ex := ev.GetExited(); ex != nil {
				return out.String(), ex.GetExitCode()
			}
			if ev.GetFailed() != nil {
				t.Fatalf("exec failed: %s", ev.GetFailed().GetReason())
			}
		}
	}
	for _, mode := range []ownsandv1.NetworkMode{ownsandv1.NetworkMode_NETWORK_MODE_OUTBOUND, ownsandv1.NetworkMode_NETWORK_MODE_NONE} {
		id := datapath.NewID()
		spec := runtime.VMSpec{SandboxID: id, GuestOS: ownsandv1.GuestOs_GUEST_OS_LINUX, Source: runtime.Source{BaseImage: ownsandv1.BaseImage_BASE_IMAGE_LINUX},
			VCPUs: 1, MemoryMiB: 256, DiskGiB: 1, Network: mode}
		if err := rt.Create(ctx, spec); err != nil {
			t.Fatal(err)
		}
		if _, err := rt.Start(ctx, id); err != nil {
			dumpConsole(t, rt, id)
			t.Fatal(err)
		}
		gc := waitGuest(t, ctx, rt, id)
		out, _ := run(id, gc, "/bin/sh", "-c", "ls /sys/class/net; cat /proc/net/route")
		t.Logf("%v guest interfaces and routes:\n%s", mode, out)
		internet := os.Getenv("OWNSAND_FC_INTERNET_TARGET")
		if internet == "" {
			internet = "1.1.1.1"
		}
		_, code := run(id, gc, "/bin/busybox", "ping", "-c", "1", "-W", "3", internet)
		if mode == ownsandv1.NetworkMode_NETWORK_MODE_OUTBOUND {
			if code != 0 {
				dumpConsole(t, rt, id)
				t.Errorf("OUTBOUND guest cannot reach %s (exit %d)", internet, code)
			}
			out, code = run(id, gc, "/bin/busybox", "nslookup", "one.one.one.one", rt.vmLink(id).Gateway.String())
			t.Logf("dns through the gateway: exit %d\n%s", code, out)
			if code != 0 {
				t.Errorf("DNS through the gateway failed")
			}
			targets := []string{rt.vmLink(id).Gateway.String()}
			if extra := os.Getenv("OWNSAND_FC_BLOCKED_TARGETS"); extra != "" {
				targets = append(targets, strings.Split(extra, ",")...)
			}
			for _, target := range targets {
				if _, code := run(id, gc, "/bin/busybox", "ping", "-c", "1", "-W", "2", target); code == 0 {
					t.Errorf("OUTBOUND guest reached the blocked address %s", target)
				} else {
					t.Logf("blocked as expected: %s", target)
				}
			}
		} else if code == 0 {
			t.Errorf("NONE guest reached %s", internet)
		}
		_ = gc.Close()
		if err := rt.Delete(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
}
