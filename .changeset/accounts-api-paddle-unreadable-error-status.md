---
"accounts-api": patch
---

Keep the Paddle status code when the body of an error response cannot be read. An unreadable 429, 401 or 403 answer now counts as not applied, so a manual top-up returns `unavailable` and the same idempotency key can charge again; an unreadable 5xx answer stays unknown.
