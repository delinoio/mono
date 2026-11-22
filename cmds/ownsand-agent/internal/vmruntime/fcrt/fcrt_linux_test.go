// SPDX-License-Identifier: FSL-1.1-ALv2

package fcrt

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
			}
			// Close the accepted connection in both branches, so no
			// descriptor stays open until a finalizer closes it (see
			// TestCreateFailureReleasesVM).
			c.Close()
		}
	}()
	c, err := v.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
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
	// Close the idle keep-alive connections of the client, so no descriptor
	// stays open until a finalizer closes it (see TestCreateFailureReleasesVM).
	defer c.hc.CloseIdleConnections()
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

// TestGrowDiskE2fsckOutcome runs growDisk with stub e2fsck and resize2fs
// tools on PATH. Only e2fsck exit codes 0 and 1 may reach resize2fs; a
// signal-terminated e2fsck (ExitCode -1) and any other exit code must fail
// before resize2fs runs.
func TestGrowDiskE2fsckOutcome(t *testing.T) {
	cases := []struct {
		name      string
		e2fsck    string
		wantErr   string
		wantRsize bool
	}{
		{name: "clean", e2fsck: "exit 0", wantRsize: true},
		{name: "corrected", e2fsck: "exit 1", wantRsize: true},
		{name: "uncorrected", e2fsck: "echo bad >&2; exit 4", wantErr: "exit status 4"},
		{name: "operational", e2fsck: "exit 8", wantErr: "exit status 8"},
		{name: "signal", e2fsck: "kill -9 $$", wantErr: "signal: killed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bin := t.TempDir()
			marker := filepath.Join(bin, "resize2fs.ran")
			writeStub := func(name, body string) {
				if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			writeStub("e2fsck", tc.e2fsck)
			writeStub("resize2fs", "touch '"+marker+"'")
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			img := filepath.Join(t.TempDir(), "disk.ext4")
			if err := os.WriteFile(img, make([]byte, 1<<20), 0o600); err != nil {
				t.Fatal(err)
			}
			err := growDisk(context.Background(), img, 4<<20)
			_, statErr := os.Stat(marker)
			ran := statErr == nil
			if tc.wantErr == "" && err != nil {
				t.Fatalf("growDisk: %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), "e2fsck") || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("growDisk error %v, want e2fsck %q", err, tc.wantErr)
			}
			if ran != tc.wantRsize {
				t.Fatalf("resize2fs ran %v, want %v", ran, tc.wantRsize)
			}
		})
	}
}

// TestGrowDiskCanceledE2fsck covers a context that ends while e2fsck runs.
func TestGrowDiskCanceledE2fsck(t *testing.T) {
	bin := t.TempDir()
	marker := filepath.Join(bin, "resize2fs.ran")
	if err := os.WriteFile(filepath.Join(bin, "e2fsck"), []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "resize2fs"), []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	img := filepath.Join(t.TempDir(), "disk.ext4")
	if err := os.WriteFile(img, make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	err := growDisk(ctx, img, 4<<20)
	if err == nil || !strings.Contains(err.Error(), "e2fsck") {
		t.Fatalf("growDisk error %v, want e2fsck failure", err)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("resize2fs ran after e2fsck was canceled")
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

// openFDs returns the open descriptors of the test process, each mapped to
// its link target.
func openFDs(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	fds := make(map[string]string, len(entries))
	for _, e := range entries {
		// The descriptor of the directory read itself is gone by now.
		if target, err := os.Readlink(filepath.Join("/proc/self/fd", e.Name())); err == nil {
			fds[e.Name()] = target
		}
	}
	return fds
}

// newFDs returns the descriptors in after that before does not have with the
// same target.
func newFDs(before, after map[string]string) []string {
	var added []string
	for fd, target := range after {
		if before[fd] != target {
			added = append(added, fd+" -> "+target)
		}
	}
	return added
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
	// A leak adds descriptors. The test compares the descriptors and not
	// their count, because descriptors of earlier tests in the same process
	// can close meanwhile. It waits briefly for descriptors that close
	// asynchronously.
	deadline := time.Now().Add(5 * time.Second)
	for {
		added := newFDs(before, openFDs(t))
		if len(added) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("descriptors opened by failed creates are still open: %v", added)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The runtime runs the startup clean-up once and then the network policy
// refresh loop of its manager, and stops the loop on Close. The test passes
// an in-memory clean-up and a manager without attachments, so it never
// reaches the live host network, whatever the capabilities of the test
// process are. Never call New here: with CAP_NET_ADMIN it would delete the
// tap devices and the nftables table of the host.
func TestRuntimeRunsNetworkRefresh(t *testing.T) {
	cleanups := 0
	r, err := newRuntime(Options{Binary: filepath.Join(t.TempDir(), "missing"), Network: netpolicy.NewManager(quiet()), Logger: quiet()}, func() error {
		cleanups++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if cleanups != 1 {
		t.Fatalf("startup clean-up ran %d times, want 1", cleanups)
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

// CleanupHost, which the device removal calls, runs the host network
// clean-up of the runtime again and returns its error. The test passes an
// in-memory clean-up and never reaches the live host network.
func TestRuntimeCleanupHostRunsHostCleanup(t *testing.T) {
	cleanups := 0
	fail := errors.New("delete nftables table inet ownsand: simulated")
	var result error
	r, err := newRuntime(Options{Binary: filepath.Join(t.TempDir(), "missing"), Network: netpolicy.NewManager(quiet()), Logger: quiet()}, func() error {
		cleanups++
		return result
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	var _ vmruntime.HostCleaner = r
	if err := r.CleanupHost(); err != nil || cleanups != 2 {
		t.Fatalf("CleanupHost = %v after %d clean-ups, want nil after 2 (startup and removal)", err, cleanups)
	}
	result = fail
	if err := r.CleanupHost(); !errors.Is(err, fail) {
		t.Fatalf("CleanupHost = %v, want the clean-up failure", err)
	}
}

// Without a network manager the startup skips the clean-up and the refresh
// loop.
func TestRuntimeWithoutNetworkSkipsCleanupAndRefresh(t *testing.T) {
	cleanups := 0
	r, err := newRuntime(Options{Binary: filepath.Join(t.TempDir(), "missing"), Logger: quiet()}, func() error {
		cleanups++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if cleanups != 0 {
		t.Fatalf("startup clean-up ran %d times without a network manager, want 0", cleanups)
	}
	if err := r.CleanupHost(); err != nil || cleanups != 0 {
		t.Fatalf("CleanupHost without a network manager = %v after %d clean-ups, want nil after 0", err, cleanups)
	}
	if r.refreshDone != nil || r.stopRefresh != nil {
		t.Fatal("refresh loop started without a network manager")
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
}

// stubOutbound builds a runtime with a network manager (never started) and
// stubbed capability and executable lookups.
func stubOutbound(t *testing.T, netAdmin bool, present ...string) *Runtime {
	t.Helper()
	r := &Runtime{
		opts:        Options{Binary: filepath.Join(t.TempDir(), "missing"), Kernel: "/nonexistent", Rootfs: "/nonexistent", Network: netpolicy.NewManager(quiet()), Logger: quiet()},
		logger:      quiet(),
		vms:         map[string]*vm{},
		hasNetAdmin: func() bool { return netAdmin },
		lookPath: func(name string) (string, error) {
			for _, p := range present {
				if p == name {
					return "/bin/" + name, nil
				}
			}
			return "", exec.ErrNotFound
		},
	}
	return r
}

func outboundCheck(t *testing.T, r *Runtime) (bool, string) {
	t.Helper()
	for _, c := range r.Capabilities(context.Background()).Checks {
		if c.GetCheckId() == "outbound-network" {
			return c.GetPassed(), c.GetDetail()
		}
	}
	t.Fatal("no outbound-network check")
	return false, ""
}

func TestOutboundNetworkReportsMissingTools(t *testing.T) {
	cases := []struct {
		name    string
		admin   bool
		present []string
		ok      bool
		detail  string
	}{
		{"all present", true, []string{"ip", "nft"}, true, ""},
		{"missing nft", true, []string{"ip"}, false, "nft"},
		{"missing ip", true, []string{"nft"}, false, "ip"},
		{"missing both", true, nil, false, "ip, nft"},
		{"missing capability", false, []string{"ip", "nft"}, false, "CAP_NET_ADMIN is missing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, detail := outboundCheck(t, stubOutbound(t, tc.admin, tc.present...))
			if ok != tc.ok || !strings.Contains(detail, tc.detail) {
				t.Fatalf("passed=%v detail=%q, want passed=%v containing %q", ok, detail, tc.ok, tc.detail)
			}
		})
	}
}

// A missing tool fails an OUTBOUND create at the preflight, before the VM
// directory exists; with the tools present, OUTBOUND proceeds to the later
// checks, and NONE ignores the tools.
func TestCreateOutboundPreflightChecksTools(t *testing.T) {
	spec := func(mode ownsandv1.NetworkMode) vmruntime.Spec {
		return vmruntime.Spec{GuestOS: ownsandv1.GuestOs_GUEST_OS_LINUX, NetworkMode: mode, Dir: filepath.Join(t.TempDir(), "vm")}
	}
	s := spec(ownsandv1.NetworkMode_NETWORK_MODE_OUTBOUND)
	_, err := stubOutbound(t, true, "ip").Create(context.Background(), s)
	if !errors.Is(err, vmruntime.ErrUnsupported) || !strings.Contains(err.Error(), "nft") {
		t.Fatalf("outbound create with missing nft: %v", err)
	}
	if _, statErr := os.Stat(s.Dir); statErr == nil {
		t.Fatal("VM directory created before the preflight failed")
	}
	_, err = stubOutbound(t, true, "ip", "nft").Create(context.Background(), spec(ownsandv1.NetworkMode_NETWORK_MODE_OUTBOUND))
	if !errors.Is(err, vmruntime.ErrUnsupported) || !strings.Contains(err.Error(), "base image") {
		t.Fatalf("outbound create with tools: %v", err)
	}
	_, err = stubOutbound(t, true).Create(context.Background(), spec(ownsandv1.NetworkMode_NETWORK_MODE_NONE))
	if !errors.Is(err, vmruntime.ErrUnsupported) || !strings.Contains(err.Error(), "base image") {
		t.Fatalf("NONE create without tools: %v", err)
	}
}

// TestDeleteRemovesFilesOfSandboxWithoutProcess checks the idempotent file
// removal that the agent relies on at start for a sandbox that lost its VM:
// the VM directory exists, but no firecracker process or tracked VM does.
func TestDeleteRemovesFilesOfSandboxWithoutProcess(t *testing.T) {
	vmsDir := t.TempDir()
	r := &Runtime{opts: Options{VMsDir: vmsDir}, logger: quiet(), vms: map[string]*vm{}, host: newLinuxProcs(), stopTimeout: exitTimeout}
	dir := r.vmDir("01a1fd14-c825-72ac-926c-3bd6b87a8ba5")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "disk.ext4"), []byte("disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // the second call checks idempotence
		if err := r.Delete(context.Background(), "01a1fd14-c825-72ac-926c-3bd6b87a8ba5"); err != nil {
			t.Fatalf("delete %d: %v", i, err)
		}
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("VM directory kept: %v", err)
	}
}
