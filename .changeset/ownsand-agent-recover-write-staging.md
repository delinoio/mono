---
"ownsand-agent": patch
---

Recover unfinished guest uploads when the helper restarts before it accepts the first chunk. After expiry, the helper removes staging files whose ownership it can verify and preserves the destination.
