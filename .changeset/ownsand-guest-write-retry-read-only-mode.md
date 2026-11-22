---
"ownsand-agent": patch
---

The guest helper now lets an unprivileged guest user repeat a file write finish after a failed rename when the finish mode has no owner write bit (for example `0444`). Before, the staging file kept the read-only mode, the reopen failed, and the repeated finish failed on a closed file. The helper now restores owner read and write permission before it reopens the staging file, and the repeated finish applies the requested mode again. A repeat on a staging file that cannot be reopened now returns a clear `failed_precondition` error.
