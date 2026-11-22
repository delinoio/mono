---
"ownsand-api": patch
---

Let a caller that waits for the `delino-accounts` machine-to-machine token return as soon as its own context ends, and let `Invalidate` return without waiting for a token request, instead of holding one lock across the request.
