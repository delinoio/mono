// SPDX-License-Identifier: FSL-1.1-ALv2

package fcrt

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
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

// delayedAckPeer listens on the vsock socket of a new vm. Each accepted
// connection reads the CONNECT line, reports it on accepted, and then waits on
// release before it answers "OK". closed receives the result of the peer's
// read: io.EOF when the dialer closes its end, or a timeout error when it
// never does.
func delayedAckPeer(t *testing.T) (v *vm, accepted chan struct{}, release chan struct{}, closed chan error) {
	t.Helper()
	dir := t.TempDir()
	f, _ := os.Open(dir)
	t.Cleanup(func() { f.Close() })
	v = &vm{dir: dir, dirFile: f}
	l, err := net.Listen("unix", v.path(vsockSocket))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	accepted = make(chan struct{}, 1)
	release = make(chan struct{})
	closed = make(chan error, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		r := bufio.NewReader(c)
		if _, err := r.ReadString('\n'); err != nil {
			return
		}
		accepted <- struct{}{}
		// Watch for the dialer closing its end while the acknowledgement is
		// delayed; the dialer sends nothing after the CONNECT line.
		read := make(chan error, 1)
		go func() {
			c.SetReadDeadline(time.Now().Add(5 * time.Second))
			_, err := r.ReadByte()
			read <- err
		}()
		select {
		case err := <-read:
			closed <- err
		case <-release:
			c.Write([]byte("OK 1073741824\n"))
			closed <- <-read
		}
	}()
	return v, accepted, release, closed
}

func TestVsockHandshakeCancelDuringAck(t *testing.T) {
	v, accepted, _, closed := delayedAckPeer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		c   net.Conn
		err error
	}
	done := make(chan result, 1)
	go func() {
		c, err := v.Dial(ctx)
		done <- result{c, err}
	}()
	select {
	case <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("peer never saw the CONNECT line")
	}
	start := time.Now()
	cancel()
	select {
	case r := <-done:
		if r.c != nil {
			r.c.Close()
			t.Fatal("a canceled dial returned a connection")
		}
		if !errors.Is(r.err, context.Canceled) {
			t.Fatalf("Dial error %v, want context.Canceled", r.err)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("Dial took %v to notice the cancellation", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Dial did not return after the cancellation")
	}
	select {
	case err := <-closed:
		if err != io.EOF {
			t.Fatalf("peer read %v, want io.EOF after the dialer closed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("peer did not see the connection close")
	}
}

func TestVsockHandshakeDeadlineDuringAck(t *testing.T) {
	v, accepted, _, _ := delayedAckPeer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	c, err := v.Dial(ctx)
	if c != nil {
		c.Close()
		t.Fatal("a timed-out dial returned a connection")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Dial error %v, want context.DeadlineExceeded", err)
	}
	select {
	case <-accepted:
	default:
		t.Fatal("peer never saw the CONNECT line")
	}
}

func TestVsockHandshakeCanceledBeforeDial(t *testing.T) {
	v, _, release, _ := delayedAckPeer(t)
	close(release)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c, err := v.Dial(ctx)
	if c != nil {
		c.Close()
		t.Fatal("a canceled dial returned a connection")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Dial error %v, want context.Canceled", err)
	}
}

func TestVsockHandshakeRefused(t *testing.T) {
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
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		bufio.NewReader(c).ReadString('\n')
		c.Write([]byte("NO\n"))
	}()
	c, err := v.Dial(context.Background())
	if c != nil {
		c.Close()
		t.Fatal("a refused handshake returned a connection")
	}
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("Dial error %v, want a refusal", err)
	}
}

// TestVsockHandshakeClearsDeadline covers the Dial branch that installs its
// own socket deadline (a ctx without a deadline) and checks that Dial clears
// that deadline once the handshake succeeds. The test never sets a deadline on
// the returned connection, so a leftover deadline makes the read time out.
func TestVsockHandshakeClearsDeadline(t *testing.T) {
	old := vsockHandshakeTimeout
	vsockHandshakeTimeout = 100 * time.Millisecond
	t.Cleanup(func() { vsockHandshakeTimeout = old })

	v, accepted, release, _ := delayedAckPeer(t)
	// The peer acknowledges at once; the idle connection then gets one byte
	// after the handshake timeout has passed.
	go func() {
		<-accepted
		close(release)
	}()
	c, err := v.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Outlive the handshake timeout, so a leftover socket deadline has expired.
	time.Sleep(3 * vsockHandshakeTimeout)
	if _, err := c.Write([]byte("x")); err != nil {
		t.Fatalf("write after the handshake timeout: %v", err)
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
	img := filepath.Join(t.TempDir(), "disk.ext4")
	content := make([]byte, 16<<20)
	if _, err := rand.Read(content); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(img, content, 0o600); err != nil {
		t.Fatal(err)
	}
	cp := img + ".copy"
	if err := copyDisk(context.Background(), img, cp); err != nil {
		t.Fatal(err)
	}
	if err := growDisk(context.Background(), cp, 64<<20); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(cp)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 64<<20 {
		t.Fatalf("size %d", len(got))
	}
	if !bytes.Equal(got[:16<<20], content) {
		t.Fatal("growDisk changed the existing image content")
	}
	for i, b := range got[16<<20:] {
		if b != 0 {
			t.Fatalf("grown region byte %d is %d, want 0", i, b)
		}
	}
	// A smaller or equal size keeps the image unchanged.
	for _, size := range []int64{1 << 20, 64 << 20} {
		if err := growDisk(context.Background(), cp, size); err != nil {
			t.Fatal(err)
		}
		after, err := os.ReadFile(cp)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(after, got) {
			t.Fatalf("growDisk to %d changed the image", size)
		}
	}
}

// TestGrowDiskKeepsExt4Image grows a real ext4 image and checks that the
// existing bytes stay identical: the host extends the file only, and the
// guest helper grows the file system online.
func TestGrowDiskKeepsExt4Image(t *testing.T) {
	mkfs := tool("mkfs.ext4")
	if _, err := os.Stat(mkfs); err != nil {
		t.Skip("mkfs.ext4 not installed")
	}
	img := filepath.Join(t.TempDir(), "disk.ext4")
	if err := os.WriteFile(img, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(img, 16<<20); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(mkfs, "-q", "-F", img).CombinedOutput(); err != nil {
		t.Fatalf("mkfs: %v %s", err, out)
	}
	before, err := os.ReadFile(img)
	if err != nil {
		t.Fatal(err)
	}
	cp := img + ".copy"
	if err := copyDisk(context.Background(), img, cp); err != nil {
		t.Fatal(err)
	}
	if err := growDisk(context.Background(), cp, 64<<20); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(cp)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 64<<20 || !bytes.Equal(after[:16<<20], before) {
		t.Fatalf("grown ext4 image size %d or content changed", len(after))
	}
}

// TestGrowDiskRunsNoFilesystemTool puts stub e2fsck, resize2fs and mkfs tools
// on PATH. growDisk must never run a file system tool on the disk image.
func TestGrowDiskRunsNoFilesystemTool(t *testing.T) {
	bin := t.TempDir()
	marker := filepath.Join(bin, "tool.ran")
	for _, name := range []string{"e2fsck", "resize2fs", "tune2fs", "debugfs", "mkfs.ext4"} {
		body := "#!/bin/sh\ntouch '" + marker + "'\nexit 0\n"
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	img := filepath.Join(t.TempDir(), "disk.ext4")
	if err := os.WriteFile(img, make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := growDisk(context.Background(), img, 4<<20); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(img); err != nil || st.Size() != 4<<20 {
		t.Fatalf("image %v %v", st, err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("growDisk ran a file system tool")
	}
}

// TestGrowDiskErrors covers a missing image and a canceled context.
func TestGrowDiskErrors(t *testing.T) {
	if err := growDisk(context.Background(), filepath.Join(t.TempDir(), "missing"), 4<<20); err == nil {
		t.Fatal("growDisk of a missing image succeeded")
	}
	img := filepath.Join(t.TempDir(), "disk.ext4")
	if err := os.WriteFile(img, make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := growDisk(ctx, img, 4<<20); !errors.Is(err, context.Canceled) {
		t.Fatalf("growDisk error %v, want context canceled", err)
	}
	if st, _ := os.Stat(img); st.Size() != 1<<20 {
		t.Fatalf("canceled growDisk changed the image size to %d", st.Size())
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

// TestCopyDiskEndsWithItsContext checks that a canceled context stops a disk
// copy and leaves no partial destination file, and that a live context copies
// the whole file.
func TestCopyDiskEndsWithItsContext(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, bytes.Repeat([]byte{7}, 3*diskCopyChunk+5), 0o600); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	dst := filepath.Join(dir, "dst")
	if err := copyDisk(canceled, src, dst); !errors.Is(err, context.Canceled) {
		t.Fatalf("copy on a canceled context = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(dst); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial copy remains after a canceled copy: %v", err)
	}
	// A context that ends between two chunks stops the chunked read. The
	// reader is exercised directly, because a reflink-capable file system
	// copies without chunks.
	mid, cancelMid := context.WithCancel(context.Background())
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	r := &ctxReader{ctx: mid, r: in}
	buf := make([]byte, 2*diskCopyChunk)
	if n, err := r.Read(buf); err != nil || n == 0 || n > diskCopyChunk {
		t.Fatalf("first read = %d, %v; want a chunk of at most %d bytes", n, err, diskCopyChunk)
	}
	cancelMid()
	if n, err := r.Read(buf); n != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("read after cancel = %d, %v", n, err)
	}
	if err := copyDisk(context.Background(), src, dst); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dst)
	if err != nil || len(got) != 3*diskCopyChunk+5 {
		t.Fatalf("copy = %d bytes, %v", len(got), err)
	}
}
