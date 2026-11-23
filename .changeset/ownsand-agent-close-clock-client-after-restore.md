---
"ownsand-agent": patch
---

Close the connection to the guest helper after the guest clock is set following a Firecracker memory-state restore, so the one-use client no longer leaves an idle connection open until the transport idle timeout. A failed clock call still only logs a warning and the restore still succeeds.
