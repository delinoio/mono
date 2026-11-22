---
"ownsand-api": patch
---

Reject a `STUN_RATE_LIMIT_BURST_BYTES` below 64 bytes at startup. A smaller burst is below the size of one STUN binding response, so the rate limiter would drop every response.
