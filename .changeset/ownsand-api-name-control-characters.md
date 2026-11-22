---
"ownsand-api": patch
---

Display names now accept the Unicode replacement character U+FFFD as ordinary text and reject the C1 control characters U+0080 to U+009F, in addition to the C0 range and DEL. Malformed UTF-8 is rejected with its own `invalid_argument` message.
