---
"ownsand-agent": patch
---

End a sandbox as DELETED with DELETED_BY_CALLER, and not as FAILED with LOST_ON_DEVICE, when its VM exits while a failed caller delete has left the record in DELETING and no deadline has passed.
