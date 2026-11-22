// SPDX-License-Identifier: FSL-1.1-ALv2

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
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/guestsync"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/launcher"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// Timeouts of a VM start.
const (
	apiTimeout   = 10 * time.Second
	readyTimeout = 60 * time.Second
	// exitTimeout bounds the wait for a killed firecracker process to end.
	exitTimeout = 10 * time.Second
)

// Runtime runs Firecracker microVMs.
type Runtime struct {
	opts   Options
	logger *slog.Logger
	mu     sync.Mutex
	vms    map[string]*vm
	// host reads the process table; signalGroup kills the process group of
	// a tracked child; stopTimeout bounds the wait for a killed process to
	// end. Tests replace them with fixtures.
	host        procHost
	signalGroup func(pid int) error
	stopTimeout time.Duration
	// waitExit observes the exit of a tracked child without reaping it, and
	// observeRetry paces its retries after a failure (see launch).
	waitExit     func(pid int) error
	observeRetry time.Duration
	// stopRefresh ends the network policy refresh loop; refreshDone
	// closes when it returned. Both are nil without a network manager.
	stopRefresh context.CancelFunc
	refreshDone chan struct{}
	// hostCleanup deletes the tap devices and the nftables table of the
	// network policy (see CleanupHost); tests pass a fake.
	hostCleanup func() error
	// hasNetAdmin and lookPath are seams so that tests can stub the host
	// capability and the executable lookup; New sets the real ones.
	hasNetAdmin func() bool
	lookPath    func(string) (string, error)
}

var _ vmruntime.Runtime = (*Runtime)(nil)

// New returns the runtime. It removes the tap devices and the nftables
// table of an earlier agent process, whose VMs ended with it, and runs the
// network policy refresh loop until Close.
func New(opts Options) (*Runtime, error) {
	network := opts.Network
	return newRuntime(opts, func() error {
		// Without CAP_NET_ADMIN this process cannot change the host
		// network, so the clean-up is skipped.
		if network == nil || !hasNetAdmin() {
			return nil
		}
		return network.CleanupHost()
	})
}

// newRuntime builds the runtime with an injectable host network clean-up.
// With a network manager it calls hostCleanup once, before the refresh loop
// starts, and again from CleanupHost; without a manager it calls neither.
// New passes the privileged host clean-up, so that tests can pass a fake and
// never reach the live host network, whatever the capabilities of the test
// process are.
func newRuntime(opts Options, hostCleanup func() error) (*Runtime, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Binary == "" {
		opts.Binary = "firecracker"
	}
	if p, err := exec.LookPath(opts.Binary); err == nil {
		opts.Binary = p
	}
	if opts.Network != nil {
		// Best effort: the clean-up logs each failed resource, and the
		// agent start does not depend on the result.
		_ = hostCleanup()
	}
	r := &Runtime{
		opts: opts, logger: opts.Logger, vms: map[string]*vm{},
		hostCleanup: hostCleanup,
		hasNetAdmin: hasNetAdmin, lookPath: exec.LookPath,
		host:        newLinuxProcs(),
		signalGroup: func(pid int) error { return syscall.Kill(-pid, syscall.SIGKILL) },
		stopTimeout: exitTimeout,
		waitExit:    waitExitNoReap, observeRetry: observeRetryDelay,
	}
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

var _ vmruntime.HostCleaner = (*Runtime)(nil)

// CleanupHost deletes the sandbox tap devices and the nftables table of the
// network policy. The device removal calls it after it deleted every
// sandbox: a removed agent exits for good, so the clean-up of the next agent
// start never runs. Without a network manager it does nothing. It returns
// the failures of the resources that it could not delete.
func (r *Runtime) CleanupHost() error {
	if r.opts.Network == nil || r.hostCleanup == nil {
		return nil
	}
	return r.hostCleanup()
}

// Kind returns RuntimeKindFirecracker.
func (r *Runtime) Kind() config.RuntimeKind { return config.RuntimeKindFirecracker }

// outboundProblem returns why OUTBOUND sandboxes cannot run, or "" when the
// network manager, CAP_NET_ADMIN and the ip and nft executables that the
// network policy runs (found through PATH, like the policy does) are present.
func (r *Runtime) outboundProblem() string {
	if r.opts.Network == nil {
		return "no network manager"
	}
	if !r.hasNetAdmin() {
		return "CAP_NET_ADMIN is missing"
	}
	var missing []string
	for _, tool := range []string{"ip", "nft"} {
		if _, err := r.lookPath(tool); err != nil {
			missing = append(missing, tool)
		}
	}
	if len(missing) > 0 {
		return "missing executables: " + strings.Join(missing, ", ")
	}
	return ""
}

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
	if problem := r.outboundProblem(); problem == "" {
		check("outbound-network", true, "CAP_NET_ADMIN, ip and nft for tap devices and nftables")
	} else {
		check("outbound-network", false, "OUTBOUND sandboxes need CAP_NET_ADMIN, ip and nft: "+problem)
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
	launch   *launch
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
	if spec.NetworkMode == ownsandv1.NetworkMode_NETWORK_MODE_OUTBOUND {
		if problem := r.outboundProblem(); problem != "" {
			return nil, fmt.Errorf("%w: OUTBOUND sandboxes need CAP_NET_ADMIN, ip and nft: %s", vmruntime.ErrUnsupported, problem)
		}
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
	// A firecracker process that survives the kill keeps its registration and
	// directory (with the launch record), so that Close and the next agent
	// start can still find and stop it.
	cleanup := func() {
		if err := r.stop(v); err != nil {
			r.logger.Error("failed create left a firecracker process running; keeping its directory", "sandbox_id", v.id, "error", err)
			return
		}
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
		if err := r.stop(v); err != nil {
			cleanup()
			return nil, fmt.Errorf("fcrt: stop the failed restore process: %w", err)
		}
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
	logf, err := vmruntime.OpenCappedLog(filepath.Join(v.dir, logFile), vmruntime.ConsoleLogLimit, r.logger, v.id, vmruntime.ConsoleLogKindFirecracker)
	if err != nil {
		return err
	}
	os.Remove(filepath.Join(v.dir, apiSocket))
	os.Remove(filepath.Join(v.dir, vsockSocket))
	cmd := exec.Command(r.opts.Binary, "--api-sock", apiSocket, "--id", idPrefix+v.id)
	cmd.Dir = v.dir
	// One writer value for both streams: os/exec then uses a single pipe and
	// one copying goroutine, and Wait returns after the copy ended, so the
	// close below never races a write.
	cmd.Stdout, cmd.Stderr = logf, logf
	// Output goes through a pipe now, so a stray descendant that keeps the
	// pipe open must not keep Wait (and v.exited) from returning.
	cmd.WaitDelay = 5 * time.Second
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := launcher.Start(cmd); err != nil {
		logf.Close()
		return fmt.Errorf("fcrt: start firecracker: %w", err)
	}
	exited := make(chan struct{})
	l := &launch{
		pid: cmd.Process.Pid, exited: exited, observe: r.waitExit,
		wait:   func() { cmd.Wait() },
		finish: func() { logf.Close() },
	}
	v.mu.Lock()
	v.cmd, v.launch, v.exited = cmd, l, exited
	v.mu.Unlock()
	go l.supervise(r.logger, v.id, r.observeRetry)
	// Without the record a later agent process cannot prove that it owns the
	// process, so a failed write fails the start; the caller stops the
	// process.
	if err := writeLaunchRecord(r.host, v.dir, v.id, cmd.Process.Pid); err != nil {
		return fmt.Errorf("fcrt: write launch record: %w", err)
	}
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
	if err := guestsync.Sync(ctx, v.id, v.Dial); err != nil {
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
func (r *Runtime) Snapshot(ctx context.Context, m vmruntime.VM, dir string) (info vmruntime.SnapshotInfo, err error) {
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
			rerr := r.Resume(context.Background(), m)
			if rerr == nil {
				return
			}
			r.logger.Error("resume after snapshot failed", "sandbox_id", v.id, "error", rerr)
			// A VM left paused while the manager reports it running must not
			// yield a READY snapshot; an earlier capture error takes priority.
			if err == nil {
				info = vmruntime.SnapshotInfo{}
				err = fmt.Errorf("fcrt: resume after snapshot: %w", rerr)
			}
		}()
	}
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

// stop kills the firecracker process of v and releases its network. It
// returns an error when the process is still alive after the bounded wait;
// v then keeps its process reference, so that a later stop tries again.
func (r *Runtime) stop(v *vm) error {
	v.mu.Lock()
	cmd, l := v.cmd, v.launch
	v.mu.Unlock()
	var err error
	if cmd != nil && cmd.Process != nil {
		exited := l.exited
		select {
		case <-exited:
		default:
			// Signal only while this launch still owns the PID: the launch
			// gate refuses the signal once its reaper committed to reap the
			// child (the exited channel stays open until the output drained
			// and the log closed), and the reaper waits for a signal in
			// flight before it reaps. The signal is the group signal, so
			// same-group descendants end with the leader.
			state, kerr := l.signal(r.signalGroup)
			if state != signalAllowed {
				r.logger.Warn("firecracker group signal skipped", "sandbox_id", v.id, "pid", cmd.Process.Pid, "reason", string(state))
			} else if kerr != nil && !errors.Is(kerr, syscall.ESRCH) {
				r.logger.Error("signal firecracker failed", "sandbox_id", v.id, "pid", cmd.Process.Pid, "error", kerr)
			}
			timer := time.NewTimer(r.stopTimeout)
			select {
			case <-exited:
			case <-timer.C:
				err = fmt.Errorf("fcrt: firecracker process %d of sandbox %s did not stop within %s", cmd.Process.Pid, v.id, r.stopTimeout)
				r.logger.Error("firecracker did not stop", "sandbox_id", v.id, "pid", cmd.Process.Pid)
			}
			timer.Stop()
		}
		if err == nil {
			v.mu.Lock()
			if v.cmd == cmd {
				v.cmd = nil
			}
			v.mu.Unlock()
		}
	}
	// Detach is idempotent. A guest that survives a failed stop loses its
	// network anyway.
	if v.attached {
		r.opts.Network.Detach(v.id)
		v.attached = false
	}
	return err
}

// Delete stops the microVM of the sandbox, also one that outlived an
// earlier agent process, and removes its files. It succeeds only after the
// firecracker process ended: while a process survives the bounded wait,
// Delete returns an error and keeps the registration and the VM directory,
// and a repeated Delete tries again. Deleting an absent VM succeeds.
func (r *Runtime) Delete(_ context.Context, sandboxID string) error {
	r.mu.Lock()
	v, tracked := r.vms[sandboxID]
	r.mu.Unlock()
	if tracked {
		if err := r.stop(v); err != nil {
			return err
		}
	}
	procs, verr := r.ownedProcesses(sandboxID)
	if verr != nil {
		// A process that could not be inspected may still run: keep the
		// registration, the directory and the launch record, which is the
		// proof a retry needs.
		for _, p := range procs {
			p.handle.close()
		}
		return verr
	}
	if err := r.killOwned(procs); err != nil {
		return err
	}
	if tracked {
		r.mu.Lock()
		if r.vms[sandboxID] == v {
			delete(r.vms, sandboxID)
		}
		r.mu.Unlock()
		v.dirFile.Close()
	}
	if r.opts.VMsDir != "" {
		return os.RemoveAll(r.vmDir(sandboxID))
	}
	return nil
}

// killOwned kills verified processes and waits for each to end. It closes
// the handles and returns an error naming the processes that survived.
func (r *Runtime) killOwned(procs []*ownedProcess) error {
	var errs []error
	for _, p := range procs {
		if err := p.handle.kill(); err != nil && !errors.Is(err, errProcessGone) {
			r.logger.Error("signal firecracker of an earlier agent process failed", "sandbox_id", p.sandboxID, "pid", p.pid, "error", err)
		}
		if !p.handle.waitExit(r.stopTimeout) {
			r.logger.Error("firecracker of an earlier agent process did not stop", "sandbox_id", p.sandboxID, "pid", p.pid)
			errs = append(errs, fmt.Errorf("fcrt: firecracker process %d of sandbox %s did not stop within %s", p.pid, p.sandboxID, r.stopTimeout))
		}
		p.handle.close()
	}
	return errors.Join(errs...)
}

// ownedProcesses returns the live firecracker processes that this agent
// started and that its VM directories prove, for the sandbox or, with an
// empty ID, for every VM directory. The launch record is the only proof of
// ownership: a command line is never enough. A process that carries the
// --id argument of a sandbox but fails the proof is not returned and is
// logged with the reason. A failure to inspect a process or its record is
// not such a reason: it is returned as an error, together with the processes
// that were verified, because the VM may still run. The caller closes the
// handles.
func (r *Runtime) ownedProcesses(sandboxID string) ([]*ownedProcess, error) {
	ids := []string{sandboxID}
	var listErr error
	if sandboxID == "" {
		ids = nil
		if r.opts.VMsDir != "" {
			if entries, err := os.ReadDir(r.opts.VMsDir); err == nil {
				for _, e := range entries {
					ids = append(ids, e.Name())
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				r.logger.Error("list VM directories failed", "error", err)
				listErr = err
			}
		}
	} else if r.opts.VMsDir == "" {
		ids = nil
	}
	var owned []*ownedProcess
	reasons := map[string]launchSkipReason{}
	verified := map[int]bool{}
	var errs []error
	if listErr != nil {
		// An incomplete inventory must not look authoritative: the caller
		// would treat the VMs it missed as gone.
		errs = append(errs, fmt.Errorf("fcrt: list VM directories: %w", listErr))
	}
	for _, id := range ids {
		p, reason, err := verifyLaunch(r.host, r.vmDir(id), id)
		if errors.Is(err, errVerifyIO) {
			r.logger.Error("cannot verify the launch record of a VM", "sandbox_id", id, "reason", string(reason), "error", err)
			errs = append(errs, fmt.Errorf("fcrt: verify sandbox %s: %w", id, err))
			continue
		}
		if p != nil {
			owned = append(owned, p)
			verified[p.pid] = true
			continue
		}
		reasons[id] = reason
		// A record of a process that ended is the normal leftover of a VM
		// that stopped with its agent.
		if reason != skipProcessGone && reason != skipRecordMissing {
			r.logger.Warn("launch record does not prove a firecracker process", "sandbox_id", id, "reason", string(reason), "error", err)
		}
	}
	r.logUnverified(sandboxID, reasons, verified)
	return owned, errors.Join(errs...)
}

// logUnverified logs the firecracker-looking processes that carry the --id
// argument of a sandbox and fail the launch proof, so that an operator sees
// why a process survives the clean-up.
func (r *Runtime) logUnverified(only string, reasons map[string]launchSkipReason, verified map[int]bool) {
	pids, err := r.host.listPIDs()
	if err != nil {
		return
	}
	self := os.Getpid()
	for _, pid := range pids {
		if pid == self || verified[pid] {
			continue
		}
		args, err := r.host.cmdline(pid)
		if err != nil {
			continue
		}
		for i := 0; i+1 < len(args); i++ {
			if args[i] != "--id" || !strings.HasPrefix(args[i+1], idPrefix) {
				continue
			}
			id := strings.TrimPrefix(args[i+1], idPrefix)
			if only != "" && id != only {
				continue
			}
			reason, ok := reasons[id]
			if !ok {
				reason = skipRecordMissing
			}
			r.logger.Warn("not signalling a process without a verified launch record", "sandbox_id", id, "pid", pid, "reason", string(reason))
		}
	}
}

// Running lists the sandboxes with a running microVM, including microVMs
// of an earlier agent process that a launch record proves.
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
	procs, err := r.ownedProcesses("")
	for _, p := range procs {
		seen[p.sandboxID] = true
		p.handle.close()
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	// An uninspectable VM directory may hold a surviving VM: report the
	// failure instead of a list that looks complete.
	return ids, err
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
