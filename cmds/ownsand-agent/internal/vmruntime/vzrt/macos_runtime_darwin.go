// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build darwin && cgo

package vzrt

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/gpt"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/guestclient"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/images"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// MacOSGuestMAC is the MAC address of the network device of every macOS
// guest. macOS names a network interface and binds its network service to
// the interface's MAC address, so every sandbox must present the one that
// the image build configured. Each sandbox has its own network stack and
// link, so the address never meets another guest.
var MacOSGuestMAC = net.HardwareAddr{0x02, 0x6f, 0x73, 0x6e, 0x64, 0x01}

// MacOSHardwareModelSupported reports whether this Mac runs macOS guests of
// the hardware model.
func (r *Runtime) MacOSHardwareModelSupported(model []byte) bool {
	return macOSGuestsSupported && macOSHardwareModelSupported(model)
}

var _ vmruntime.MacOSHardwareModelChecker = (*Runtime)(nil)

// macOSImage returns the hardware model of the built macOS base image, or
// nil and the reason why there is no usable image.
func (r *Runtime) macOSImage() ([]byte, string) {
	if !macOSGuestsSupported {
		return nil, "macOS guests need an Apple silicon Mac"
	}
	dir := r.opts.MacOSImage
	if dir == "" {
		return nil, "no macOS image directory is configured"
	}
	// Resolve first restores the previous image when an image build ended
	// between the two renames of its install.
	if _, err := images.Resolve(dir); err != nil {
		return nil, "the macOS base image is not built; run `ownsand-agent image build`"
	}
	for _, name := range []string{images.MacOSDisk, images.MacOSAuxStorage} {
		if !fileExists(filepath.Join(dir, name)) {
			return nil, "the macOS base image is incomplete; build it again"
		}
	}
	model, err := os.ReadFile(filepath.Join(dir, images.MacOSHardwareModel))
	if err != nil {
		return nil, "the macOS base image has no hardware model; build it again"
	}
	if !macOSHardwareModelSupported(model) {
		return nil, "this Mac does not support the hardware model of the macOS base image"
	}
	return model, "the macOS base image is present"
}

// reserveMachineID records id as the machine identifier of v unless another
// VM of this runtime uses it. Virtualization.framework must never run two
// VMs with the same macOS machine identifier.
func (r *Runtime) reserveMachineID(v *vm, id []byte) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, other := range r.vms {
		if other != v && other.guestOS == ownsandv1.GuestOs_GUEST_OS_MACOS && bytes.Equal(other.machineID, id) {
			return false
		}
	}
	v.machineID = id
	return true
}

// createMacOS boots a macOS guest from the macOS base image or a macOS
// snapshot. Every cold boot gets a new machine identifier; only a
// memory-state restore uses the snapshot's identifier, and only while no
// other VM uses it.
func (r *Runtime) createMacOS(ctx context.Context, spec vmruntime.Spec) (vmruntime.VM, error) {
	if !macOSGuestsSupported {
		return nil, fmt.Errorf("%w: macOS guests need an Apple silicon Mac", vmruntime.ErrUnsupported)
	}
	src := r.opts.MacOSImage
	if spec.Snapshot != nil {
		src = spec.Snapshot.Dir
	} else {
		if model, detail := r.macOSImage(); model == nil {
			return nil, fmt.Errorf("%w: %s", vmruntime.ErrUnsupported, detail)
		}
		if meta, err := images.Resolve(src); err == nil {
			if spec.Resources.GetVcpuCount() < meta.MinimumCPUCount || spec.Resources.GetMemoryMib() < meta.MinimumMemoryMiB {
				return nil, fmt.Errorf("%w: macOS guests need at least %d vCPUs and %d MiB of memory", vmruntime.ErrUnsupported, meta.MinimumCPUCount, meta.MinimumMemoryMiB)
			}
		}
	}
	model, err := os.ReadFile(filepath.Join(src, images.MacOSHardwareModel))
	if err != nil {
		return nil, fmt.Errorf("vzrt: read the macOS hardware model: %w", err)
	}
	if !macOSHardwareModelSupported(model) {
		return nil, fmt.Errorf("%w: this Mac does not support the hardware model of the macOS guest", vmruntime.ErrUnsupported)
	}
	start := time.Now()
	if err := os.RemoveAll(spec.Dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(spec.Dir, 0o700); err != nil {
		return nil, err
	}
	v := &vm{id: spec.SandboxID, dir: spec.Dir, network: spec.NetworkMode, guestOS: spec.GuestOS,
		hardwareModel: model, vcpu: spec.Resources.GetVcpuCount(), memMiB: spec.Resources.GetMemoryMib()}
	cleanup := func() { r.discard(v) }
	prepare := func() error {
		if err := copyFile(filepath.Join(src, images.MacOSDisk), filepath.Join(spec.Dir, diskFile)); err != nil {
			return fmt.Errorf("vzrt: prepare disk: %w", err)
		}
		if err := copyFile(filepath.Join(src, images.MacOSAuxStorage), filepath.Join(spec.Dir, auxFile)); err != nil {
			return fmt.Errorf("vzrt: prepare auxiliary storage: %w", err)
		}
		return nil
	}
	if err := prepare(); err != nil {
		cleanup()
		return nil, err
	}
	r.mu.Lock()
	r.vms[spec.SandboxID] = v
	r.mu.Unlock()

	memoryRestore := vmruntime.MemoryRestoreEligible(spec, memoryStateSupported)
	if memoryRestore {
		err := r.restoreMacOS(ctx, v, spec)
		if err == nil {
			v.restore = ownsandv1.SandboxRestorePath_SANDBOX_RESTORE_PATH_MEMORY_STATE
			clockCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			if err := guestclient.New(v.Dial).SetClock(clockCtx, time.Now()); err != nil {
				r.logger.Warn("set guest clock after memory restore failed", "sandbox_id", v.id, "error", err)
			}
			cancel()
			r.logger.Info("macOS VM restored from memory state", "sandbox_id", v.id, "total_ms", time.Since(start).Milliseconds())
			return v, nil
		}
		r.logger.Warn("macOS memory-state restore failed; booting the snapshot disk cold", "sandbox_id", v.id, "error", err)
		// Never boot the snapshot disk cold while the failed attempt's VM may
		// still run on the same disk.
		if stopErr := r.stopVM(v); stopErr != nil {
			cleanup()
			return nil, fmt.Errorf("vzrt: stop the VM of the failed memory-state restore: %w", stopErr)
		}
		r.mu.Lock()
		v.machineID = nil
		r.mu.Unlock()
		if err := prepare(); err != nil {
			cleanup()
			return nil, err
		}
	}
	disk := filepath.Join(spec.Dir, diskFile)
	if err := growFile(disk, spec.Resources.GetDiskGib()<<30); err != nil {
		cleanup()
		return nil, fmt.Errorf("vzrt: resize disk: %w", err)
	}
	// Move the backup partition table to the end of the grown disk; the
	// guest helper then grows the system container into the free space.
	if err := gpt.Fit(disk); err != nil {
		cleanup()
		return nil, fmt.Errorf("vzrt: resize the partition table: %w", err)
	}
	if err := r.coldBootMacOS(ctx, v, spec); err != nil {
		cleanup()
		return nil, err
	}
	if spec.Snapshot != nil {
		v.restore = ownsandv1.SandboxRestorePath_SANDBOX_RESTORE_PATH_DISK_COLD_BOOT
	} else {
		v.restore = ownsandv1.SandboxRestorePath_SANDBOX_RESTORE_PATH_BASE_IMAGE
	}
	r.logger.Info("macOS VM started", "sandbox_id", v.id, "restore_path", v.restore.String(), "total_ms", time.Since(start).Milliseconds())
	return v, nil
}

// configureMacOS builds the configuration of the macOS guest v with the
// machine identifier id (nil for a new one) and reserves the identifier.
func (r *Runtime) configureMacOS(v *vm, spec vmruntime.Spec, id []byte) error {
	m := MacOSMachine{
		Disk:          filepath.Join(v.dir, diskFile),
		AuxStorage:    filepath.Join(v.dir, auxFile),
		HardwareModel: v.hardwareModel,
		MachineID:     id,
		CPUs:          spec.Resources.GetVcpuCount(),
		MemoryMiB:     spec.Resources.GetMemoryMib(),
	}
	if spec.NetworkMode == ownsandv1.NetworkMode_NETWORK_MODE_OUTBOUND {
		dev, err := r.attachNetwork(v, MacOSGuestMAC)
		if err != nil {
			return err
		}
		m.Network = dev
	}
	cfg, used, err := NewMacOSConfiguration(m)
	if err != nil {
		return err
	}
	if !r.reserveMachineID(v, used) {
		return fmt.Errorf("vzrt: another VM uses the machine identifier of sandbox %s", v.id)
	}
	return r.newMachine(v, cfg)
}

func (r *Runtime) coldBootMacOS(ctx context.Context, v *vm, spec vmruntime.Spec) error {
	if err := r.configureMacOS(v, spec, nil); err != nil {
		return err
	}
	if err := v.machine.Start(); err != nil {
		return fmt.Errorf("vzrt: start macOS VM: %w", err)
	}
	return r.waitGuest(ctx, v)
}

// restoreMacOS restores the saved state of a macOS snapshot with the
// snapshot's machine identifier, which no other VM may use at that time.
func (r *Runtime) restoreMacOS(ctx context.Context, v *vm, spec vmruntime.Spec) error {
	id, err := os.ReadFile(filepath.Join(spec.Snapshot.Dir, images.MacOSMachineID))
	if err != nil {
		return err
	}
	if !r.reserveMachineID(v, id) {
		return fmt.Errorf("vzrt: a running VM uses the machine identifier of snapshot %s", spec.Snapshot.SnapshotID)
	}
	if err := r.configureMacOS(v, spec, id); err != nil {
		return err
	}
	if err := restoreMemory(v.machine, filepath.Join(spec.Snapshot.Dir, snapState)); err != nil {
		return err
	}
	if err := v.machine.Resume(); err != nil {
		return fmt.Errorf("vzrt: resume restored macOS VM: %w", err)
	}
	return r.waitGuest(ctx, v)
}

// snapshotMacOS writes the snapshot of a paused macOS guest: APFS clones of
// the disk and the auxiliary storage, the hardware model and, for NONE
// sandboxes, the machine state with the machine identifier.
func (r *Runtime) snapshotMacOS(v *vm, dir string) (vmruntime.SnapshotInfo, error) {
	info := vmruntime.SnapshotInfo{MacOSHardwareModel: v.hardwareModel}
	if memoryStateSupported && v.network == ownsandv1.NetworkMode_NETWORK_MODE_NONE {
		machine, _, err := v.current()
		if err != nil {
			return vmruntime.SnapshotInfo{}, err
		}
		if err := saveMemory(machine, filepath.Join(dir, snapState)); err != nil {
			r.logger.Warn("saving the macOS machine state failed; taking a disk-only snapshot", "sandbox_id", v.id, "error", err)
			os.Remove(filepath.Join(dir, snapState))
		} else {
			info.HasMemoryState = true
		}
	}
	// The machine identifier is kept with every macOS snapshot; a cold boot
	// does not use it.
	if err := os.WriteFile(filepath.Join(dir, images.MacOSMachineID), v.machineID, 0o600); err != nil {
		return vmruntime.SnapshotInfo{}, err
	}
	if err := os.WriteFile(filepath.Join(dir, images.MacOSHardwareModel), v.hardwareModel, 0o600); err != nil {
		return vmruntime.SnapshotInfo{}, err
	}
	if err := copyFile(filepath.Join(v.dir, diskFile), filepath.Join(dir, images.MacOSDisk)); err != nil {
		return vmruntime.SnapshotInfo{}, err
	}
	if err := copyFile(filepath.Join(v.dir, auxFile), filepath.Join(dir, images.MacOSAuxStorage)); err != nil {
		return vmruntime.SnapshotInfo{}, err
	}
	for _, name := range []string{snapState, images.MacOSDisk, images.MacOSAuxStorage, images.MacOSHardwareModel, images.MacOSMachineID} {
		if st, err := os.Stat(filepath.Join(dir, name)); err == nil {
			info.SizeBytes += st.Size()
		}
	}
	return info, nil
}
