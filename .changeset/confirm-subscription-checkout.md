---
"accounts-api": patch
---

Return a subscription checkout ID only after Paddle confirms an automatic transaction in `draft`, `ready` or `billed` status. An unconfirmed response gives callers a retryable error and preserves any pending checkout.
