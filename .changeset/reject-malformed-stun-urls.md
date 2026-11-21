---
"ownsand-api": patch
---

Reject a malformed or hostless STUN URL in `STUN_URLS` at startup. The server no longer publishes a URL such as `stun:` to agents, where it made peer connection setup fail.
