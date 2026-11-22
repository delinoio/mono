---
"ownsand-agent": patch
---

Keep a relay grant that admitted no stream, or whose last stream ended before its expiry, until its expiry plus the 30-second admission tolerance, so a first request that the control plane still admits no longer meets a released grant.
