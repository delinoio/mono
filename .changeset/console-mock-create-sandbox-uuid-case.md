---
"ownsand-console": patch
---

Make the mock API treat a `CreateSandbox` request as one request when its organization, idempotency key, placement device or snapshot and template IDs differ only in hex case, so a replay returns the first sandbox instead of creating another one, and a replay with other parameters fails with `AlreadyExists`.
