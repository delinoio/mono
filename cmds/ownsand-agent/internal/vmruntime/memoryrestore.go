// SPDX-License-Identifier: FSL-1.1-ALv2

package vmruntime

import ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"

// MemoryRestoreEligible reports whether a runtime may restore the guest memory
// state of spec.Snapshot instead of cold-booting the guest. The memory state
// pins the guest to the snapshot's hardware, so the restore needs a snapshot
// with memory state, an allowed restore, a host that supports the memory
// state (memoryStateSupported), a sandbox without network, and the same vCPU
// count, memory size and disk size as the snapshot. A larger disk cannot be
// restored from memory because the guest already holds the old block device
// geometry; such a request cold-boots from the snapshot disk and grows it.
func MemoryRestoreEligible(spec Spec, memoryStateSupported bool) bool {
	return spec.Snapshot != nil && spec.Snapshot.HasMemoryState && spec.MemoryRestoreAllowed &&
		memoryStateSupported &&
		spec.NetworkMode == ownsandv1.NetworkMode_NETWORK_MODE_NONE &&
		spec.Snapshot.Resources.GetVcpuCount() == spec.Resources.GetVcpuCount() &&
		spec.Snapshot.Resources.GetMemoryMib() == spec.Resources.GetMemoryMib() &&
		spec.Snapshot.Resources.GetDiskGib() == spec.Resources.GetDiskGib()
}
