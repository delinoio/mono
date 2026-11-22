---
"ownsand-api": patch
---

Limit every request message to 2 MiB after decompression, so an oversized or compressed request ends with `resource_exhausted` before authentication, and end an agent channel whose message exceeds the limit with the same code.
