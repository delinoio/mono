---
"ownsand-console": patch
---

Keep every valid user label, including `mock-idempotency-key`, on sandboxes in the mock API, and fail a reused create idempotency key with other parameters like the real API.
