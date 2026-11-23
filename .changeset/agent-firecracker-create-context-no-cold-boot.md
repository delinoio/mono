---
"ownsand-agent": patch
---

A Linux sandbox create whose context is canceled or expires during a memory-state restore now fails with that context error and does not take the cold-boot fallback. Before, any restore error took the fallback, so the agent logged that it was booting the snapshot disk cold, and a failure to stop the restore process replaced the context error with a stop error. The create now stops the restore process, releases the registration and the VM directory (or keeps them when the process does not exit, as before) and starts no second Firecracker process. A snapshot-load error while the context is still active still stops the process and boots the snapshot disk cold.
