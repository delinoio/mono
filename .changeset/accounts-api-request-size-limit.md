---
"accounts-api": patch
---

Limit every Connect request message to 1 MiB after decompression. A larger request, also a compressed one that inflates over 1 MiB, now fails with `resource_exhausted` before authentication instead of being buffered without a limit.
