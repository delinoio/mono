---
"ownsand-api": patch
---

When the rate limiter reaches its key cap and no bucket has refilled, it now forgets the buckets charged longest ago instead of a random half, so recently limited keys stay limited.
