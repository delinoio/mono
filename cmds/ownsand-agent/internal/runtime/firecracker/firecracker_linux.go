//go:build linux

package firecracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/dnsfwd"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/runtime"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/update"
	"github.com/delinoio/mono/cmds/ownsand-guest/guest"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

const (
	apiSocket      = "firecracker.sock"
	vsockSocket    = "vsock.sock"
	diskFile       = "rootfs.ext4"
	consoleLog     = "console.log"
	snapStateFile  = "vmstate"
	snapMemoryFile = "memory"
	snapMetaFile   = "meta.json"
	// baseBootArgs: serial console for debugging, reboot ends the VM process,
	// and no PCI (Firecracker uses virtio-mmio).
	baseBootArgs = "console=ttyS0 reboot=k panic=1 pci=off"
)

// Config configures the Firecracker runtime.
type Config struct {
	StateDir string
	Logger   *slog.Logger
	// Binary is the firecracker executable. Default: $OWNSAND_AGENT_FIRECRACKER
	// or /usr/local/bin/firecracker.
	Binary string
	// GuestRange holds the guest links. Default: DefaultGuestRange.
	GuestRange netip.Prefix
	// AllowUnsignedImage accepts a base image without a valid signed
	// manifest. Development and acceptance only; it is logged at start.
	AllowUnsignedImage bool
	// ResolvConf is the host resolver configuration. Default /etc/resolv.conf.
	ResolvConf string
	// SkipNetwork disables the network setup (acceptance runs without
	// CAP_NET_ADMIN); OUTBOUND creates then fail.
	SkipNetwork bool
}

type vm struct {
	id     string
	dir    string
	spec   runtime.VMSpec
	cmd    *exec.Cmd
	api    *API
	link   *Link
	dns    *dnsfwd.Forwarder
	exited chan struct{}
	paused bool
}

// Runtime runs Firecracker microVMs.
type Runtime struct {
	cfg      Config
	log      *slog.Logger
	launcher *launcher
	net      *network
	alloc    *Allocator
	ctx      context.Context
	cancel   context.CancelFunc

	mu  sync.Mutex
	vms map[string]*vm

	imgMu     sync.Mutex
	imgStatus ImageStatus
	imgErr    error
	imgKey    string
}

// New starts the runtime: it checks the guest range, applies the network
// policy and starts the policy watcher.
func New(cfg Config) (*Runtime, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Binary == "" {
		cfg.Binary = os.Getenv("OWNSAND_AGENT_FIRECRACKER")
	}
	if cfg.Binary == "" {
		cfg.Binary = "/usr/local/bin/firecracker"
	}
	if !cfg.GuestRange.IsValid() {
		cfg.GuestRange = DefaultGuestRange
	}
	if cfg.ResolvConf == "" {
		cfg.ResolvConf = "/etc/resolv.conf"
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &Runtime{
		cfg:      cfg,
		log:      cfg.Logger.With("runtime", "firecracker"),
		launcher: newLauncher(),
		net:      &network{log: cfg.Logger, rng: cfg.GuestRange},
		alloc:    NewAllocator(cfg.GuestRange),
		ctx:      ctx,
		cancel:   cancel,
		vms:      map[string]*vm{},
	}
	for _, d := range []string{r.vmsDir(), r.snapshotsDir(), r.ImageDir()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			cancel()
			return nil, err
		}
	}
	if cfg.AllowUnsignedImage {
		r.log.Warn("development mode: an unsigned Linux base image is accepted")
	}
	if cg, err := os.ReadFile("/proc/self/cgroup"); err == nil {
		// VM processes inherit this cgroup, which the service manager kills
		// when the agent exits.
		r.log.Info("agent cgroup", "cgroup", strings.TrimSpace(string(cg)))
	}
	if cfg.SkipNetwork {
		r.net.setErr(errors.New("network setup is disabled"))
	} else if err := r.net.setup(ctx); err != nil {
		r.log.Error("network policy unavailable; OUTBOUND sandboxes cannot be created", "error", err)
	}
	if !cfg.SkipNetwork {
		go r.net.watch(ctx)
	}
	return r, nil
}

func (r *Runtime) vmsDir() string       { return filepath.Join(r.cfg.StateDir, "vms") }
func (r *Runtime) snapshotsDir() string { return filepath.Join(r.cfg.StateDir, "snapshots") }

// ImageDir is the directory of the LINUX base image.
func (r *Runtime) ImageDir() string { return filepath.Join(r.cfg.StateDir, "images", "linux") }

// Name returns "firecracker".
func (r *Runtime) Name() string { return "firecracker" }

func (r *Runtime) firecrackerVersion(ctx context.Context) (string, error) {
	out, err := run(ctx, r.cfg.Binary, nil, "--version")
	if err != nil {
		return "", err
	}
	first, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	f := strings.Fields(first)
	if len(f) < 2 {
		return "", errors.New("unexpected firecracker --version output")
	}
	return f[len(f)-1], nil
}

// imageStatus verifies the base image, cached by file sizes and times.
func (r *Runtime) imageStatus() (ImageStatus, error) {
	key := ""
	for _, f := range []string{KernelFile, RootfsFile, ImageManifest} {
		if fi, err := os.Stat(filepath.Join(r.ImageDir(), f)); err == nil {
			key += fmt.Sprintf("%s:%d:%d;", f, fi.Size(), fi.ModTime().UnixNano())
		}
	}
	r.imgMu.Lock()
	defer r.imgMu.Unlock()
	if key == r.imgKey && r.imgStatus != 0 {
		return r.imgStatus, r.imgErr
	}
	st, err := VerifyImage(r.ImageDir(), update.BuiltinReleaseKeys, r.cfg.AllowUnsignedImage)
	r.imgKey, r.imgStatus, r.imgErr = key, st, err
	r.log.Info("linux base image checked", "status", int(st), "error", errString(err))
	return st, err
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// Capabilities probes KVM, the pinned Firecracker binary, the base image
// and the network policy.
func (r *Runtime) Capabilities(ctx context.Context) runtime.Capabilities {
	checks := map[string]string{}
	ok := true
	if f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0); err != nil {
		checks["kvm"] = "unavailable: " + err.Error()
		ok = false
	} else {
		_ = f.Close()
		checks["kvm"] = "ok"
	}
	if v, err := r.firecrackerVersion(ctx); err != nil {
		checks["firecracker"] = "unavailable: " + err.Error()
		ok = false
	} else if v != Version {
		checks["firecracker"] = fmt.Sprintf("version %s found, %s required", v, Version)
		ok = false
	} else {
		checks["firecracker"] = "ok"
	}
	if _, err := os.Stat("/dev/net/tun"); err != nil {
		checks["tun"] = "unavailable"
	} else {
		checks["tun"] = "ok"
	}
	if err := r.net.status(); err != nil {
		checks["network_policy"] = "unavailable: " + err.Error()
	} else {
		checks["network_policy"] = "ok"
	}
	caps := runtime.Capabilities{HypervisorChecks: checks}
	switch st, err := r.imageStatus(); st {
	case ImageVerified:
		checks["base_image"] = "ok"
	case ImageUnsigned:
		checks["base_image"] = "unsigned (development mode)"
	default:
		checks["base_image"] = "unavailable: " + errString(err)
	}
	if ok {
		caps.GuestOSes = []ownsandv1.GuestOs{ownsandv1.GuestOs_GUEST_OS_LINUX}
		if st, _ := r.imageStatus(); st == ImageVerified || st == ImageUnsigned {
			caps.BuiltBaseImages = []ownsandv1.BaseImage{ownsandv1.BaseImage_BASE_IMAGE_LINUX}
		}
	}
	return caps
}

func (r *Runtime) get(id string) (*vm, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v := r.vms[id]
	if v == nil {
		return nil, runtime.ErrVMNotFound
	}
	return v, nil
}

// snapshotMeta is meta.json of a snapshot.
type snapshotMeta struct {
	FirecrackerVersion string                `json:"firecracker_version"`
	VCPUs              int64                 `json:"vcpu_count"`
	MemoryMiB          int64                 `json:"memory_mib"`
	Network            ownsandv1.NetworkMode `json:"network_mode"`
	LinkIndex          int                   `json:"link_index"`
}

// Create prepares the VM directory and its disk.
func (r *Runtime) Create(ctx context.Context, spec runtime.VMSpec) error {
	if spec.GuestOS != ownsandv1.GuestOs_GUEST_OS_LINUX {
		return runtime.Unsupported("a non-Linux guest")
	}
	if spec.Network == ownsandv1.NetworkMode_NETWORK_MODE_OUTBOUND {
		if err := r.net.status(); err != nil {
			return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("the network policy is unavailable: %w", err))
		}
	}
	dir := filepath.Join(r.vmsDir(), spec.SandboxID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	src := filepath.Join(r.ImageDir(), RootfsFile)
	if spec.Source.SnapshotID != "" {
		src = filepath.Join(r.snapshotsDir(), spec.Source.SnapshotID, diskFile)
	}
	disk := filepath.Join(dir, diskFile)
	if err := cloneFile(src, disk); err != nil {
		_ = os.RemoveAll(dir)
		return fmt.Errorf("prepare disk: %w", err)
	}
	// Grow the disk file to the requested size. The base image grows its
	// file system to the disk at boot (an image requirement).
	fi, err := os.Stat(disk)
	if err != nil {
		_ = os.RemoveAll(dir)
		return err
	}
	if want := spec.DiskGiB << 30; want > fi.Size() {
		if err := os.Truncate(disk, want); err != nil {
			_ = os.RemoveAll(dir)
			return err
		}
	}
	r.mu.Lock()
	r.vms[spec.SandboxID] = &vm{id: spec.SandboxID, dir: dir, spec: spec, exited: make(chan struct{})}
	r.mu.Unlock()
	r.log.Info("vm prepared", "sandbox_id", spec.SandboxID)
	return nil
}

// launch starts the firecracker process of v.
func (r *Runtime) launch(v *vm) error {
	_ = os.Remove(filepath.Join(v.dir, apiSocket))
	_ = os.Remove(filepath.Join(v.dir, vsockSocket))
	logf, err := os.OpenFile(filepath.Join(v.dir, consoleLog), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	// The VM directory is the working directory, so the disk and vsock paths
	// in the VM configuration are relative and a memory snapshot restores in
	// another VM directory.
	cmd := exec.Command(r.cfg.Binary, "--api-sock", apiSocket, "--id", v.id)
	cmd.Dir = v.dir
	cmd.Stdout = logf // the guest serial console: sandbox content, kept in the VM directory only
	cmd.Stderr = logf
	if err := r.launcher.start(cmd); err != nil {
		_ = logf.Close()
		return fmt.Errorf("start firecracker: %w", err)
	}
	v.cmd = cmd
	v.exited = make(chan struct{})
	exited := v.exited
	go func() {
		_ = cmd.Wait()
		_ = logf.Close()
		close(exited)
	}()
	v.api = NewAPI(filepath.Join(v.dir, apiSocket))
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(v.dir, apiSocket)); err == nil {
			return nil
		}
		select {
		case <-exited:
			return errors.New("firecracker exited before its API socket appeared")
		case <-time.After(20 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			r.kill(v)
			return errors.New("firecracker API socket did not appear")
		}
	}
}

func (r *Runtime) kill(v *vm) {
	if v.cmd != nil && v.cmd.Process != nil {
		_ = syscall.Kill(-v.cmd.Process.Pid, syscall.SIGKILL)
		select {
		case <-v.exited:
		case <-time.After(10 * time.Second):
			r.log.Error("firecracker process did not exit after SIGKILL", "sandbox_id", v.id)
		}
	}
}

// setupLink allocates the guest link, creates the tap device and starts the
// DNS forwarder.
func (r *Runtime) setupLink(ctx context.Context, v *vm, preferred int) error {
	l, err := r.alloc.Allocate(preferred)
	if err != nil {
		return err
	}
	if err := r.net.addTap(ctx, l); err != nil {
		r.alloc.Release(l.Index)
		return fmt.Errorf("create tap device: %w", err)
	}
	fwd, err := dnsfwd.Start(netip.AddrPortFrom(l.Gateway, 53), dnsfwd.Upstreams(r.cfg.ResolvConf), r.log)
	if err != nil {
		r.net.deleteTap(ctx, l.Tap)
		r.alloc.Release(l.Index)
		return fmt.Errorf("start DNS forwarder: %w", err)
	}
	v.link, v.dns = &l, fwd
	return nil
}

func (r *Runtime) teardownLink(v *vm) {
	if v.dns != nil {
		_ = v.dns.Close()
		v.dns = nil
	}
	if v.link != nil {
		r.net.deleteTap(context.Background(), v.link.Tap)
		r.alloc.Release(v.link.Index)
		v.link = nil
	}
}

// Start boots the VM. For a snapshot source with TryMemoryRestore, it first
// tries to load the memory state, and boots the snapshot disk cold when the
// load fails.
func (r *Runtime) Start(ctx context.Context, id string) (ownsandv1.RestorePath, error) {
	v, err := r.get(id)
	if err != nil {
		return 0, err
	}
	if v.spec.Source.SnapshotID != "" && v.spec.TryMemoryRestore {
		if err := r.restore(ctx, v); err == nil {
			r.log.Info("vm restored from memory state", "sandbox_id", id)
			return ownsandv1.RestorePath_RESTORE_PATH_MEMORY_STATE, nil
		} else {
			r.log.Warn("memory state restore failed; booting the snapshot disk cold", "sandbox_id", id, "error", err)
			r.kill(v)
			r.teardownLink(v)
		}
	}
	if err := r.coldBoot(ctx, v); err != nil {
		r.kill(v)
		r.teardownLink(v)
		return 0, err
	}
	if v.spec.Source.SnapshotID != "" {
		return ownsandv1.RestorePath_RESTORE_PATH_COLD_BOOT, nil
	}
	return ownsandv1.RestorePath_RESTORE_PATH_BASE_IMAGE, nil
}

func (r *Runtime) coldBoot(ctx context.Context, v *vm) error {
	bootArgs := baseBootArgs
	if v.spec.Network == ownsandv1.NetworkMode_NETWORK_MODE_OUTBOUND {
		if err := r.setupLink(ctx, v, -1); err != nil {
			return err
		}
		l := v.link
		// Kernel IP autoconfiguration: guest address, gateway, netmask,
		// device, no autoconf, and the gateway as DNS server.
		bootArgs += fmt.Sprintf(" ip=%s::%s:255.255.255.252::eth0:off:%s", l.Guest, l.Gateway, l.Gateway)
	}
	if err := r.launch(v); err != nil {
		return err
	}
	steps := []struct {
		method, path string
		body         any
	}{
		{"PUT", "/boot-source", BootSource{KernelImagePath: filepath.Join(r.ImageDir(), KernelFile), BootArgs: bootArgs}},
		{"PUT", "/drives/rootfs", Drive{DriveID: "rootfs", PathOnHost: diskFile, IsRootDevice: true}},
		{"PUT", "/machine-config", MachineConfig{VCPUCount: v.spec.VCPUs, MemSizeMiB: v.spec.MemoryMiB}},
		{"PUT", "/vsock", Vsock{GuestCID: GuestVsockCID, UDSPath: vsockSocket}},
	}
	if v.link != nil {
		steps = append(steps, struct {
			method, path string
			body         any
		}{"PUT", "/network-interfaces/eth0", NetworkInterface{IfaceID: "eth0", HostDevName: v.link.Tap, GuestMAC: v.link.MAC}})
	}
	steps = append(steps, struct {
		method, path string
		body         any
	}{"PUT", "/actions", Action{ActionType: "InstanceStart"}})
	for _, s := range steps {
		if err := v.api.Do(ctx, s.method, s.path, s.body); err != nil {
			return err
		}
	}
	r.log.Info("vm booted", "sandbox_id", v.id, "vcpu_count", v.spec.VCPUs, "memory_mib", v.spec.MemoryMiB, "network_mode", v.spec.Network.String())
	return nil
}

func (r *Runtime) restore(ctx context.Context, v *vm) error {
	snapDir := filepath.Join(r.snapshotsDir(), v.spec.Source.SnapshotID)
	var meta snapshotMeta
	data, err := os.ReadFile(filepath.Join(snapDir, snapMetaFile))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return err
	}
	if meta.FirecrackerVersion != Version || meta.VCPUs != v.spec.VCPUs || meta.MemoryMiB != v.spec.MemoryMiB || meta.Network != v.spec.Network {
		return errors.New("the snapshot does not match the request or the Firecracker version")
	}
	load := SnapshotLoad{
		SnapshotPath:  filepath.Join(snapDir, snapStateFile),
		MemBackend:    MemBackend{BackendType: "File", BackendPath: filepath.Join(snapDir, snapMemoryFile)},
		ResumeVM:      true,
		VsockOverride: &VsockOverride{UDSPath: vsockSocket},
	}
	if v.spec.Network == ownsandv1.NetworkMode_NETWORK_MODE_OUTBOUND {
		// The guest keeps the address of the snapshot in its memory, so a
		// memory restore needs the same guest link, which is free only when
		// the source sandbox no longer uses it.
		if err := r.setupLink(ctx, v, meta.LinkIndex); err != nil {
			return fmt.Errorf("the snapshot's guest link is in use: %w", err)
		}
		load.NetworkOverrides = []NetworkOverride{{IfaceID: "eth0", HostDevName: v.link.Tap}}
	}
	if err := r.launch(v); err != nil {
		return err
	}
	return v.api.Do(ctx, "PUT", "/snapshot/load", load)
}

// Pause stops the vCPUs.
func (r *Runtime) Pause(ctx context.Context, id string) error {
	v, err := r.get(id)
	if err != nil {
		return err
	}
	if err := v.api.Do(ctx, "PATCH", "/vm", VMState{State: "Paused"}); err != nil {
		return err
	}
	r.mu.Lock()
	v.paused = true
	r.mu.Unlock()
	return nil
}

// Resume continues the vCPUs.
func (r *Runtime) Resume(ctx context.Context, id string) error {
	v, err := r.get(id)
	if err != nil {
		return err
	}
	if err := v.api.Do(ctx, "PATCH", "/vm", VMState{State: "Resumed"}); err != nil {
		return err
	}
	r.mu.Lock()
	v.paused = false
	r.mu.Unlock()
	return nil
}

// Stop ends the VM process and keeps its disk.
func (r *Runtime) Stop(_ context.Context, id string) error {
	v, err := r.get(id)
	if err != nil {
		return nil
	}
	r.kill(v)
	r.teardownLink(v)
	return nil
}

// Delete ends the VM process and deletes its directory.
func (r *Runtime) Delete(_ context.Context, id string) error {
	r.mu.Lock()
	v := r.vms[id]
	delete(r.vms, id)
	r.mu.Unlock()
	if v != nil {
		r.kill(v)
		r.teardownLink(v)
	} else {
		r.killLeftover(id)
	}
	if err := os.RemoveAll(filepath.Join(r.vmsDir(), id)); err != nil {
		return err
	}
	r.log.Info("vm deleted", "sandbox_id", id)
	return nil
}

// Snapshot pauses the VM if it runs, writes a full snapshot (memory state
// and device state) and a copy of the disk, and resumes the VM.
func (r *Runtime) Snapshot(ctx context.Context, id, snapshotID string) (runtime.SnapshotInfo, error) {
	v, err := r.get(id)
	if err != nil {
		return runtime.SnapshotInfo{}, err
	}
	dir := filepath.Join(r.snapshotsDir(), snapshotID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return runtime.SnapshotInfo{}, err
	}
	fail := func(err error) (runtime.SnapshotInfo, error) {
		_ = os.RemoveAll(dir)
		return runtime.SnapshotInfo{}, err
	}
	r.mu.Lock()
	wasPaused := v.paused
	r.mu.Unlock()
	if !wasPaused {
		if err := v.api.Do(ctx, "PATCH", "/vm", VMState{State: "Paused"}); err != nil {
			return fail(err)
		}
		defer func() {
			if err := v.api.Do(context.WithoutCancel(ctx), "PATCH", "/vm", VMState{State: "Resumed"}); err != nil {
				r.log.Error("resume after snapshot failed", "sandbox_id", id, "error", err)
			}
		}()
	}
	if err := v.api.Do(ctx, "PUT", "/snapshot/create", SnapshotCreate{
		SnapshotType: "Full",
		SnapshotPath: filepath.Join(dir, snapStateFile),
		MemFilePath:  filepath.Join(dir, snapMemoryFile),
	}); err != nil {
		return fail(err)
	}
	if err := cloneFile(filepath.Join(v.dir, diskFile), filepath.Join(dir, diskFile)); err != nil {
		return fail(err)
	}
	meta := snapshotMeta{FirecrackerVersion: Version, VCPUs: v.spec.VCPUs, MemoryMiB: v.spec.MemoryMiB, Network: v.spec.Network, LinkIndex: -1}
	if v.link != nil {
		meta.LinkIndex = v.link.Index
	}
	data, _ := json.Marshal(meta)
	if err := os.WriteFile(filepath.Join(dir, snapMetaFile), data, 0o600); err != nil {
		return fail(err)
	}
	var size int64
	for _, f := range []string{snapStateFile, snapMemoryFile, diskFile} {
		if fi, err := os.Stat(filepath.Join(dir, f)); err == nil {
			size += fi.Size()
		}
	}
	r.log.Info("snapshot written", "sandbox_id", id, "snapshot_id", snapshotID, "size_bytes", size)
	return runtime.SnapshotInfo{SnapshotID: snapshotID, SizeBytes: size, HasMemoryState: true}, nil
}

// DeleteSnapshot deletes a snapshot directory.
func (r *Runtime) DeleteSnapshot(_ context.Context, snapshotID string) error {
	if snapshotID == "" || strings.ContainsAny(snapshotID, `/\`) {
		return errors.New("invalid snapshot ID")
	}
	return os.RemoveAll(filepath.Join(r.snapshotsDir(), snapshotID))
}

// List returns the firecracker processes that run in the agent's VM
// directory, tagged by their --id argument. It finds processes that an
// earlier agent process started, too.
func (r *Runtime) List(context.Context) ([]runtime.VMInfo, error) {
	procs, err := r.scanProcesses()
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]runtime.VMInfo, 0, len(procs))
	for id := range procs {
		info := runtime.VMInfo{SandboxID: id}
		if v := r.vms[id]; v != nil {
			info.Paused = v.paused
		}
		out = append(out, info)
	}
	return out, nil
}

// scanProcesses maps sandbox IDs to the PIDs of their firecracker processes.
func (r *Runtime) scanProcesses() (map[string]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	vmsDir := r.vmsDir()
	out := map[string]int{}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		cwd, err := os.Readlink(filepath.Join("/proc", e.Name(), "cwd"))
		if err != nil || filepath.Dir(cwd) != vmsDir {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue
		}
		args := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "--id" && filepath.Base(cwd) == args[i+1] {
				out[args[i+1]] = pid
			}
		}
	}
	return out, nil
}

func (r *Runtime) killLeftover(id string) {
	procs, err := r.scanProcesses()
	if err != nil {
		return
	}
	if pid, ok := procs[id]; ok {
		r.log.Warn("killing a leftover firecracker process", "sandbox_id", id, "pid", pid)
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

// DialGuest connects to the guest helper over the VM's vsock socket.
func (r *Runtime) DialGuest(ctx context.Context, id string) (io.ReadWriteCloser, error) {
	v, err := r.get(id)
	if err != nil {
		return nil, err
	}
	c, err := dialUnix(ctx, filepath.Join(v.dir, vsockSocket))
	if err != nil {
		return nil, err
	}
	if err := Handshake(c, guest.DefaultVsockPort, 5*time.Second); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

// DeleteAll deletes every VM and snapshot. The Linux base image comes from
// the release process and is not a locally built image, so it stays.
func (r *Runtime) DeleteAll(ctx context.Context) error {
	r.mu.Lock()
	ids := make([]string, 0, len(r.vms))
	for id := range r.vms {
		ids = append(ids, id)
	}
	r.mu.Unlock()
	if procs, err := r.scanProcesses(); err == nil {
		for id := range procs {
			ids = append(ids, id)
		}
	}
	var errs []error
	for _, id := range ids {
		if err := r.Delete(ctx, id); err != nil {
			errs = append(errs, err)
		}
	}
	errs = append(errs, os.RemoveAll(r.vmsDir()), os.RemoveAll(r.snapshotsDir()))
	_ = os.MkdirAll(r.vmsDir(), 0o700)
	_ = os.MkdirAll(r.snapshotsDir(), 0o700)
	return errors.Join(errs...)
}

// Close stops every VM and removes the agent's network table: VMs never
// outlive the agent.
func (r *Runtime) Close() error {
	r.cancel()
	r.mu.Lock()
	vms := make([]*vm, 0, len(r.vms))
	for _, v := range r.vms {
		vms = append(vms, v)
	}
	r.mu.Unlock()
	for _, v := range vms {
		r.kill(v)
		r.teardownLink(v)
	}
	if !r.cfg.SkipNetwork {
		r.net.teardown(context.Background())
	}
	return nil
}

var _ runtime.Runtime = (*Runtime)(nil)

// vmLink returns the guest link of a VM, or a zero Link (tests and logs).
func (r *Runtime) vmLink(id string) Link {
	r.mu.Lock()
	defer r.mu.Unlock()
	if v := r.vms[id]; v != nil && v.link != nil {
		return *v.link
	}
	return Link{}
}
