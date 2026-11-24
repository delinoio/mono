---
"ownsand-api": patch
---

Raise the minimum STUN response burst to 800 bytes so that the largest 420 error response fits in a fresh byte budget. If you set `STUN_RATE_LIMIT_BURST_BYTES` below 800, raise it to at least 800 before deploying this release. Startup rejects smaller values. The default remains 4096 bytes.
