---
"ownsand-agent": patch
---

A Windows sandbox create whose disk size is below the virtual size of the disk it starts from (the base image disk, or the snapshot disk) now fails as a failed precondition before the VM starts, names the requested and the required size in GiB and removes the VM directory. Before, the agent ignored the smaller request and booted the guest with the larger disk.
