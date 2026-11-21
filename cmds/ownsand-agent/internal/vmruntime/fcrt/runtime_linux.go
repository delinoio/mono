// SPDX-License-Identifier: Apache-2.0

package fcrt

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/config"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/guestclient"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/launcher"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/netpolicy"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// Timeouts of a VM start.
const (
	apiTimeout   = 10 * time.Second
	readyTimeout = 60 * time.Second
)

// Runtime runs Firecracker microVMs.
type Runtime struct {
	opts   Options
	logger *slog.Logger
	mu     sync.Mutex
	vms    map[string]*vm
	// stopRefresh ends the network policy refresh loop; refreshDone
	// closes when it returned. Both are nil without a network manager.
	stopRefresh context.CancelFunc
	refreshDone chan struct{}
}

var _ vmruntime.Runtime = (*Runtime)(nil)

// New returns the runtime. It removes the tap devices and the nftables
// table of an earlier agent process, whose VMs ended with it, and runs the
// network policy refresh loop until Close.
func New(opts Options) (*Runtime, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Binary == "" {
		opts.Binary = "firecracker"
	}
	if p, err := exec.LookPath(opts.Binary); err == nil {
		opts.Binary = p
	}
	if opts.Network != nil && hasNetAdmin() {
		netpolicy.CleanupHost(opts.Logger)
	}
	r := &Runtime{opts: opts, logger: opts.Logger, vms: map[string]*vm{}}
	if opts.Network != nil {
		ctx, cancel := context.WithCancel(context.Background())
		r.stopRefresh, r.refreshDone = cancel, make(chan struct{})
		go func() {
			defer close(r.refreshDone)
			opts.Network.Run(ctx)
		}()
	}
	return r, nil
}

// Kind returns RuntimeKindFirecracker.
func (r *Runtime) Kind() config.RuntimeKind { return config.RuntimeKindFirecracker }

// hasNetAdmin reports whether the process has CAP_NET_ADMIN.
func hasNetAdmin() bool {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(sc.Text(), "CapEff:"); ok {
			var caps uint64
			fmt.Sscanf(strings.TrimSpace(rest), "%x", &caps)
			return caps&(1<<unix.CAP_NET_ADMIN) != 0
		}
	}
	return false
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// Capabilities runs the hypervisor checks.
func (r *Runtime) Capabilities(ctx context.Context) vmruntime.Capabilities {
	var caps vmruntime.Capabilities
	check := func(id string, passed bool, detail string) {
		caps.Checks = append(caps.Checks, &ownsandv1.HypervisorCheck{CheckId: id, Passed: passed, Detail: detail})
	}
	kvm := false
	if f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0); err == nil {
		f.Close()
		kvm = true
		check("kvm-access", true, "/dev/kvm is readable and writable")
	} else {
		check("kvm-access", false, "/dev/kvm is not accessible")
	}
	fc := false
	vctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	out, err := exec.CommandContext(vctx, r.opts.Binary, "--version").Output()
	cancel()
	if err == nil {
		v := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
		fc = true
		detail := v
		if !strings.Contains(v, PinnedVersion) {
			detail += "; this agent is tested with Firecracker " + PinnedVersion
		}
		check("firecracker-binary", true, detail)
	} else {
		check("firecracker-binary", false, "firecracker is not installed or does not run")
	}
	image := fileExists(r.opts.Kernel) && fileExists(r.opts.Rootfs)
	if image {
		check("linux-base-image", true, "kernel and root file system are present")
	} else {
		check("linux-base-image", false, "kernel or root file system is missing")
	}
	netOK := r.opts.Network != nil && hasNetAdmin()
	if netOK {
		check("outbound-network", true, "CAP_NET_ADMIN for tap devices and nftables")
	} else {
		check("outbound-network", false, "OUTBOUND sandboxes need CAP_NET_ADMIN, ip and nft")
	}
	if kvm && fc {
		caps.GuestOSes = []ownsandv1.GuestOs{ownsandv1.GuestOs_GUEST_OS_LINUX}
		if image {
			caps.BaseImages = []ownsandv1.BaseImage{ownsandv1.BaseImage_BASE_IMAGE_LINUX}
		}
	}
	return caps
}

type vm struct {
	id       string
	dir      string
	dirFile  *os.File
	cmd      *exec.Cmd
	exited   chan struct{}
	api      *apiClient
	restore  ownsandv1.SandboxRestorePath
	network  ownsandv1.NetworkMode
	vcpu     int32
	memMiB   int64
	mu       sync.Mutex
	paused   bool
	attached bool
}

func (v *vm) SandboxID() string                         { return v.id }
func (v *vm) RestorePath() ownsandv1.SandboxRestorePath { return v.restore }
func (v *vm) Exited() <-chan struct{}                   { return v.exited }

// path returns a socket path inside the VM directory that fits the unix
// socket path limit: /proc/self/fd/<dir fd>/<name> when the full path is
// too long.
func (v *vm) path(name string) string {
	full := filepath.Join(v.dir, name)
	if len(full) <= maxSocketPath {
		return full
	}
	return fmt.Sprintf("/proc/self/fd/%d/%s", v.dirFile.Fd(), name)
}

// Dial connects to the guest helper through the Firecracker vsock device:
// it connects to the device's unix socket and asks for the guest port.
func (v *vm) Dial(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", v.path(vsockSocket))
	if err != nil {
		return nil, err
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(10 * time.Second)
	}
	c.SetDeadline(deadline)
	if _, err := fmt.Fprintf(c, "CONNECT %d\n", GuestPort); err != nil {
		c.Close()
		return nil, err
	}
	// Read the acknowledgement byte by byte, so no byte of the stream is
	// consumed.
	var line []byte
	b := make([]byte, 1)
	for len(line) < 64 {
		if _, err := c.Read(b); err != nil {
			c.Close()
			return nil, fmt.Errorf("fcrt: vsock handshake: %w", err)
		}
		if b[0] == '\n' {
			break
		}
		line = append(line, b[0])
	}
	if !bytes.HasPrefix(line, []byte("OK ")) {
		c.Close()
		return nil, errors.New("fcrt: vsock handshake refused")
	}
	c.SetDeadline(time.Time{})
	return c, nil
}

func (r *Runtime) vmDir(id string) string { return filepath.Join(r.opts.VMsDir, id) }

// Create boots a microVM for the sandbox.
func (r *Runtime) Create(ctx context.Context, spec vmruntime.Spec) (vmruntime.VM, error) {
	if spec.GuestOS != ownsandv1.GuestOs_GUEST_OS_LINUX {
		return nil, fmt.Errorf("%w: Firecracker runs only Linux guests", vmruntime.ErrUnsupported)
	}
	if spec.NetworkMode == ownsandv1.NetworkMode_NETWORK_MODE_OUTBOUND && (r.opts.Network == nil || !hasNetAdmin()) {
		return nil, fmt.Errorf("%w: OUTBOUND sandboxes need CAP_NET_ADMIN", vmruntime.ErrUnsupported)
	}
	if spec.Snapshot == nil && !(fileExists(r.opts.Kernel) && fileExists(r.opts.Rootfs)) {
		return nil, fmt.Errorf("%w: the Linux base image is missing", vmruntime.ErrUnsupported)
	}
	if err := os.RemoveAll(spec.Dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(spec.Dir, 0o700); err != nil {
		return nil, err
	}
	dirFile, err := os.Open(spec.Dir)
	if err != nil {
		return nil, err
	}
	v := &vm{id: spec.SandboxID, dir: spec.Dir, dirFile: dirFile, network: spec.NetworkMode,
		vcpu: spec.Resources.GetVcpuCount(), memMiB: spec.Resources.GetMemoryMib()}
	// cleanup releases everything of a failed create: the sandbox manager
	// records the sandbox as FAILED and never calls Delete for it, so the
	// registration and the directory handle must not outlive this call.
	cleanup := func() {
		r.stop(v)
		r.mu.Lock()
		if r.vms[spec.SandboxID] == v {
			delete(r.vms, spec.SandboxID)
		}
		r.mu.Unlock()
		v.dirFile.Close()
		os.RemoveAll(spec.Dir)
	}

	memoryRestore := spec.Snapshot != nil && spec.Snapshot.HasMemoryState && spec.MemoryRestoreAllowed &&
		spec.NetworkMode == ownsandv1.NetworkMode_NETWORK_MODE_NONE &&
		spec.Snapshot.Resources.GetVcpuCount() == spec.Resources.GetVcpuCount() &&
		spec.Snapshot.Resources.GetMemoryMib() == spec.Resources.GetMemoryMib()
	src := r.opts.Rootfs
	if spec.Snapshot != nil {
		src = filepath.Join(spec.Snapshot.Dir, snapDisk)
	}
	if err := copyDisk(src, filepath.Join(spec.Dir, diskFile)); err != nil {
		cleanup()
		return nil, fmt.Errorf("fcrt: prepare disk: %w", err)
	}
	if !memoryRestore {
		if err := growDisk(ctx, filepath.Join(spec.Dir, diskFile), spec.Resources.GetDiskGib()<<30); err != nil {
			cleanup()
			return nil, fmt.Errorf("fcrt: resize disk: %w", err)
		}
	}
	r.mu.Lock()
	r.vms[spec.SandboxID] = v
	r.mu.Unlock()

	if memoryRestore {
		err := r.restoreMemory(ctx, v, spec.Snapshot)
		if err == nil {
			v.restore = ownsandv1.SandboxRestorePath_SANDBOX_RESTORE_PATH_MEMORY_STATE
			if err := r.waitGuest(ctx, v); err != nil {
				cleanup()
				return nil, err
			}
			clockCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			if err := guestclient.New(v.Dial).SetClock(clockCtx, time.Now()); err != nil {
				r.logger.Warn("set guest clock after memory restore failed", "sandbox_id", v.id, "error", err)
			}
			cancel()
			r.logger.Info("microVM restored from memory state", "sandbox_id", v.id)
			return v, nil
		}
		// The hypervisor could not read the saved state: boot the snapshot
		// disk cold.
		r.logger.Warn("memory-state restore failed; booting the snapshot disk cold", "sandbox_id", v.id, "error", err)
		r.stop(v)
		if err := growDisk(ctx, filepath.Join(spec.Dir, diskFile), spec.Resources.GetDiskGib()<<30); err != nil {
			cleanup()
			return nil, err
		}
	}
	if err := r.coldBoot(ctx, v, spec); err != nil {
		cleanup()
		return nil, err
	}
	if spec.Snapshot != nil {
		v.restore = ownsandv1.SandboxRestorePath_SANDBOX_RESTORE_PATH_DISK_COLD_BOOT
	} else {
		v.restore = ownsandv1.SandboxRestorePath_SANDBOX_RESTORE_PATH_BASE_IMAGE
	}
	if err := r.waitGuest(ctx, v); err != nil {
		cleanup()
		return nil, err
	}
	r.logger.Info("microVM started", "sandbox_id", v.id, "restore_path", v.restore.String())
	return v, nil
}

// startProcess starts firecracker for v, bound to the agent process.
func (r *Runtime) startProcess(ctx context.Context, v *vm) error {
	logf, err := os.OpenFile(filepath.Join(v.dir, logFile), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	os.Remove(filepath.Join(v.dir, apiSocket))
	os.Remove(filepath.Join(v.dir, vsockSocket))
	cmd := exec.Command(r.opts.Binary, "--api-sock", apiSocket, "--id", idPrefix+v.id)
	cmd.Dir = v.dir
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := launcher.Start(cmd); err != nil {
		logf.Close()
		return fmt.Errorf("fcrt: start firecracker: %w", err)
	}
	exited := make(chan struct{})
	go func() {
		cmd.Wait()
		logf.Close()
		close(exited)
	}()
	v.mu.Lock()
	v.cmd, v.exited = cmd, exited
	v.mu.Unlock()
	v.api = newAPIClient(v.path(apiSocket))
	actx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	return v.api.waitReady(actx, exited)
}

func (r *Runtime) coldBoot(ctx context.Context, v *vm, spec vmruntime.Spec) error {
	if err := r.startProcess(ctx, v); err != nil {
		return err
	}
	kernel, err := filepath.Abs(r.opts.Kernel)
	if err != nil {
		return err
	}
	args := "console=ttyS0 reboot=k panic=1 pci=off random.trust_cpu=on init=/sbin/init"
	var tap string
	if spec.NetworkMode == ownsandv1.NetworkMode_NETWORK_MODE_OUTBOUND {
		link, t, err := r.opts.Network.Attach(v.id)
		if err != nil {
			return err
		}
		v.attached = true
		tap = t
		args += " " + link.KernelIPArg()
	}
	steps := []struct {
		method, path string
		body         any
	}{
		{http.MethodPut, "/machine-config", machineConfig{VCPUCount: spec.Resources.GetVcpuCount(), MemSizeMiB: spec.Resources.GetMemoryMib()}},
		{http.MethodPut, "/boot-source", bootSource{KernelImagePath: kernel, BootArgs: args}},
		{http.MethodPut, "/drives/rootfs", drive{DriveID: "rootfs", PathOnHost: diskFile, IsRootDevice: true}},
		{http.MethodPut, "/vsock", vsock{GuestCID: guestCID, UDSPath: vsockSocket}},
		{http.MethodPut, "/entropy", struct{}{}},
	}
	if tap != "" {
		steps = append(steps, struct {
			method, path string
			body         any
		}{http.MethodPut, "/network-interfaces/eth0", networkInterface{IfaceID: "eth0", HostDevName: tap}})
	}
	steps = append(steps, struct {
		method, path string
		body         any
	}{http.MethodPut, "/actions", action{ActionType: "InstanceStart"}})
	for _, s := range steps {
		if err := v.api.call(ctx, s.method, s.path, s.body); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runtime) restoreMemory(ctx context.Context, v *vm, snap *vmruntime.SnapshotSource) error {
	if err := r.startProcess(ctx, v); err != nil {
		return err
	}
	state, _ := filepath.Abs(filepath.Join(snap.Dir, snapState))
	mem, _ := filepath.Abs(filepath.Join(snap.Dir, snapMemory))
	return v.api.call(ctx, http.MethodPut, "/snapshot/load", snapshotLoad{
		SnapshotPath:  state,
		MemBackend:    memBackend{BackendType: "File", BackendPath: mem},
		ResumeVM:      true,
		VsockOverride: &vsockOverride{UDSPath: vsockSocket},
		ClockRealtime: true,
	})
}

// waitGuest waits until the guest helper answers CheckReady.
func (r *Runtime) waitGuest(ctx context.Context, v *vm) error {
	rctx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	client := guestclient.New(v.Dial)
	defer client.Close()
	done := make(chan error, 1)
	go func() {
		_, err := client.WaitReady(rctx, 100*time.Millisecond)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("fcrt: guest helper not ready: %w", err)
		}
		return nil
	case <-v.exited:
		return errors.New("fcrt: firecracker exited during boot")
	}
}

func (r *Runtime) lookup(id string) (*vm, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.vms[id]
	if !ok {
		return nil, fmt.Errorf("fcrt: sandbox %s has no microVM", id)
	}
	return v, nil
}

func (r *Runtime) setState(ctx context.Context, m vmruntime.VM, state string, paused bool) error {
	v, err := r.lookup(m.SandboxID())
	if err != nil {
		return err
	}
	if err := v.api.call(ctx, http.MethodPatch, "/vm", vmState{State: state}); err != nil {
		return err
	}
	v.mu.Lock()
	v.paused = paused
	v.mu.Unlock()
	return nil
}

// Pause flushes the guest page cache and then stops the vCPUs and keeps the
// memory. The flush makes a later disk copy of the paused VM (Snapshot of
// an already paused sandbox) hold every write completed before the pause.
func (r *Runtime) Pause(ctx context.Context, m vmruntime.VM) error {
	v, err := r.lookup(m.SandboxID())
	if err != nil {
		return err
	}
	if err := syncGuest(ctx, v); err != nil {
		r.logger.Warn("guest sync before pause failed", "sandbox_id", v.id, "error", err)
	}
	return r.setState(ctx, m, "Paused", true)
}

// Resume continues the vCPUs.
func (r *Runtime) Resume(ctx context.Context, m vmruntime.VM) error {
	return r.setState(ctx, m, "Resumed", false)
}

// Snapshot writes the disk and, for NONE sandboxes, the memory state.
// OUTBOUND sandboxes get disk-only snapshots: their guest network
// configuration is in memory and would not match the link of a restored
// sandbox.
func (r *Runtime) Snapshot(ctx context.Context, m vmruntime.VM, dir string) (vmruntime.SnapshotInfo, error) {
	v, err := r.lookup(m.SandboxID())
	if err != nil {
		return vmruntime.SnapshotInfo{}, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return vmruntime.SnapshotInfo{}, err
	}
	v.mu.Lock()
	wasPaused := v.paused
	v.mu.Unlock()
	// A paused VM was flushed by its Pause; Pause flushes a running one.
	if !wasPaused {
		if err := r.Pause(ctx, m); err != nil {
			return vmruntime.SnapshotInfo{}, err
		}
		defer func() {
			if err := r.Resume(context.Background(), m); err != nil {
				r.logger.Error("resume after snapshot failed", "sandbox_id", v.id, "error", err)
			}
		}()
	}
	info := vmruntime.SnapshotInfo{}
	if v.network == ownsandv1.NetworkMode_NETWORK_MODE_NONE {
		absDir, _ := filepath.Abs(dir)
		if err := v.api.call(ctx, http.MethodPut, "/snapshot/create", snapshotCreate{
			SnapshotType: "Full",
			SnapshotPath: filepath.Join(absDir, snapState),
			MemFilePath:  filepath.Join(absDir, snapMemory),
		}); err != nil {
			return vmruntime.SnapshotInfo{}, err
		}
		info.HasMemoryState = true
	}
	if err := copyDisk(filepath.Join(v.dir, diskFile), filepath.Join(dir, snapDisk)); err != nil {
		return vmruntime.SnapshotInfo{}, err
	}
	for _, name := range []string{snapState, snapMemory, snapDisk} {
		if st, err := os.Stat(filepath.Join(dir, name)); err == nil {
			info.SizeBytes += st.Size()
		}
	}
	return info, nil
}

// stop kills the firecracker process of v and releases its network.
func (r *Runtime) stop(v *vm) {
	v.mu.Lock()
	cmd, exited := v.cmd, v.exited
	v.cmd = nil
	v.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			r.logger.Error("firecracker did not stop", "sandbox_id", v.id)
		}
	}
	if v.attached {
		r.opts.Network.Detach(v.id)
		v.attached = false
	}
}

// Delete stops the microVM of the sandbox, also one that outlived an
// earlier agent process, and removes its files.
func (r *Runtime) Delete(_ context.Context, sandboxID string) error {
	r.mu.Lock()
	v, ok := r.vms[sandboxID]
	delete(r.vms, sandboxID)
	r.mu.Unlock()
	if ok {
		r.stop(v)
		v.dirFile.Close()
	}
	for _, pid := range orphanPIDs()[sandboxID] {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	if r.opts.VMsDir != "" {
		return os.RemoveAll(r.vmDir(sandboxID))
	}
	return nil
}

// Running lists the sandboxes with a running microVM, including microVMs
// of an earlier agent process.
func (r *Runtime) Running(context.Context) ([]string, error) {
	seen := map[string]bool{}
	r.mu.Lock()
	for id, v := range r.vms {
		select {
		case <-v.exited:
		default:
			seen[id] = true
		}
	}
	r.mu.Unlock()
	for id := range orphanPIDs() {
		seen[id] = true
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	return ids, nil
}

// orphanPIDs finds firecracker processes that this agent started (by their
// --id argument), keyed by sandbox ID.
func orphanPIDs() map[string][]int {
	out := map[string][]int{}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return out
	}
	self := os.Getpid()
	for _, e := range entries {
		var pid int
		if _, err := fmt.Sscanf(e.Name(), "%d", &pid); err != nil || pid == self {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue
		}
		args := strings.Split(string(raw), "\x00")
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "--id" && strings.HasPrefix(args[i+1], idPrefix) && strings.Contains(filepath.Base(args[0]), "firecracker") {
				out[strings.TrimPrefix(args[i+1], idPrefix)] = append(out[strings.TrimPrefix(args[i+1], idPrefix)], pid)
			}
		}
	}
	return out
}

// Close stops every microVM and the network policy refresh loop.
func (r *Runtime) Close() error {
	if r.stopRefresh != nil {
		r.stopRefresh()
		<-r.refreshDone
	}
	r.mu.Lock()
	ids := make([]string, 0, len(r.vms))
	for id := range r.vms {
		ids = append(ids, id)
	}
	r.mu.Unlock()
	var errs []error
	for _, id := range ids {
		errs = append(errs, r.Delete(context.Background(), id))
	}
	return errors.Join(errs...)
}

// copyDisk copies a disk image, with a reflink where the file system
// supports it.
func copyDisk(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if unix.IoctlFileClone(int(out.Fd()), int(in.Fd())) == nil {
		return out.Close()
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func tool(name string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	for _, dir := range []string{"/usr/sbin", "/sbin"} {
		if p := filepath.Join(dir, name); fileExists(p) {
			return p
		}
	}
	return name
}

// growDisk grows the disk image to size bytes and resizes its ext4 file
// system offline (e2fsck and resize2fs of e2fsprogs). A smaller size keeps
// the image.
func growDisk(ctx context.Context, path string, size int64) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if size <= st.Size() {
		return nil
	}
	if err := os.Truncate(path, size); err != nil {
		return err
	}
	// e2fsck exits with 1 when it corrected the file system.
	if out, err := exec.CommandContext(ctx, tool("e2fsck"), "-fy", path).CombinedOutput(); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() > 1 {
			return fmt.Errorf("e2fsck: %v: %s", err, bytes.TrimSpace(out))
		}
	}
	if out, err := exec.CommandContext(ctx, tool("resize2fs"), path).CombinedOutput(); err != nil {
		return fmt.Errorf("resize2fs: %v: %s", err, bytes.TrimSpace(out))
	}
	return nil
}

// NewRuntime returns the runtime as a vmruntime.Runtime.
func NewRuntime(opts Options) (vmruntime.Runtime, error) {
	return New(opts)
}
