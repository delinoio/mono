// SPDX-License-Identifier: Apache-2.0

package fcrt

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/netpolicy"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func quiet() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func TestVsockHandshake(t *testing.T) {
	dir := t.TempDir()
	f, _ := os.Open(dir)
	defer f.Close()
	v := &vm{dir: dir, dirFile: f}
	l, err := net.Listen("unix", v.path(vsockSocket))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			r := bufio.NewReader(c)
			line, _ := r.ReadString('\n')
			if line == "CONNECT 1024\n" {
				c.Write([]byte("OK 1073741824\npayload"))
			} else {
				c.Close()
			}
		}
	}()
	c, err := v.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 7)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "payload" {
		t.Fatalf("stream after handshake %q %v", buf, err)
	}
	if len(dir) > maxSocketPath && !strings.HasPrefix(v.path(vsockSocket), "/proc/self/fd/") {
		t.Fatal("long socket path not shortened")
	}
}

func TestAPIClientErrors(t *testing.T) {
	dir := t.TempDir()
	f, _ := os.Open(dir)
	defer f.Close()
	v := &vm{dir: dir, dirFile: f}
	l, err := net.Listen("unix", v.path(apiSocket))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = append(got, r.Method+" "+r.URL.Path+" "+string(body))
		if r.URL.Path == "/fail" {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(apiFault{FaultMessage: "bad request"})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})}
	go srv.Serve(l)
	defer srv.Close()
	c := newAPIClient(v.path(apiSocket))
	if err := c.waitReady(context.Background(), make(chan struct{})); err != nil {
		t.Fatal(err)
	}
	if err := c.call(context.Background(), http.MethodPatch, "/vm", vmState{State: "Paused"}); err != nil {
		t.Fatal(err)
	}
	if err := c.call(context.Background(), http.MethodPut, "/fail", nil); err == nil || !strings.Contains(err.Error(), "bad request") {
		t.Fatalf("fault: %v", err)
	}
	if got[len(got)-2] != `PATCH /vm {"state":"Paused"}` {
		t.Fatalf("requests %v", got)
	}
}

func TestGrowDisk(t *testing.T) {
	mkfs := tool("mkfs.ext4")
	if _, err := os.Stat(mkfs); err != nil {
		t.Skip("mkfs.ext4 not installed")
	}
	img := filepath.Join(t.TempDir(), "disk.ext4")
	if err := os.WriteFile(img, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	os.Truncate(img, 16<<20)
	if out, err := exec.Command(mkfs, "-q", "-F", img).CombinedOutput(); err != nil {
		t.Fatalf("mkfs: %v %s", err, out)
	}
	cp := img + ".copy"
	if err := copyDisk(img, cp); err != nil {
		t.Fatal(err)
	}
	if err := growDisk(context.Background(), cp, 64<<20); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(cp); st.Size() != 64<<20 {
		t.Fatalf("size %d", st.Size())
	}
	// A smaller size keeps the image.
	if err := growDisk(context.Background(), cp, 1<<20); err != nil {
		t.Fatal(err)
	}
}

func TestCapabilitiesWithoutFirecracker(t *testing.T) {
	r, err := New(Options{Binary: filepath.Join(t.TempDir(), "missing"), Kernel: "/nonexistent", Rootfs: "/nonexistent", Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	caps := r.Capabilities(context.Background())
	if len(caps.GuestOSes) != 0 || len(caps.BaseImages) != 0 {
		t.Fatalf("capabilities without firecracker: %+v", caps)
	}
	ids := map[string]bool{}
	for _, c := range caps.Checks {
		ids[c.GetCheckId()] = c.GetPassed()
	}
	if ids["firecracker-binary"] || ids["linux-base-image"] {
		t.Fatalf("checks %v", ids)
	}
	_, err = r.Create(context.Background(), vmruntime.Spec{SandboxID: "x", Dir: t.TempDir(), GuestOS: ownsandv1.GuestOs_GUEST_OS_WINDOWS})
	if err == nil {
		t.Fatal("windows guest accepted")
	}
	_, err = r.Create(context.Background(), vmruntime.Spec{SandboxID: "x", Dir: t.TempDir(), GuestOS: ownsandv1.GuestOs_GUEST_OS_LINUX, NetworkMode: ownsandv1.NetworkMode_NETWORK_MODE_NONE, Resources: &ownsandv1.Resources{VcpuCount: 1, MemoryMib: 128, DiskGib: 1}})
	if err == nil {
		t.Fatal("create without a base image accepted")
	}
}

func openFDs(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

// A create whose boot fails releases the VM registration and the directory
// handle: the sandbox manager never calls Delete for a FAILED sandbox.
func TestCreateFailureReleasesVM(t *testing.T) {
	falseBin, err := exec.LookPath("false")
	if err != nil {
		t.Skip("false not installed")
	}
	img := t.TempDir()
	kernel, rootfs := filepath.Join(img, "vmlinux"), filepath.Join(img, "rootfs.ext4")
	for _, p := range []string{kernel, rootfs} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r, err := New(Options{Binary: falseBin, Kernel: kernel, Rootfs: rootfs, Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	before := openFDs(t)
	for i := 0; i < 3; i++ {
		dir := filepath.Join(t.TempDir(), "vm")
		_, err := r.Create(context.Background(), vmruntime.Spec{SandboxID: "x", Dir: dir, GuestOS: ownsandv1.GuestOs_GUEST_OS_LINUX,
			NetworkMode: ownsandv1.NetworkMode_NETWORK_MODE_NONE, Resources: &ownsandv1.Resources{VcpuCount: 1, MemoryMib: 128}})
		if err == nil {
			t.Fatal("create with a failing firecracker succeeded")
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("VM directory kept: %v", err)
		}
	}
	r.mu.Lock()
	n := len(r.vms)
	r.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d failed VMs still registered", n)
	}
	if after := openFDs(t); after != before {
		t.Fatalf("open descriptors %d before, %d after failed creates", before, after)
	}
}

// The runtime runs the network policy refresh loop of its manager and
// stops it on Close.
func TestRuntimeRunsNetworkRefresh(t *testing.T) {
	r, err := New(Options{Binary: filepath.Join(t.TempDir(), "missing"), Network: netpolicy.NewManager(quiet()), Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	if r.refreshDone == nil {
		t.Fatal("refresh loop not started")
	}
	select {
	case <-r.refreshDone:
		t.Fatal("refresh loop ended before Close")
	default:
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-r.refreshDone:
	default:
		t.Fatal("refresh loop still runs after Close")
	}
}
