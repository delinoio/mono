---
"ownsand-api": patch
---

Reject a `STUN_URLS` entry whose port is outside 1 to 65535 at startup, instead of publishing a URL that agents and callers cannot use.
