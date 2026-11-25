---
"ownsand-api": patch
---

Reject empty, whitespace-only, and delimiter-only `STUN_URLS` values at startup. Omit `STUN_URLS` to use the default STUN server.
