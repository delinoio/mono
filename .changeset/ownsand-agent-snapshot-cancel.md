---
"ownsand-agent": patch
---

A snapshot capture whose channel context ended is no longer published as ready, even when the runtime returns success after the cancellation: the agent removes the record and the content directory, releases the pending capture and fails the command with `ABORTED`. A snapshot that was published before stays available. The Firecracker disk copy now stops at the next chunk and removes its partial file, and the Hyper-V disk copy uses `CopyFileExW` and stops when the context ends. The resume of the VM after a capture does not depend on the context.
