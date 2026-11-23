---
"ownsand-sdk": patch
---

Treat a `resource_exhausted` error with a billing reason (`ACCOUNT_REQUIRED`, `BLOCKED` or `SUSPENDED`) as terminal: `classifyFailure` and `OpenSession` no longer retry it or fall back to the relay. Add the exports `billingReasonOf` and `isBillingError`, and a `client.billing` service client for `BillingService`.
