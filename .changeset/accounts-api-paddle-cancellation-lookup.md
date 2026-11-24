---
"accounts-api": patch
---

Return a retryable `unavailable` error when Accounts API cannot check whether Paddle already canceled a subscription or checkout during billing account deletion. The billing account and its credit remain intact until deletion can complete.
