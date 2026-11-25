---
"ownsand-agent": patch
---

Confirm Hyper-V VM exit before deleting sandbox files or publishing a Windows base image. If the Host Compute Service disconnects, the agent checks the VM's status through a fresh query. Closing the original handle releases its watcher and preserves sandbox files when exit remains unconfirmed.
