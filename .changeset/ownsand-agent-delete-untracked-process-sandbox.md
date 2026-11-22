---
"ownsand-agent": patch
---

The process test runtime now removes the files of a sandbox that it does not track. After an agent restart, the start-up cleanup and `Delete` removed no directory of a lost process-runtime sandbox, so `vms/<sandbox ID>` (`root` and `guest-state`) stayed on the device while the record ended as deleted or lost. The runtime also records the socket directory that macOS keeps outside the sandbox directory, removes it with the sandbox, and removes a sandbox whose helper exited when the runtime closes.
