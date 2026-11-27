---
"ownsand-api": patch
---

Keep a pending Accounts token refresh when Accounts rejects the cached token. Later callers share the refresh and reuse its replacement token.
