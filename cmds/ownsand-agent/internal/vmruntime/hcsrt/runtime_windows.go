// SPDX-License-Identifier: FSL-1.1-ALv2

package hcsrt

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/Microsoft/go-winio"
	"github.com/Microsoft/go-winio/pkg/guid"
	"golang.org/x/sys/windows"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/config"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/guestclient"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/guestsync"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/netpolicy"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/netstack"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// Timeouts of the runtime.
const (
	readyTimeout  = 60 * time.Second
	stopTimeout   = 30 * time.Second
	dialTimeout   = 10 * time.Second
	tunnelTimeout = 20 * time.Second
	removeTimeout = 10 * time.Second
)

// guestLink is the link of every OUTBOUND sandbox: each sandbox has its own
// network stack, so every sandbox uses the same addresses.
var guestLink = mustLink(netpolicy.LinkFor(1))

func mustLink(l netpolicy.Link, err error) netpolicy.Link {
	if err != nil {
		panic(err)
	}
	return l
}

// Runtime runs Hyper-V VMs through the Host Compute Service.
type Runtime struct {
	opts   Options
	logger *slog.Logger
	filter *netpolicy.Filter
	stop   context.CancelFunc
	done   chan struct{}
	mu     sync.Mutex
	vms    map[string]*vm
}

var _ vmruntime.Runtime = (*Runtime)(nil)

// New returns the runtime and starts the network policy filter refresh.
func New(opts Options) (vmruntime.Runtime, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &Runtime{opts: opts, logger: opts.Logger, filter: netpolicy.NewFilter(opts.Logger),
		stop: cancel, done: make(chan struct{}), vms: map[string]*vm{}}
	go func() {
		defer close(r.done)
		r.filter.Run(ctx)
	}()
	return r, nil
}

// Kind returns RuntimeKindHyperV.
func (r *Runtime) Kind() config.RuntimeKind { return config.RuntimeKindHyperV }

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// hyperVCheck reports whether the HCS is reachable from this process.
func hyperVCheck() (bool, string) {
	if err := hcsLoad(); err != nil {
		return false, fmt.Sprintf("the Host Compute Service API (computecore.dll) is not available; enable Hyper-V: %v", err)
	}
	if !windows.GetCurrentProcessToken().IsElevated() {
		return false, "the agent process is not elevated; the Host Compute Service needs an Administrator or SYSTEM"
	}
	systems, err := enumerateSystems(Owner)
	if err != nil {
		return false, fmt.Sprintf("the Host Compute Service is not reachable: %v", err)
	}
	return true, fmt.Sprintf("the Host Compute Service is reachable (%d compute systems of %s)", len(systems), Owner)
}

// Capabilities runs the hypervisor checks.
func (r *Runtime) Capabilities(context.Context) vmruntime.Capabilities {
	var caps vmruntime.Capabilities
	check := func(id string, passed bool, detail string) {
		caps.Checks = append(caps.Checks, &ownsandv1.HypervisorCheck{CheckId: id, Passed: passed, Detail: detail})
	}
	hv, detail := hyperVCheck()
	check(checkHyperV, hv, detail)
	image := fileExists(r.opts.Kernel) && fileExists(r.opts.Rootfs)
	if image {
		check(checkImage, true, "kernel and root file system are present")
	} else {
		check(checkImage, false, "kernel or root file system is missing")
	}
	check(checkNetwork, true, "OUTBOUND sandboxes use the agent's userspace network stack through a Hyper-V socket tunnel")
	if hv {
		caps.GuestOSes = []ownsandv1.GuestOs{ownsandv1.GuestOs_GUEST_OS_LINUX}
		if image {
			caps.BaseImages = []ownsandv1.BaseImage{ownsandv1.BaseImage_BASE_IMAGE_LINUX}
		}
	}
	return caps
}

type vm struct {
	id        string
	dir       string
	restore   ownsandv1.SandboxRestorePath
	network   ownsandv1.NetworkMode
	runtimeID guid.GUID
	// exited is closed when the compute system ends.
	exited   chan struct{}
	exitOnce sync.Once
	// stopping is closed when the runtime stops the VM, which ends the
	// console copy and the tunnel.
	stopping    chan struct{}
	stopOnce    sync.Once
	consoleDone chan struct{}
	mu          sync.Mutex
	sys         *system
	net         *netstack.Network
	netDone     chan struct{}
	paused      bool
}

func (v *vm) markExited() { v.exitOnce.Do(func() { close(v.exited) }) }

func (v *vm) SandboxID() string                         { return v.id }
func (v *vm) RestorePath() ownsandv1.SandboxRestorePath { return v.restore }
func (v *vm) Exited() <-chan struct{}                   { return v.exited }

// dialPort connects to a vsock port of the guest through a Hyper-V socket.
func (v *vm) dialPort(ctx context.Context, port uint32) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	go func() {
		select {
		case <-v.exited:
			cancel()
		case <-ctx.Done():
		}
	}()
	conn, err := winio.Dial(ctx, &winio.HvsockAddr{VMID: v.runtimeID, ServiceID: winio.VsockServiceID(port)})
	if err != nil {
		select {
		case <-v.exited:
			return nil, errors.New("hcsrt: the VM stopped")
		default:
		}
		return nil, fmt.Errorf("hcsrt: connect to guest port %d: %w", port, err)
	}
	return conn, nil
}

// Dial connects to the guest helper.
func (v *vm) Dial(ctx context.Context) (net.Conn, error) { return v.dialPort(ctx, GuestPort) }

func (r *Runtime) vmDir(id string) string { return filepath.Join(r.opts.VMsDir, id) }

func systemID(sandboxID string) string { return systemIDPrefix + sandboxID }

var procCopyFileW = windows.NewLazySystemDLL("kernel32.dll").NewProc("CopyFileW")

// copyFile copies src to dst with CopyFileW, which clones blocks on ReFS
// volumes, and makes dst writable.
func copyFile(src, dst string) error {
	_ = os.Remove(dst)
	s, err := windows.UTF16PtrFromString(src)
	if err != nil {
		return err
	}
	d, err := windows.UTF16PtrFromString(dst)
	if err != nil {
		return err
	}
	if ok, _, e := procCopyFileW.Call(uintptr(unsafe.Pointer(s)), uintptr(unsafe.Pointer(d)), 1); ok == 0 {
		return fmt.Errorf("hcsrt: copy %s: %w", src, e)
	}
	return os.Chmod(dst, 0o600)
}

// removeAll removes a VM directory. The Hyper-V worker process can hold the
// disk open for a moment after the compute system ended, so the removal
// retries sharing violations for up to removeTimeout.
func removeAll(dir string) error {
	deadline := time.Now().Add(removeTimeout)
	for {
		err := os.RemoveAll(dir)
		if err == nil || time.Now().After(deadline) {
			return err
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// consolePipe is the named pipe of the COM1 port of a sandbox VM.
func consolePipe(sandboxID string) string { return `\\.\pipe\ownsand-` + sandboxID + `-com1` }

// Create boots a VM for the sandbox.
func (r *Runtime) Create(ctx context.Context, spec vmruntime.Spec) (vmruntime.VM, error) {
	if spec.GuestOS != ownsandv1.GuestOs_GUEST_OS_LINUX {
		return nil, fmt.Errorf("%w: this agent version runs only Linux guests on Windows hosts", vmruntime.ErrUnsupported)
	}
	if !fileExists(r.opts.Kernel) {
		return nil, fmt.Errorf("%w: the Linux kernel is missing", vmruntime.ErrUnsupported)
	}
	if spec.Snapshot == nil && !fileExists(r.opts.Rootfs) {
		return nil, fmt.Errorf("%w: the Linux base image is missing", vmruntime.ErrUnsupported)
	}
	if err := hcsLoad(); err != nil {
		return nil, fmt.Errorf("%w: the Host Compute Service is not available: %v", vmruntime.ErrUnsupported, err)
	}
	start := time.Now()
	if err := removeAll(spec.Dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(spec.Dir, 0o700); err != nil {
		return nil, err
	}
	v := &vm{id: spec.SandboxID, dir: spec.Dir, network: spec.NetworkMode,
		exited: make(chan struct{}), stopping: make(chan struct{})}
	// cleanup releases everything of a failed create: the sandbox manager
	// records the sandbox as FAILED and never calls Delete for it.
	cleanup := func() {
		r.stopVM(v)
		r.mu.Lock()
		if r.vms[spec.SandboxID] == v {
			delete(r.vms, spec.SandboxID)
		}
		r.mu.Unlock()
		if err := removeAll(spec.Dir); err != nil {
			r.logger.Warn("remove VM directory failed", "sandbox_id", v.id, "error", err)
		}
	}
	// A compute system with this ID is left from a failed earlier attempt.
	r.stopOrphan(spec.SandboxID)

	if err := grantVMGroup(spec.Dir, accessReadWrite, true); err != nil {
		cleanup()
		return nil, err
	}
	if err := grantVMGroup(r.opts.Kernel, accessRead, false); err != nil {
		cleanup()
		return nil, err
	}
	src, hasFooter := r.opts.Rootfs, false
	if spec.Snapshot != nil {
		src, hasFooter = filepath.Join(spec.Snapshot.Dir, snapDisk), true
	}
	disk := filepath.Join(spec.Dir, diskFile)
	if err := copyFile(src, disk); err != nil {
		cleanup()
		return nil, fmt.Errorf("hcsrt: prepare disk: %w", err)
	}
	size, err := makeFixedVHD(disk, spec.Resources.GetDiskGib()<<30, hasFooter)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("hcsrt: prepare disk: %w", err)
	}
	if err := grantVMGroup(disk, accessReadWrite, false); err != nil {
		cleanup()
		return nil, err
	}
	diskReady := time.Now()
	r.mu.Lock()
	r.vms[spec.SandboxID] = v
	r.mu.Unlock()

	vs := vmSpec{Kernel: r.opts.Kernel, Disk: disk, ConsolePipe: consolePipe(v.id),
		VCPUs: spec.Resources.GetVcpuCount(), MemoryMiB: spec.Resources.GetMemoryMib()}
	if spec.NetworkMode == ownsandv1.NetworkMode_NETWORK_MODE_OUTBOUND {
		vs.Tunnel = &guestLink
	}
	sys, err := createSystem(systemID(v.id), newComputeSystemDoc(vs))
	if err != nil {
		cleanup()
		return nil, err
	}
	v.mu.Lock()
	v.sys = sys
	v.mu.Unlock()
	go func() {
		<-sys.exited
		v.markExited()
	}()
	props, err := sys.properties()
	if err == nil {
		v.runtimeID, err = guid.FromString(props.RuntimeID)
	}
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("hcsrt: read the VM runtime ID: %w", err)
	}
	if err := sys.start(); err != nil {
		cleanup()
		return nil, err
	}
	v.consoleDone = make(chan struct{})
	go r.copyConsole(v)
	booted := time.Now()
	if err := r.waitGuest(ctx, v); err != nil {
		cleanup()
		return nil, err
	}
	ready := time.Now()
	if spec.NetworkMode == ownsandv1.NetworkMode_NETWORK_MODE_OUTBOUND {
		if err := r.attachNetwork(ctx, v); err != nil {
			cleanup()
			return nil, err
		}
	}
	if spec.Snapshot != nil {
		v.restore = ownsandv1.SandboxRestorePath_SANDBOX_RESTORE_PATH_DISK_COLD_BOOT
	} else {
		v.restore = ownsandv1.SandboxRestorePath_SANDBOX_RESTORE_PATH_BASE_IMAGE
	}
	r.logger.Info("VM started", "sandbox_id", v.id, "restore_path", v.restore.String(),
		"runtime_id", v.runtimeID.String(), "disk_bytes", size,
		"disk_ms", diskReady.Sub(start).Milliseconds(), "create_ms", booted.Sub(diskReady).Milliseconds(),
		"guest_ready_ms", ready.Sub(booted).Milliseconds(), "total_ms", time.Since(start).Milliseconds())
	return v, nil
}

// copyConsole copies the COM1 output of v to console.log for boot
// debugging. The Hyper-V worker process serves the named pipe of the port,
// so the runtime connects as a client once the VM runs.
func (r *Runtime) copyConsole(v *vm) {
	defer close(v.consoleDone)
	out, err := os.OpenFile(filepath.Join(v.dir, consoleFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		r.logger.Warn("open console log failed", "sandbox_id", v.id, "error", err)
		return
	}
	defer out.Close()
	for {
		timeout := time.Second
		conn, err := winio.DialPipe(consolePipe(v.id), &timeout)
		if err == nil {
			go func() {
				select {
				case <-v.stopping:
				case <-v.exited:
				}
				conn.Close()
			}()
			_, _ = io.Copy(out, conn)
			conn.Close()
		}
		select {
		case <-v.stopping:
			return
		case <-v.exited:
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
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
			return fmt.Errorf("hcsrt: guest helper not ready: %w", err)
		}
		return nil
	case <-v.exited:
		return errors.New("hcsrt: the VM stopped during boot")
	}
}

// attachNetwork starts the userspace network stack of v and connects it to
// the guest's network tunnel. The first connection must succeed; later
// connection losses redial while the VM runs.
func (r *Runtime) attachNetwork(ctx context.Context, v *vm) error {
	mac, err := netstack.RandomMAC()
	if err != nil {
		return err
	}
	stack, err := netstack.New(netstack.Options{SandboxID: v.id, Link: guestLink, GuestMAC: mac, Filter: r.filter, Logger: r.logger})
	if err != nil {
		return err
	}
	v.mu.Lock()
	v.net = stack
	v.netDone = make(chan struct{})
	v.mu.Unlock()
	tctx, cancel := context.WithTimeout(ctx, tunnelTimeout)
	defer cancel()
	var conn net.Conn
	for {
		conn, err = v.dialPort(tctx, TunnelPort)
		if err == nil {
			break
		}
		select {
		case <-tctx.Done():
			close(v.netDone)
			return fmt.Errorf("hcsrt: connect the network tunnel: %w", err)
		case <-v.exited:
			close(v.netDone)
			return errors.New("hcsrt: the VM stopped before the network tunnel connected")
		case <-time.After(100 * time.Millisecond):
		}
	}
	go r.runTunnel(v, stack, conn)
	return nil
}

// runTunnel serves the tunnel connections of v until the VM stops.
func (r *Runtime) runTunnel(v *vm, stack *netstack.Network, conn net.Conn) {
	defer close(v.netDone)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-v.stopping:
		case <-v.exited:
		}
		cancel()
	}()
	for {
		if conn != nil {
			r.logger.Info("network tunnel connected", "sandbox_id", v.id)
			err := stack.Serve(ctx, conn, netstack.FramingLengthPrefixed)
			conn.Close()
			if ctx.Err() != nil {
				return
			}
			r.logger.Warn("network tunnel ended; reconnecting", "sandbox_id", v.id, "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
		c, err := v.dialPort(ctx, TunnelPort)
		if err != nil {
			r.logger.Warn("network tunnel reconnect failed", "sandbox_id", v.id, "error", err)
			conn = nil
			continue
		}
		conn = c
	}
}

func (r *Runtime) lookup(id string) (*vm, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.vms[id]
	if !ok {
		return nil, fmt.Errorf("hcsrt: sandbox %s has no VM", id)
	}
	return v, nil
}

func (v *vm) system() (*system, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.sys == nil {
		return nil, fmt.Errorf("hcsrt: sandbox %s has no running VM", v.id)
	}
	return v.sys, nil
}

// Pause flushes the guest page cache and then stops the vCPUs and keeps the
// memory. The flush makes a later disk copy of the paused VM hold every
// write completed before the pause.
func (r *Runtime) Pause(ctx context.Context, m vmruntime.VM) error {
	v, err := r.lookup(m.SandboxID())
	if err != nil {
		return err
	}
	sys, err := v.system()
	if err != nil {
		return err
	}
	if err := guestsync.Sync(ctx, v.id, v.Dial); err != nil {
		r.logger.Warn("guest sync before pause failed", "sandbox_id", v.id, "error", err)
	}
	if err := sys.pause(); err != nil {
		return err
	}
	v.mu.Lock()
	v.paused = true
	v.mu.Unlock()
	r.logger.Info("VM paused", "sandbox_id", v.id)
	return nil
}

// Resume continues the vCPUs.
func (r *Runtime) Resume(_ context.Context, m vmruntime.VM) error {
	v, err := r.lookup(m.SandboxID())
	if err != nil {
		return err
	}
	sys, err := v.system()
	if err != nil {
		return err
	}
	if err := sys.resume(); err != nil {
		return err
	}
	v.mu.Lock()
	v.paused = false
	v.mu.Unlock()
	r.logger.Info("VM resumed", "sandbox_id", v.id)
	return nil
}

// Snapshot writes the disk. Snapshots are disk-only in this version: the
// HCS save state (HcsSaveComputeSystem) is not used, so a create from a
// snapshot always boots the disk cold.
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
	start := time.Now()
	dst := filepath.Join(dir, snapDisk)
	if err := copyFile(filepath.Join(v.dir, diskFile), dst); err != nil {
		return vmruntime.SnapshotInfo{}, err
	}
	st, err := os.Stat(dst)
	if err != nil {
		return vmruntime.SnapshotInfo{}, err
	}
	r.logger.Info("disk snapshot written", "sandbox_id", v.id, "bytes", st.Size(), "copy_ms", time.Since(start).Milliseconds())
	return vmruntime.SnapshotInfo{SizeBytes: st.Size()}, nil
}

// stopSystem terminates sys, waits until it exited and closes its handle.
func (r *Runtime) stopSystem(sandboxID string, sys *system) {
	if err := sys.terminate(); err != nil {
		r.logger.Warn("terminate VM failed", "sandbox_id", sandboxID, "error", err)
	}
	select {
	case <-sys.exited:
	case <-time.After(stopTimeout):
		r.logger.Error("VM did not stop", "sandbox_id", sandboxID)
	}
	sys.close()
	// The exit callback ends with the handle; a system whose exit event did
	// not arrive counts as exited once its last handle is closed.
	sys.markExited()
}

// stopVM stops the VM of v, if it runs, and its network stack.
func (r *Runtime) stopVM(v *vm) {
	v.stopOnce.Do(func() { close(v.stopping) })
	v.mu.Lock()
	sys := v.sys
	v.sys = nil
	stack, netDone := v.net, v.netDone
	v.net, v.netDone = nil, nil
	consoleDone := v.consoleDone
	v.mu.Unlock()
	if sys != nil {
		r.stopSystem(v.id, sys)
	} else {
		// No compute system was created: nothing else closes exited.
		v.markExited()
	}
	if stack != nil {
		stack.Close()
	}
	for _, ch := range []chan struct{}{netDone, consoleDone} {
		if ch == nil {
			continue
		}
		select {
		case <-ch:
		case <-time.After(stopTimeout):
			r.logger.Warn("VM helper goroutine did not end", "sandbox_id", v.id)
		}
	}
}

// stopOrphan stops a compute system of the sandbox that no tracked VM owns,
// for example one that an earlier agent process left.
func (r *Runtime) stopOrphan(sandboxID string) {
	sys, err := openSystem(systemID(sandboxID))
	if err != nil {
		if !isNotFoundOrStopped(err) {
			r.logger.Debug("open compute system failed", "sandbox_id", sandboxID, "error", err)
		}
		return
	}
	r.logger.Warn("stopping a compute system without a tracked VM", "sandbox_id", sandboxID)
	r.stopSystem(sandboxID, sys)
}

// Delete stops the VM of the sandbox and removes its files.
func (r *Runtime) Delete(_ context.Context, sandboxID string) error {
	r.mu.Lock()
	v, ok := r.vms[sandboxID]
	delete(r.vms, sandboxID)
	r.mu.Unlock()
	if ok {
		r.stopVM(v)
	} else if hcsLoad() == nil {
		r.stopOrphan(sandboxID)
	}
	if r.opts.VMsDir != "" {
		return removeAll(r.vmDir(sandboxID))
	}
	return nil
}

// Running lists the sandboxes with a running or paused VM, including
// compute systems of an earlier agent process (owner ownsand-agent), so
// that the agent stops them after a restart.
func (r *Runtime) Running(context.Context) ([]string, error) {
	r.mu.Lock()
	seen := map[string]bool{}
	ids := make([]string, 0, len(r.vms))
	for id, v := range r.vms {
		select {
		case <-v.exited:
		default:
			ids = append(ids, id)
			seen[id] = true
		}
	}
	r.mu.Unlock()
	if hcsLoad() != nil {
		return ids, nil
	}
	systems, err := enumerateSystems(Owner)
	if err != nil {
		// Without HCS access no VM can run; the hyper-v check reports why.
		r.logger.Warn("list compute systems failed", "error", err)
		return ids, nil
	}
	for _, s := range systems {
		id, ok := strings.CutPrefix(s.ID, systemIDPrefix)
		if !ok || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids, nil
}

// Close stops every VM and the filter refresh.
func (r *Runtime) Close() error {
	r.stop()
	<-r.done
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
