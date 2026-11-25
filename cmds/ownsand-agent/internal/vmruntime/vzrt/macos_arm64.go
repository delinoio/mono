// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build darwin && cgo && arm64

package vzrt

import (
	"errors"
	"fmt"

	"github.com/Code-Hex/vz/v3"
)

// macOSGuestsSupported: Virtualization.framework runs macOS guests only on
// Apple silicon.
const macOSGuestsSupported = true

// NewMacOSConfiguration returns the VM configuration of a macOS guest and the
// machine identifier that it uses. The guest gets a virtio block device
// (full synchronization), a virtio socket device for the guest helper, an
// entropy device, a graphics device without a window and, for OUTBOUND
// guests, the network device.
func NewMacOSConfiguration(m MacOSMachine) (*vz.VirtualMachineConfiguration, []byte, error) {
	memoryBytes, err := memoryMiBToBytes(m.MemoryMiB)
	if err != nil {
		return nil, nil, err
	}
	model, err := vz.NewMacHardwareModelWithData(m.HardwareModel)
	if err != nil {
		return nil, nil, fmt.Errorf("vzrt: hardware model: %w", err)
	}
	if !model.Supported() {
		return nil, nil, errors.New("vzrt: this Mac does not support the hardware model of the macOS guest")
	}
	var id *vz.MacMachineIdentifier
	if m.MachineID != nil {
		id, err = vz.NewMacMachineIdentifierWithData(m.MachineID)
	} else {
		id, err = vz.NewMacMachineIdentifier()
	}
	if err != nil {
		return nil, nil, fmt.Errorf("vzrt: machine identifier: %w", err)
	}
	aux, err := vz.NewMacAuxiliaryStorage(m.AuxStorage)
	if err != nil {
		return nil, nil, fmt.Errorf("vzrt: auxiliary storage: %w", err)
	}
	platform, err := vz.NewMacPlatformConfiguration(vz.WithMacHardwareModel(model),
		vz.WithMacMachineIdentifier(id), vz.WithMacAuxiliaryStorage(aux))
	if err != nil {
		return nil, nil, err
	}
	boot, err := vz.NewMacOSBootLoader()
	if err != nil {
		return nil, nil, err
	}
	cfg, err := vz.NewVirtualMachineConfiguration(boot, uint(m.CPUs), memoryBytes)
	if err != nil {
		return nil, nil, err
	}
	cfg.SetPlatformVirtualMachineConfiguration(platform)

	attachment, err := vz.NewDiskImageStorageDeviceAttachmentWithCacheAndSync(m.Disk, false,
		vz.DiskImageCachingModeAutomatic, vz.DiskImageSynchronizationModeFull)
	if err != nil {
		return nil, nil, err
	}
	block, err := vz.NewVirtioBlockDeviceConfiguration(attachment)
	if err != nil {
		return nil, nil, err
	}
	cfg.SetStorageDevicesVirtualMachineConfiguration([]vz.StorageDeviceConfiguration{block})

	sock, err := vz.NewVirtioSocketDeviceConfiguration()
	if err != nil {
		return nil, nil, err
	}
	cfg.SetSocketDevicesVirtualMachineConfiguration([]vz.SocketDeviceConfiguration{sock})

	entropy, err := vz.NewVirtioEntropyDeviceConfiguration()
	if err != nil {
		return nil, nil, err
	}
	cfg.SetEntropyDevicesVirtualMachineConfiguration([]*vz.VirtioEntropyDeviceConfiguration{entropy})

	// macOS guests expect a display; nothing shows it.
	graphics, err := vz.NewMacGraphicsDeviceConfiguration()
	if err != nil {
		return nil, nil, err
	}
	display, err := vz.NewMacGraphicsDisplayConfiguration(1280, 800, 80)
	if err != nil {
		return nil, nil, err
	}
	graphics.SetDisplays(display)
	cfg.SetGraphicsDevicesVirtualMachineConfiguration([]vz.GraphicsDeviceConfiguration{graphics})

	if m.Network != nil {
		cfg.SetNetworkDevicesVirtualMachineConfiguration([]*vz.VirtioNetworkDeviceConfiguration{m.Network})
	}
	if ok, err := cfg.Validate(); !ok || err != nil {
		return nil, nil, fmt.Errorf("vzrt: invalid macOS VM configuration: %w", err)
	}
	return cfg, id.DataRepresentation(), nil
}

// macOSHardwareModelSupported reports whether this Mac runs guests of the
// hardware model.
func macOSHardwareModelSupported(model []byte) bool {
	if len(model) == 0 {
		return false
	}
	m, err := vz.NewMacHardwareModelWithData(model)
	return err == nil && m.Supported()
}
