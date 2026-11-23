---
"ownsand-api": patch
---

`CreateSandbox` now decides the expiry of an idempotency key with one time sample for the locked lookup and the key write. Before, a key that expired between the two samples made the request fail with an internal error instead of reusing the key. Replay, conflict and the 24-hour retention are unchanged.
