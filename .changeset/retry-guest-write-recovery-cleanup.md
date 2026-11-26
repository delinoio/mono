---
"ownsand-agent": patch
---

Report recovered guest writes as expired while cleanup is pending. Retry stale write record removal on the next sweep if startup cleanup fails.
