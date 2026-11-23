---
"ownsand-console": patch
---

Show a positive sandbox TTL below one second, such as an idle TTL of 500 ms, as a duration in milliseconds (for example `500 ms`, or `<1 ms` below one millisecond) in the Idle TTL and Maximum TTL rows of the sandbox page, instead of `0 s`. A missing TTL still shows `—`, an exact zero shows `0 s`, and whole-second values are unchanged.
