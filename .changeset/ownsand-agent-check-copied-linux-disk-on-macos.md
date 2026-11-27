---
"ownsand-agent": patch
---

On macOS, reject a Linux sandbox create if a base image rebuild causes the copied disk to exceed the requested capacity. The agent removes the sandbox directory and reports a failed precondition before starting the VM.
