---
"ownsand-api": patch
---

Report `deadline_exceeded` instead of `canceled` when a unary request fails because its deadline passed. A canceled caller still gets `canceled`.
