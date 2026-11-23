---
"ownsand-agent": patch
---

A duplicate snapshot create now reads the existing snapshot while it holds the manager lock, so a delete that marks the snapshot at the same time no longer causes a data race. The create returns the ready snapshot when it runs before the delete mark, and FAILED_PRECONDITION after it.
