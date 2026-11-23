---
"ownsand-api": patch
---

The scheduler now allows a memory-state restore, and prefers the snapshot device because of it, only when the requested `disk_gib` equals the disk size of the snapshot, in addition to the vCPU count, memory size and network mode. Before, a sandbox created from a snapshot or template with a larger disk was still pinned to the snapshot device and got the memory-restore permission, although the agent boots such a sandbox cold. Now it is placed on the eligible device with the lowest allocated share (lowest device ID on a tie), gets an import when that device has no copy, and carries no memory-restore permission. A snapshot that is not exported still runs only on the devices that hold it, and a smaller disk is still rejected with `invalid_argument`.
