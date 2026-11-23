---
"ownsand-api": patch
---

Enforce the device nonce deadline again at the locked validation point, so a nonce that expired while the request waited for the device row lock is rejected and not consumed.
