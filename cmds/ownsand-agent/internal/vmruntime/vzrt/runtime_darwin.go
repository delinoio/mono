// SPDX-License-Identifier: Apache-2.0

//go:build darwin && cgo

package vzrt

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/Code-Hex/vz/v3"
	"golang.org/x/sys/unix"

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
	readyTimeout = 60 * time.Second
	// macOSReadyTimeout covers a macOS cold boot, which takes far longer
	// than a Linux boot.
	macOSReadyTimeout = 5 * time.Minute
	stopTimeout       = 10 * time.Second
	dialTimeout       = 10 * time.Second
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

// Runtime runs Virtualization.framework VMs.
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

// Kind returns RuntimeKindVirtualization.
func (r *Runtime) Kind() config.RuntimeKind { return config.RuntimeKindVirtualization }

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// hasEntitlement reports whether the agent executable is signed with the
// com.apple.security.virtualization entitlement, which every
// Virtualization.framework VM needs.
func hasEntitlement(ctx context.Context) (bool, string) {
	exe, err := os.Executable()
	if err != nil {
		return false, "cannot find the agent executable"
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/codesign", "-d", "--entitlements", "-", "--xml", exe).Output()
	if err != nil {
		return false, "the agent executable is not code-signed"
	}
	if !bytes.Contains(out, []byte("com.apple.security.virtualization")) {
		return false, "the agent executable lacks the com.apple.security.virtualization entitlement"
	}
	return true, "the agent executable has the com.apple.security.virtualization entitlement"
}

// Capabilities runs the hypervisor checks.
func (r *Runtime) Capabilities(ctx context.Context) vmruntime.Capabilities {
	var caps vmruntime.Capabilities
	check := func(id string, passed bool, detail string) {
		caps.Checks = append(caps.Checks, &ownsandv1.HypervisorCheck{CheckId: id, Passed: passed, Detail: detail})
	}
	framework := vz.VirtualMachineConfigurationMaximumAllowedCPUCount() > 0
	if framework {
		check(checkFramework, true, fmt.Sprintf("Virtualization.framework allows up to %d vCPUs", vz.VirtualMachineConfigurationMaximumAllowedCPUCount()))
	} else {
		check(checkFramework, false, "Virtualization.framework is not available")
	}
	entitled, detail := hasEntitlement(ctx)
	check(checkEntitlement, entitled, detail)
	image := fileExists(r.opts.Kernel) && fileExists(r.opts.Rootfs)
	if image {
		check(checkImage, true, "kernel and root file system are present")
	} else {
		check(checkImage, false, "kernel or root file system is missing")
	}
	check(checkNetwork, true, "OUTBOUND sandboxes use the agent's userspace network stack")
	macModel, macDetail := r.macOSImage()
	check(checkMacOSImage, macModel != nil, macDetail)
	if framework && entitled {
		caps.GuestOSes = []ownsandv1.GuestOs{ownsandv1.GuestOs_GUEST_OS_LINUX}
		if image {
			caps.BaseImages = []ownsandv1.BaseImage{ownsandv1.BaseImage_BASE_IMAGE_LINUX}
		}
		if macOSGuestsSupported {
			caps.MacOSGuestsSupported = true
			caps.GuestOSes = append(caps.GuestOSes, ownsandv1.GuestOs_GUEST_OS_MACOS)
			if macModel != nil {
				caps.BaseImages = append(caps.BaseImages, ownsandv1.BaseImage_BASE_IMAGE_MACOS)
				caps.MacOSHardwareModels = [][]byte{macModel}
			}
		}
	}
	return caps
}

type vm struct {
	id        string
	dir       string
	guestOS   ownsandv1.GuestOs
	machine   *vz.VirtualMachine
	machineID []byte
	// hardwareModel is the hardware model of a macOS guest.
	hardwareModel []byte
	exited        <-chan struct{}
	restore       ownsandv1.SandboxRestorePath
	network       ownsandv1.NetworkMode
	net           *netstack.Network
	netConn       net.Conn
	vcpu          int32
	memMiB        int64
	mu            sync.Mutex
	paused        bool
}

func (v *vm) SandboxID() string                         { return v.id }
func (v *vm) RestorePath() ownsandv1.SandboxRestorePath { return v.restore }
func (v *vm) Exited() <-chan struct{}                   { return v.exited }

// current returns the VM object, or an error after the VM was stopped.
func (v *vm) current() (*vz.VirtualMachine, <-chan struct{}, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.machine == nil {
		return nil, nil, fmt.Errorf("vzrt: sandbox %s has no running VM", v.id)
	}
	return v.machine, v.exited, nil
}

// Dial connects to the guest helper through the virtio socket device.
func (v *vm) Dial(ctx context.Context) (net.Conn, error) {
	machine, exited, err := v.current()
	if err != nil {
		return nil, err
	}
	return DialGuest(ctx, machine, GuestPort, exited)
}

func (r *Runtime) vmDir(id string) string { return filepath.Join(r.opts.VMsDir, id) }

// copyFile copies src to dst with an APFS clone where possible.
func copyFile(src, dst string) error {
	_ = os.Remove(dst)
	if err := unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// growFile enlarges a disk image to size bytes; the guest grows its file
// system at boot. A smaller size keeps the image.
func growFile(path string, size int64) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if size <= st.Size() {
		return nil
	}
	return os.Truncate(path, size)
}

// Create boots a VM for the sandbox.
func (r *Runtime) Create(ctx context.Context, spec vmruntime.Spec) (vmruntime.VM, error) {
	switch spec.GuestOS {
	case ownsandv1.GuestOs_GUEST_OS_LINUX:
	case ownsandv1.GuestOs_GUEST_OS_MACOS:
		return r.createMacOS(ctx, spec)
	default:
		return nil, fmt.Errorf("%w: macOS hosts run Linux and macOS guests", vmruntime.ErrUnsupported)
	}
	if !fileExists(r.opts.Kernel) {
		return nil, fmt.Errorf("%w: the Linux kernel is missing", vmruntime.ErrUnsupported)
	}
	if spec.Snapshot == nil && !fileExists(r.opts.Rootfs) {
		return nil, fmt.Errorf("%w: the Linux base image is missing", vmruntime.ErrUnsupported)
	}
	if err := os.RemoveAll(spec.Dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(spec.Dir, 0o700); err != nil {
		return nil, err
	}
	v := &vm{id: spec.SandboxID, dir: spec.Dir, network: spec.NetworkMode, guestOS: spec.GuestOS,
		vcpu: spec.Resources.GetVcpuCount(), memMiB: spec.Resources.GetMemoryMib()}
	// cleanup releases everything of a failed create: the sandbox manager
	// records the sandbox as FAILED and never calls Delete for it.
	cleanup := func() {
		r.stopVM(v)
		r.mu.Lock()
		if r.vms[spec.SandboxID] == v {
			delete(r.vms, spec.SandboxID)
		}
		r.mu.Unlock()
		os.RemoveAll(spec.Dir)
	}

	memoryRestore := spec.Snapshot != nil && spec.Snapshot.HasMemoryState && spec.MemoryRestoreAllowed &&
		memoryStateSupported &&
		spec.NetworkMode == ownsandv1.NetworkMode_NETWORK_MODE_NONE &&
		spec.Snapshot.Resources.GetVcpuCount() == spec.Resources.GetVcpuCount() &&
		spec.Snapshot.Resources.GetMemoryMib() == spec.Resources.GetMemoryMib()
	src := r.opts.Rootfs
	if spec.Snapshot != nil {
		src = filepath.Join(spec.Snapshot.Dir, snapDisk)
	}
	disk := filepath.Join(spec.Dir, diskFile)
	if err := copyFile(src, disk); err != nil {
		cleanup()
		return nil, fmt.Errorf("vzrt: prepare disk: %w", err)
	}
	r.mu.Lock()
	r.vms[spec.SandboxID] = v
	r.mu.Unlock()

	if memoryRestore {
		err := r.restoreMemory(ctx, v, spec)
		if err == nil {
			v.restore = ownsandv1.SandboxRestorePath_SANDBOX_RESTORE_PATH_MEMORY_STATE
			clockCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			if err := guestclient.New(v.Dial).SetClock(clockCtx, time.Now()); err != nil {
				r.logger.Warn("set guest clock after memory restore failed", "sandbox_id", v.id, "error", err)
			}
			cancel()
			r.logger.Info("VM restored from memory state", "sandbox_id", v.id)
			return v, nil
		}
		// Virtualization.framework could not restore the saved state: boot
		// the snapshot disk cold.
		r.logger.Warn("memory-state restore failed; booting the snapshot disk cold", "sandbox_id", v.id, "error", err)
		r.stopVM(v)
		if err := copyFile(src, disk); err != nil {
			cleanup()
			return nil, fmt.Errorf("vzrt: prepare disk: %w", err)
		}
	}
	if err := growFile(disk, spec.Resources.GetDiskGib()<<30); err != nil {
		cleanup()
		return nil, fmt.Errorf("vzrt: resize disk: %w", err)
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
	r.logger.Info("VM started", "sandbox_id", v.id, "restore_path", v.restore.String())
	return v, nil
}

// configure builds the VM configuration of v. machineID is the generic
// machine identifier to reuse (for a memory-state restore), or nil for a
// new one.
func (r *Runtime) configure(v *vm, spec vmruntime.Spec, machineID []byte) (*vz.VirtualMachineConfiguration, error) {
	cmdline := "console=hvc0 root=/dev/vda rw init=/sbin/init panic=1 random.trust_cpu=on"
	if spec.NetworkMode == ownsandv1.NetworkMode_NETWORK_MODE_OUTBOUND {
		cmdline += " " + guestLink.KernelIPArg()
	}
	boot, err := vz.NewLinuxBootLoader(r.opts.Kernel, vz.WithCommandLine(cmdline))
	if err != nil {
		return nil, err
	}
	cfg, err := vz.NewVirtualMachineConfiguration(boot, uint(spec.Resources.GetVcpuCount()), uint64(spec.Resources.GetMemoryMib())<<20)
	if err != nil {
		return nil, err
	}
	var id *vz.GenericMachineIdentifier
	if machineID != nil {
		id, err = vz.NewGenericMachineIdentifierWithData(machineID)
	} else {
		id, err = vz.NewGenericMachineIdentifier()
	}
	if err != nil {
		return nil, err
	}
	platform, err := vz.NewGenericPlatformConfiguration(vz.WithGenericMachineIdentifier(id))
	if err != nil {
		return nil, err
	}
	cfg.SetPlatformVirtualMachineConfiguration(platform)
	v.machineID = id.DataRepresentation()

	attachment, err := vz.NewDiskImageStorageDeviceAttachmentWithCacheAndSync(filepath.Join(v.dir, diskFile), false,
		vz.DiskImageCachingModeAutomatic, vz.DiskImageSynchronizationModeFull)
	if err != nil {
		return nil, err
	}
	block, err := vz.NewVirtioBlockDeviceConfiguration(attachment)
	if err != nil {
		return nil, err
	}
	cfg.SetStorageDevicesVirtualMachineConfiguration([]vz.StorageDeviceConfiguration{block})

	sock, err := vz.NewVirtioSocketDeviceConfiguration()
	if err != nil {
		return nil, err
	}
	cfg.SetSocketDevicesVirtualMachineConfiguration([]vz.SocketDeviceConfiguration{sock})

	entropy, err := vz.NewVirtioEntropyDeviceConfiguration()
	if err != nil {
		return nil, err
	}
	cfg.SetEntropyDevicesVirtualMachineConfiguration([]*vz.VirtioEntropyDeviceConfiguration{entropy})

	console, err := vz.NewFileSerialPortAttachment(filepath.Join(v.dir, consoleFile), true)
	if err != nil {
		return nil, err
	}
	serial, err := vz.NewVirtioConsoleDeviceSerialPortConfiguration(console)
	if err != nil {
		return nil, err
	}
	cfg.SetSerialPortsVirtualMachineConfiguration([]*vz.VirtioConsoleDeviceSerialPortConfiguration{serial})

	if spec.NetworkMode == ownsandv1.NetworkMode_NETWORK_MODE_OUTBOUND {
		mac, err := netstack.RandomMAC()
		if err != nil {
			return nil, err
		}
		dev, err := r.attachNetwork(v, mac)
		if err != nil {
			return nil, err
		}
		cfg.SetNetworkDevicesVirtualMachineConfiguration([]*vz.VirtioNetworkDeviceConfiguration{dev})
	}
	if ok, err := cfg.Validate(); !ok || err != nil {
		return nil, fmt.Errorf("vzrt: invalid VM configuration: %w", err)
	}
	return cfg, nil
}

// attachNetwork starts the userspace network stack of v and returns the
// network device with the MAC address mac that is attached to it through a
// datagram socket pair.
func (r *Runtime) attachNetwork(v *vm, mac net.HardwareAddr) (*vz.VirtioNetworkDeviceConfiguration, error) {
	vmFile, conn, err := datagramPair()
	if err != nil {
		return nil, err
	}
	stack, err := netstack.New(netstack.Options{SandboxID: v.id, Link: guestLink, GuestMAC: mac, Filter: r.filter, Logger: r.logger})
	if err != nil {
		vmFile.Close()
		conn.Close()
		return nil, err
	}
	dev, err := networkDevice(vmFile, mac)
	if err != nil {
		stack.Close()
		vmFile.Close()
		conn.Close()
		return nil, err
	}
	v.net, v.netConn = stack, conn
	go func() {
		if err := stack.Serve(context.Background(), conn, netstack.FramingDatagram); err != nil {
			r.logger.Warn("sandbox network link ended", "sandbox_id", v.id, "error", err)
		}
	}()
	return dev, nil
}

// datagramPair returns the VM end of a datagram socket pair for a
// file-handle network attachment and the agent end as a connection.
func datagramPair() (*os.File, net.Conn, error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM, 0)
	if err != nil {
		return nil, nil, err
	}
	// Virtualization.framework recommends a send buffer of at least twice
	// the receive buffer for datagram attachments.
	for _, fd := range fds {
		_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_SNDBUF, 1<<20)
		_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF, 4<<20)
		unix.CloseOnExec(fd)
	}
	vmFile := os.NewFile(uintptr(fds[0]), "vz-network")
	agentFile := os.NewFile(uintptr(fds[1]), "agent-network")
	conn, err := net.FileConn(agentFile)
	agentFile.Close()
	if err != nil {
		vmFile.Close()
		return nil, nil, err
	}
	return vmFile, conn, nil
}

// networkDevice returns a virtio network device with the MAC address mac on
// a file-handle attachment of vmFile.
func networkDevice(vmFile *os.File, mac net.HardwareAddr) (*vz.VirtioNetworkDeviceConfiguration, error) {
	attachment, err := vz.NewFileHandleNetworkDeviceAttachment(vmFile)
	if err != nil {
		return nil, err
	}
	dev, err := vz.NewVirtioNetworkDeviceConfiguration(attachment)
	if err != nil {
		return nil, err
	}
	vzMAC, err := vz.NewMACAddress(mac)
	if err != nil {
		return nil, err
	}
	dev.SetMACAddress(vzMAC)
	return dev, nil
}

// NewIdleNetworkDevice returns a network device with the MAC address mac
// whose link carries no traffic, for the image build: it lets the guest
// create its network service without network access. Close the returned
// closer after the VM stopped.
func NewIdleNetworkDevice(mac net.HardwareAddr) (*vz.VirtioNetworkDeviceConfiguration, io.Closer, error) {
	vmFile, conn, err := datagramPair()
	if err != nil {
		return nil, nil, err
	}
	dev, err := networkDevice(vmFile, mac)
	if err != nil {
		vmFile.Close()
		conn.Close()
		return nil, nil, err
	}
	// Drain the guest's frames so the socket never fills.
	go func() {
		buf := make([]byte, 65536)
		for {
			if _, err := conn.Read(buf); err != nil {
				return
			}
		}
	}()
	return dev, closers{vmFile, conn}, nil
}

type closers []io.Closer

func (c closers) Close() error {
	var errs []error
	for _, x := range c {
		errs = append(errs, x.Close())
	}
	return errors.Join(errs...)
}

// newMachine creates the VM object of v and watches its state.
func (r *Runtime) newMachine(v *vm, cfg *vz.VirtualMachineConfiguration) error {
	machine, err := vz.NewVirtualMachine(cfg)
	if err != nil {
		return err
	}
	exited := WatchStopped(machine)
	v.mu.Lock()
	v.machine, v.exited = machine, exited
	v.mu.Unlock()
	return nil
}

func (r *Runtime) coldBoot(ctx context.Context, v *vm, spec vmruntime.Spec) error {
	cfg, err := r.configure(v, spec, nil)
	if err != nil {
		return err
	}
	if err := r.newMachine(v, cfg); err != nil {
		return err
	}
	if err := v.machine.Start(); err != nil {
		return fmt.Errorf("vzrt: start VM: %w", err)
	}
	return r.waitGuest(ctx, v)
}

// waitGuest waits until the guest helper answers CheckReady.
func (r *Runtime) waitGuest(ctx context.Context, v *vm) error {
	timeout := readyTimeout
	if v.guestOS == ownsandv1.GuestOs_GUEST_OS_MACOS {
		timeout = macOSReadyTimeout
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
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
			return fmt.Errorf("vzrt: guest helper not ready: %w", err)
		}
		return nil
	case <-v.exited:
		return errors.New("vzrt: the VM stopped during boot")
	}
}

func (r *Runtime) lookup(id string) (*vm, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.vms[id]
	if !ok {
		return nil, fmt.Errorf("vzrt: sandbox %s has no VM", id)
	}
	return v, nil
}

// waitState waits until the VM reaches want.
func waitState(ctx context.Context, machine *vz.VirtualMachine, want vz.VirtualMachineState) error {
	t := time.NewTicker(10 * time.Millisecond)
	defer t.Stop()
	for machine.State() != want {
		select {
		case <-ctx.Done():
			return fmt.Errorf("vzrt: VM state %v, want %v: %w", machine.State(), want, ctx.Err())
		case <-t.C:
		}
	}
	return nil
}

// Pause flushes the guest page cache and then stops the vCPUs and keeps the
// memory. The flush makes a later disk copy of the paused VM hold every
// write completed before the pause.
func (r *Runtime) Pause(ctx context.Context, m vmruntime.VM) error {
	v, err := r.lookup(m.SandboxID())
	if err != nil {
		return err
	}
	if err := guestsync.Sync(ctx, v.id, v.Dial); err != nil {
		r.logger.Warn("guest sync before pause failed", "sandbox_id", v.id, "error", err)
	}
	machine, _, err := v.current()
	if err != nil {
		return err
	}
	if err := machine.Pause(); err != nil {
		return fmt.Errorf("vzrt: pause: %w", err)
	}
	if err := waitState(ctx, machine, vz.VirtualMachineStatePaused); err != nil {
		return err
	}
	v.mu.Lock()
	v.paused = true
	v.mu.Unlock()
	return nil
}

// Resume continues the vCPUs.
func (r *Runtime) Resume(ctx context.Context, m vmruntime.VM) error {
	v, err := r.lookup(m.SandboxID())
	if err != nil {
		return err
	}
	machine, _, err := v.current()
	if err != nil {
		return err
	}
	if err := machine.Resume(); err != nil {
		return fmt.Errorf("vzrt: resume: %w", err)
	}
	if err := waitState(ctx, machine, vz.VirtualMachineStateRunning); err != nil {
		return err
	}
	v.mu.Lock()
	v.paused = false
	v.mu.Unlock()
	return nil
}

// Snapshot writes the disk and, for NONE sandboxes on Apple silicon, the
// memory state. OUTBOUND sandboxes get disk-only snapshots: their guest
// network configuration is in memory and would not match a restored
// sandbox's network stack.
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
	if v.guestOS == ownsandv1.GuestOs_GUEST_OS_MACOS {
		return r.snapshotMacOS(v, dir)
	}
	info := vmruntime.SnapshotInfo{}
	if memoryStateSupported && v.network == ownsandv1.NetworkMode_NETWORK_MODE_NONE {
		machine, _, err := v.current()
		if err != nil {
			return vmruntime.SnapshotInfo{}, err
		}
		if err := saveMemory(machine, filepath.Join(dir, snapState)); err != nil {
			// A disk-only snapshot is still complete; a create from it
			// boots cold.
			r.logger.Warn("saving the memory state failed; taking a disk-only snapshot", "sandbox_id", v.id, "error", err)
			os.Remove(filepath.Join(dir, snapState))
		} else if err := os.WriteFile(filepath.Join(dir, snapMachineID), v.machineID, 0o600); err != nil {
			return vmruntime.SnapshotInfo{}, err
		} else {
			info.HasMemoryState = true
		}
	}
	if err := copyFile(filepath.Join(v.dir, diskFile), filepath.Join(dir, snapDisk)); err != nil {
		return vmruntime.SnapshotInfo{}, err
	}
	for _, name := range []string{snapState, snapDisk, snapMachineID} {
		if st, err := os.Stat(filepath.Join(dir, name)); err == nil {
			info.SizeBytes += st.Size()
		}
	}
	return info, nil
}

// restoreMemory creates the VM of v with the snapshot's machine identifier
// and restores its saved state, then resumes it and waits for the guest
// helper.
func (r *Runtime) restoreMemory(ctx context.Context, v *vm, spec vmruntime.Spec) error {
	machineID, err := os.ReadFile(filepath.Join(spec.Snapshot.Dir, snapMachineID))
	if err != nil {
		return err
	}
	cfg, err := r.configure(v, spec, machineID)
	if err != nil {
		return err
	}
	if err := r.newMachine(v, cfg); err != nil {
		return err
	}
	if err := restoreMemory(v.machine, filepath.Join(spec.Snapshot.Dir, snapState)); err != nil {
		return err
	}
	if err := v.machine.Resume(); err != nil {
		return fmt.Errorf("vzrt: resume restored VM: %w", err)
	}
	return r.waitGuest(ctx, v)
}

// stopVM stops the VM of v, if it runs, and its network stack.
func (r *Runtime) stopVM(v *vm) {
	v.mu.Lock()
	machine, exited := v.machine, v.exited
	v.machine = nil
	stack, conn := v.net, v.netConn
	v.net, v.netConn = nil, nil
	v.mu.Unlock()
	if machine != nil && machine.State() != vz.VirtualMachineStateStopped && machine.State() != vz.VirtualMachineStateError {
		if machine.CanStop() {
			if err := machine.Stop(); err != nil {
				r.logger.Warn("stop VM failed", "sandbox_id", v.id, "error", err)
			}
		}
		select {
		case <-exited:
		case <-time.After(stopTimeout):
			r.logger.Error("VM did not stop", "sandbox_id", v.id)
		}
	}
	if stack != nil {
		stack.Close()
	}
	if conn != nil {
		conn.Close()
	}
}

// Delete stops the VM of the sandbox and removes its files.
func (r *Runtime) Delete(_ context.Context, sandboxID string) error {
	r.mu.Lock()
	v, ok := r.vms[sandboxID]
	delete(r.vms, sandboxID)
	r.mu.Unlock()
	if ok {
		r.stopVM(v)
	}
	if r.opts.VMsDir != "" {
		return os.RemoveAll(r.vmDir(sandboxID))
	}
	return nil
}

// Running lists the sandboxes with a running or paused VM. VMs run inside
// the agent process, so no VM outlives an earlier agent process.
func (r *Runtime) Running(context.Context) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make([]string, 0, len(r.vms))
	for id, v := range r.vms {
		v.mu.Lock()
		exited := v.exited
		v.mu.Unlock()
		if exited == nil {
			continue
		}
		select {
		case <-exited:
		default:
			ids = append(ids, id)
		}
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
