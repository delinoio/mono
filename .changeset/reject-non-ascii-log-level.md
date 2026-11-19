---
"accounts-api": patch
---

Reject `LOG_LEVEL` values that only match a level name after Unicode case mapping, such as `İNFO` (U+0130 followed by `NFO`), instead of accepting them as `info`. The server now ignores only ASCII letter case when it matches `LOG_LEVEL`, so such a value stops the server at startup.
