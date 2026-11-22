---
"ownsand-agent": patch
---

Record a sandbox as `PAUSED` when a Firecracker snapshot paused it and could not resume it. A snapshot of a running sandbox pauses the VM for the capture; when Firecracker rejected the resume, the snapshot failed but the agent kept the record `RUNNING`, so a later resume command returned success without reaching the VM and the sandbox stayed frozen. The runtime now reports the paused source with the error, also when the capture failed first, and the agent publishes the paused state with a new revision, so one explicit resume reaches the runtime. A snapshot that fails for another reason, or whose resume succeeds, leaves the source state unchanged.
