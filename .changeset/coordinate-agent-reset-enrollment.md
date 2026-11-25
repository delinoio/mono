---
"ownsand-agent": patch
---

Make reset wait for enrollment in the same state directory before deleting local data. If the lock stays busy, reset reports an error so the operator can retry.
