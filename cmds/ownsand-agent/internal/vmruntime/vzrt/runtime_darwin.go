// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build darwin && cgo

package vzrt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/Code-Hex/vz/v3"
	"golang.org/x/sys/unix"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/config"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/gpt"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/guestclient"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/guestsync"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/netpolicy"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/netstack"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime"
	"github.com/delinoio/mono/internal/fsutil"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// Timeouts of the runtime.
const (
	readyTimeout = 60 * time.Second
	// macOSReadyTimeout covers a macOS cold boot, which takes far longer
	// than a Linux boot.
	macOSReadyTimeout     = 5 * time.Minute
	dialTimeout           = 10 * time.Second
	snapshotResumeTimeout = 10 * time.Second
)

var errVMTerminalState = errors.New("VM reached a terminal state")

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

// hasEntitlement reports whether the signed agent enables the
// com.apple.security.virtualization entitlement.
func hasEntitlement(ctx context.Context) (bool, string) {
	exe, err := os.Executable()
	if err != nil {
		return false, "cannot find the agent executable"
	}
	return inspectVirtualizationEntitlement(ctx, exe)
}

func inspectVirtualizationEntitlement(ctx context.Context, exe string) (bool, string) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/codesign", "-d", "--entitlements", "-", "--xml", exe).Output()
	if err != nil {
		return false, "cannot inspect the signed entitlements of the agent executable"
	}
	enabled, err := virtualizationEntitlementEnabled(ctx, out)
	if err != nil {
		return false, "cannot read the signed entitlement plist of the agent executable"
	}
	if !enabled {
		return false, "the agent executable lacks an enabled com.apple.security.virtualization entitlement"
	}
	return true, "the agent executable has an enabled com.apple.security.virtualization entitlement"
}

func virtualizationEntitlementEnabled(ctx context.Context, plist []byte) (bool, error) {
	// Read the converted dictionary so dots in the key stay literal.
	cmd := exec.CommandContext(ctx, "/usr/bin/plutil", "-convert", "json", "-o", "-", "--", "-")
	cmd.Stdin = bytes.NewReader(plist)
	out, err := cmd.Output()
	if err != nil {
		return false, err
	}
	var entitlements map[string]any
	if err := json.Unmarshal(out, &entitlements); err != nil {
		return false, err
	}
	enabled, _ := entitlements["com.apple.security.virtualization"].(bool)
	return enabled, nil
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
	watcher       *machineWatcher
	restore       ownsandv1.SandboxRestorePath
	network       ownsandv1.NetworkMode
	net           *netstack.Network
	netConn       net.Conn
	vcpu          int32
	memMiB        int64
	mu            sync.Mutex
	paused        bool
	// console is the bounded writer of console.log. Create opens it once, so
	// the memory-restore attempt and the cold-boot fallback share one file and
	// one byte counter. Delete and the failed-create cleanup close it after the
	// VM stopped; stopVM must not, because the fallback boots again.
	console *vmruntime.CappedLog
	// consoleIn is the write end of the guest console input pipe of the
	// current VM configuration. It stays open and unused, so that the guest
	// never reads an EOF from its console; stopVM closes it.
	consoleIn *os.File
	// stopMu serializes cleanup and the installation of each machine and its
	// watcher, so cleanup always releases the pair it stopped.
	stopMu sync.Mutex
}

func (v *vm) SandboxID() string                         { return v.id }
func (v *vm) RestorePath() ownsandv1.SandboxRestorePath { return v.restore }
func (v *vm) Exited() <-chan struct{} {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.watcher == nil {
		return nil
	}
	return v.watcher.exited
}

// current returns the VM object, or an error after the VM was stopped.
func (v *vm) current() (*vz.VirtualMachine, <-chan struct{}, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.machine == nil {
		return nil, nil, fmt.Errorf("vzrt: sandbox %s has no running VM", v.id)
	}
	return v.machine, v.watcher.exited, nil
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

// copyFile copies src to dst with an APFS clone where possible. Clonefile
// cannot be interrupted. A streamed copy checks ctx between chunks.
func copyFile(ctx context.Context, src, dst string) error {
	return copyFileWithClone(ctx, src, dst, unix.Clonefile)
}

func copyFileWithClone(ctx context.Context, src, dst string, cloneFile func(string, string, int) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_ = os.Remove(dst)
	if err := ctx.Err(); err != nil {
		return err
	}
	cloneErr := cloneFile(src, dst, unix.CLONE_NOFOLLOW)
	if err := ctx.Err(); err != nil || cloneErr == nil {
		return err
	}
	return copyFileStream(ctx, src, dst)
}

func copyFileStream(ctx context.Context, src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	var copyErr error
	if ctx.Done() == nil {
		_, copyErr = io.Copy(out, in)
	} else {
		_, copyErr = fsutil.CopyContext(ctx, out, in)
	}
	if copyErr != nil {
		out.Close()
		return copyErr
	}
	if err := out.Close(); err != nil {
		return err
	}
	return ctx.Err()
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
	return r.createVM(ctx, spec, vmCreateOps{cloneFile: unix.Clonefile, growFile: growFile, fitGPT: gpt.Fit})
}

// vmCreateOps lets tests cancel create during a disk operation.
type vmCreateOps struct {
	cloneFile func(string, string, int) error
	growFile  func(string, int64) error
	fitGPT    func(string) error
}

func (ops vmCreateOps) copyFile(ctx context.Context, src, dst string) error {
	return copyFileWithClone(ctx, src, dst, ops.cloneFile)
}

// runCreateStep checks cancellation around a synchronous create operation.
func runCreateStep(ctx context.Context, run func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	stepErr := run()
	if err := ctx.Err(); err != nil {
		return err
	}
	return stepErr
}

func (r *Runtime) createVM(ctx context.Context, spec vmruntime.Spec, ops vmCreateOps) (vmruntime.VM, error) {
	switch spec.GuestOS {
	case ownsandv1.GuestOs_GUEST_OS_LINUX:
	case ownsandv1.GuestOs_GUEST_OS_MACOS:
		return r.createMacOS(ctx, spec, ops)
	default:
		return nil, fmt.Errorf("%w: macOS hosts run Linux and macOS guests", vmruntime.ErrUnsupported)
	}
	if !fileExists(r.opts.Kernel) {
		return nil, fmt.Errorf("%w: the Linux kernel is missing", vmruntime.ErrUnsupported)
	}
	diskBytes := spec.Resources.GetDiskGib() << 30
	if spec.Snapshot == nil {
		baseDisk, err := os.Stat(r.opts.Rootfs)
		if err != nil || !baseDisk.Mode().IsRegular() {
			return nil, fmt.Errorf("%w: the Linux base image is missing", vmruntime.ErrUnsupported)
		}
		if err := checkBaseDiskCapacity(baseDisk.Size(), diskBytes); err != nil {
			return nil, err
		}
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
	cleanup := func() { r.discard(v) }
	console, err := vmruntime.OpenCappedLog(filepath.Join(spec.Dir, consoleFile), vmruntime.ConsoleLogLimit,
		r.logger, spec.SandboxID, vmruntime.ConsoleLogKindConsole)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("vzrt: open the console log: %w", err)
	}
	v.console = console

	memoryRestore := vmruntime.MemoryRestoreEligible(spec, memoryStateSupported)
	src := r.opts.Rootfs
	if spec.Snapshot != nil {
		src = filepath.Join(spec.Snapshot.Dir, snapDisk)
	}
	disk := filepath.Join(spec.Dir, diskFile)
	if err := ops.copyFile(ctx, src, disk); err != nil {
		cleanup()
		return nil, fmt.Errorf("vzrt: prepare disk: %w", err)
	}
	r.mu.Lock()
	r.vms[spec.SandboxID] = v
	r.mu.Unlock()

	if memoryRestore {
		err := r.restoreMemory(ctx, v, spec)
		if err == nil && ctx.Err() == nil {
			v.restore = ownsandv1.SandboxRestorePath_SANDBOX_RESTORE_PATH_MEMORY_STATE
			clockCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			if err := guestclient.New(v.Dial).SetClock(clockCtx, time.Now()); err != nil {
				r.logger.Warn("set guest clock after memory restore failed", "sandbox_id", v.id, "error", err)
			}
			cancel()
			r.logger.Info("VM restored from memory state", "sandbox_id", v.id)
			return v, nil
		}
		if err := finishFailedRestore(ctx, func() error { return r.stopVM(v) }, cleanup); err != nil {
			return nil, err
		}
		r.logger.Warn("memory-state restore failed; booting the snapshot disk cold", "sandbox_id", v.id, "error", err)
		if err := ops.copyFile(ctx, src, disk); err != nil {
			cleanup()
			return nil, fmt.Errorf("vzrt: prepare disk: %w", err)
		}
	}
	if err := runCreateStep(ctx, func() error { return ops.growFile(disk, diskBytes) }); err != nil {
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
	memoryBytes, err := memoryMiBToBytes(spec.Resources.GetMemoryMib())
	if err != nil {
		return nil, err
	}
	cmdline := "console=hvc0 root=/dev/vda rw init=/sbin/init panic=1 random.trust_cpu=on"
	if spec.NetworkMode == ownsandv1.NetworkMode_NETWORK_MODE_OUTBOUND {
		cmdline += " " + guestLink.KernelIPArg()
	}
	boot, err := vz.NewLinuxBootLoader(r.opts.Kernel, vz.WithCommandLine(cmdline))
	if err != nil {
		return nil, err
	}
	cfg, err := vz.NewVirtualMachineConfiguration(boot, uint(spec.Resources.GetVcpuCount()), memoryBytes)
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

	serial, abortConsole, err := r.attachConsole(v)
	if err != nil {
		return nil, err
	}
	// Release the console pipes on every later error return.
	configured := false
	defer func() {
		if !configured {
			abortConsole()
		}
	}()
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
	configured = true
	return cfg, nil
}

// attachConsole connects the virtio console of v to the bounded console log
// through a pair of pipes and a file-handle serial attachment, so the agent
// counts and caps every guest console byte; a file attachment would let the
// guest grow console.log without a limit.
//
// The framework duplicates both descriptors it receives, so the agent closes
// its copies of the guest-side ends right after the attachment exists. The
// output pipe then reports EOF when the framework releases its copy. A
// goroutine drains the output pipe into v.console, which discards everything
// past the cap. The write end of the input pipe stays open on v: nothing is
// sent to the guest, and a closed input would read as EOF there.
//
// The returned abort releases the pipes when the caller does not use the
// returned port after all; it must not be called otherwise.
func (r *Runtime) attachConsole(v *vm) (*vz.VirtioConsoleDeviceSerialPortConfiguration, func(), error) {
	if v.console == nil {
		return nil, nil, errors.New("vzrt: the console log is not open")
	}
	outRead, outWrite, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	inRead, inWrite, err := os.Pipe()
	if err != nil {
		outRead.Close()
		outWrite.Close()
		return nil, nil, err
	}
	// The framework reads guest input from inRead and writes guest output to
	// outWrite.
	attachment, err := vz.NewFileHandleSerialPortAttachment(inRead, outWrite)
	inRead.Close()
	outWrite.Close()
	if err != nil {
		outRead.Close()
		inWrite.Close()
		return nil, nil, err
	}
	serial, err := vz.NewVirtioConsoleDeviceSerialPortConfiguration(attachment)
	if err != nil {
		outRead.Close()
		inWrite.Close()
		return nil, nil, err
	}
	v.mu.Lock()
	previous := v.consoleIn
	v.consoleIn = inWrite
	v.mu.Unlock()
	if previous != nil {
		previous.Close()
	}
	go drainConsole(outRead, v.console)
	abort := func() {
		// Closing the read end ends the drain; closing the write end of the
		// input pipe releases it.
		outRead.Close()
		v.mu.Lock()
		if v.consoleIn == inWrite {
			v.consoleIn = nil
		}
		v.mu.Unlock()
		inWrite.Close()
	}
	return serial, abort, nil
}

// closeConsole closes the bounded console log of v after its VM stopped. It
// is safe for a VM without a console log.
func (r *Runtime) closeConsole(v *vm) {
	if v.console == nil {
		return
	}
	if err := v.console.Close(); err != nil {
		r.logger.Warn("close console log failed", "sandbox_id", v.id, "error", err)
	}
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
// a file-handle attachment. It takes ownership of vmFile and closes it when
// configuration ends. The attachment owns its duplicate descriptor.
func networkDevice(vmFile *os.File, mac net.HardwareAddr) (*vz.VirtioNetworkDeviceConfiguration, error) {
	defer vmFile.Close()
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
	return dev, conn, nil
}

// newMachine creates the VM object of v and watches its state.
func (r *Runtime) newMachine(v *vm, cfg *vz.VirtualMachineConfiguration) error {
	machine, err := vz.NewVirtualMachine(cfg)
	if err != nil {
		return err
	}
	v.stopMu.Lock()
	defer v.stopMu.Unlock()
	v.mu.Lock()
	v.machine = machine
	v.watcher = watchMachine(machine.StateChangedNotify(), terminalState)
	v.mu.Unlock()
	return nil
}

func (r *Runtime) coldBoot(ctx context.Context, v *vm, spec vmruntime.Spec) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cfg, err := r.configure(v, spec, nil)
	if err != nil {
		return err
	}
	if err := r.newMachine(v, cfg); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
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
	return waitGuestResult(done, v.Exited())
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

// waitState waits until the VM reaches want, exits, or the context ends.
func waitState(ctx context.Context, state func() vz.VirtualMachineState, want vz.VirtualMachineState) error {
	t := time.NewTicker(10 * time.Millisecond)
	defer t.Stop()
	for {
		current := state()
		if current == want {
			return nil
		}
		if terminalState(current) {
			return fmt.Errorf("vzrt: VM state %v, want %v: %w", current, want, errVMTerminalState)
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("vzrt: VM state %v, want %v: %w", current, want, err)
		}
		select {
		case <-ctx.Done():
		case <-t.C:
		}
	}
}

// Pause flushes the guest page cache and then stops the vCPUs and keeps the
// memory. The flush makes a later disk copy of the paused VM hold every
// write completed before the pause.
func (r *Runtime) Pause(ctx context.Context, m vmruntime.VM) error {
	v, err := r.lookup(m.SandboxID())
	if err != nil {
		return err
	}
	r.flushForPause(ctx, v)
	return r.pauseVM(ctx, v)
}

func (r *Runtime) flushForPause(ctx context.Context, v *vm) {
	if err := guestsync.Sync(ctx, v.id, v.Dial); err != nil {
		r.logger.Warn("guest sync before pause failed", "sandbox_id", v.id, "error", err)
	}
}

func (r *Runtime) pauseVM(ctx context.Context, v *vm) error {
	machine, _, err := v.current()
	if err != nil {
		return err
	}
	if err := machine.Pause(); err != nil {
		return fmt.Errorf("vzrt: pause: %w", err)
	}
	if err := waitState(ctx, machine.State, vz.VirtualMachineStatePaused); err != nil {
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
	if err := waitState(ctx, machine.State, vz.VirtualMachineStateRunning); err != nil {
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
	if err := ctx.Err(); err != nil {
		return vmruntime.SnapshotInfo{}, err
	}
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
	// A paused VM was flushed by its Pause; flush a running one before capture.
	info, err := vmruntime.SnapshotWithPause(r.logger, v.id, wasPaused,
		func() error {
			r.flushForPause(ctx, v)
			if err := ctx.Err(); err != nil {
				return err
			}
			// Wait for a pause that started so SnapshotWithPause can resume
			// the VM even if the channel closes during the native transition.
			pauseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), StopTimeout)
			defer cancel()
			return r.pauseVM(pauseCtx, v)
		},
		func() error {
			// Recover the source even when the snapshot context has ended.
			// The deadline bounds the state poll after the native Resume call.
			resumeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), snapshotResumeTimeout)
			defer cancel()
			return r.Resume(resumeCtx, m)
		},
		func() (vmruntime.SnapshotInfo, error) { return r.capture(ctx, v, dir) })
	if errors.Is(err, errVMTerminalState) {
		// The VM exit watcher reconciles a terminal source after Snapshot
		// returns and the command lock is released.
		info.SourceState = ownsandv1.SandboxState_SANDBOX_STATE_UNSPECIFIED
	}
	return info, err
}

// capture writes the snapshot content of the paused VM v into dir.
func (r *Runtime) capture(ctx context.Context, v *vm, dir string) (vmruntime.SnapshotInfo, error) {
	if err := ctx.Err(); err != nil {
		return vmruntime.SnapshotInfo{}, err
	}
	if v.guestOS == ownsandv1.GuestOs_GUEST_OS_MACOS {
		return r.snapshotMacOS(ctx, v, dir)
	}
	info := vmruntime.SnapshotInfo{}
	if memoryStateSupported && v.network == ownsandv1.NetworkMode_NETWORK_MODE_NONE {
		saved, err := r.saveSnapshotMemory(ctx, v, filepath.Join(dir, snapState))
		if err != nil {
			return vmruntime.SnapshotInfo{}, err
		}
		if saved {
			if err := os.WriteFile(filepath.Join(dir, snapMachineID), v.machineID, 0o600); err != nil {
				return vmruntime.SnapshotInfo{}, err
			}
			info.HasMemoryState = true
		}
	}
	if err := copyFile(ctx, filepath.Join(v.dir, diskFile), filepath.Join(dir, snapDisk)); err != nil {
		return vmruntime.SnapshotInfo{}, err
	}
	for _, name := range []string{snapState, snapDisk, snapMachineID} {
		if st, err := os.Stat(filepath.Join(dir, name)); err == nil {
			info.SizeBytes += st.Size()
		}
	}
	return info, ctx.Err()
}

// saveSnapshotMemory waits for the native save, which cannot be interrupted.
// A native save error permits a disk-only snapshot if the channel is still open.
func (r *Runtime) saveSnapshotMemory(ctx context.Context, v *vm, path string) (bool, error) {
	machine, _, err := v.current()
	if err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	saveErr := saveMemory(machine, path)
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if saveErr != nil {
		r.logger.Warn("saving the memory state failed; taking a disk-only snapshot", "sandbox_id", v.id, "error", saveErr)
		os.Remove(path)
		return false, nil
	}
	return true, nil
}

// restoreMemory creates the VM of v with the snapshot's machine identifier
// and restores its saved state, then resumes it and waits for the guest
// helper.
func (r *Runtime) restoreMemory(ctx context.Context, v *vm, spec vmruntime.Spec) error {
	if err := ctx.Err(); err != nil {
		return err
	}
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
	statePath := filepath.Join(spec.Snapshot.Dir, snapState)
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := restoreMemory(v.machine, statePath); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := v.machine.Resume(); err != nil {
		return fmt.Errorf("vzrt: resume restored VM: %w", err)
	}
	return r.waitGuest(ctx, v)
}

// vzMachine adapts a Virtualization.framework VM to stoppableMachine.
type vzMachine struct{ m *vz.VirtualMachine }

func (z vzMachine) Terminal() bool {
	return terminalState(z.m.State())
}

// A terminal notification can let cleanup discard the last tracked handle
// while either native call is in progress. Keep the VM and its queue alive
// until the call returns.
func (z vzMachine) CanStop() bool {
	ok := z.m.CanStop()
	runtime.KeepAlive(z.m)
	return ok
}

func (z vzMachine) Stop() error {
	err := z.m.Stop()
	runtime.KeepAlive(z.m)
	return err
}

// StopVMForCleanup confirms that machine reached a terminal state. It uses
// the runtime stop timeout for both the request and the wait. It returns
// primary, if any, on success. If termination is unconfirmed, it joins the
// stop failure and ErrVMMayRun with primary.
func StopVMForCleanup(primary error, machine *vz.VirtualMachine, exited <-chan struct{}) error {
	return stopForCleanup(primary, vzMachine{machine}, exited, StopTimeout)
}

// stopVM stops the VM of v, if it runs, and then its network stack. It
// returns an error, and keeps the VM handle and the network stack, when the
// framework rejects the stop or the VM does not reach a terminal state in
// time: the VM may still run then, so the caller must keep the sandbox state
// and may call stopVM again. A VM that never started, or that an earlier call
// already stopped, succeeds at once.
func (r *Runtime) stopVM(v *vm) error {
	v.stopMu.Lock()
	defer v.stopMu.Unlock()
	v.mu.Lock()
	machine, watcher := v.machine, v.watcher
	v.mu.Unlock()
	if machine != nil {
		if err := watcher.stop(vzMachine{machine}, StopTimeout); err != nil {
			r.logger.Error("VM did not stop", "sandbox_id", v.id, "error", err)
			return err
		}
	}
	// The VM is gone: nothing uses the network stack any more.
	v.mu.Lock()
	v.machine = nil
	stack, conn := v.net, v.netConn
	v.net, v.netConn = nil, nil
	consoleIn := v.consoleIn
	v.consoleIn = nil
	v.mu.Unlock()
	if stack != nil {
		stack.Close()
	}
	if conn != nil {
		conn.Close()
	}
	if consoleIn != nil {
		consoleIn.Close()
	}
	return nil
}

// removeVMDir removes the VM directory of the sandbox.
func (r *Runtime) removeVMDir(sandboxID string) error {
	if r.opts.VMsDir == "" {
		return nil
	}
	return os.RemoveAll(r.vmDir(sandboxID))
}

// discard releases the VM of a failed create: it stops the VM and then
// removes the sandbox directory. When the VM does not stop, it keeps the
// tracked handle and the directory, because the files are still in use; the
// runtime lists the VM in Running, and Delete and Close retry the stop.
func (r *Runtime) discard(v *vm) {
	err := releaseAfterStop(&r.mu, r.vms, v.id, v,
		func() error { return r.stopVM(v) },
		func() error {
			r.closeConsole(v)
			return os.RemoveAll(v.dir)
		})
	if err != nil {
		r.logger.Error("discard of a failed create incomplete; the VM stays tracked for a retry", "sandbox_id", v.id, "error", err)
	}
}

// Delete stops the VM of the sandbox and removes its files. When the VM does
// not stop, it returns an error and keeps the VM handle and the files, so
// the sandbox manager can retry the delete; the files are removed only after
// the VM reached a terminal state.
func (r *Runtime) Delete(_ context.Context, sandboxID string) error {
	r.mu.Lock()
	v, ok := r.vms[sandboxID]
	r.mu.Unlock()
	remove := func() error { return r.removeVMDir(sandboxID) }
	if !ok {
		return remove()
	}
	return releaseAfterStop(&r.mu, r.vms, sandboxID, v,
		func() error { return r.stopVM(v) },
		func() error {
			r.closeConsole(v)
			return remove()
		})
}

// Running lists the sandboxes with a running or paused VM. VMs run inside
// the agent process, so no VM outlives an earlier agent process.
func (r *Runtime) Running(context.Context) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make([]string, 0, len(r.vms))
	for id, v := range r.vms {
		exited := v.Exited()
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
