---
"ownsand-api": patch
---

Remove a pending relay grant when the agent acknowledges it at or after its admission deadline (expiry plus the 30-second admission tolerance) instead of installing a grant that can no longer be admitted. A waiting `AuthorizeRelay` call now returns `unavailable` without a retry delay, and the `authorization_id` is released.
