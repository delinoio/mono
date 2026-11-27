---
"ownsand-agent": patch
---

Stop fresh snapshot captures when their command context ends or device removal starts. If a delete was accepted during a capture that fails, return `ABORTED` and retain any failed cleanup for a later delete retry.
