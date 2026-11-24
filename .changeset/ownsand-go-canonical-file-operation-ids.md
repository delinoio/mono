---
"ownsand-sdk": patch
---

The Go SDK now accepts uppercase UUID v7 file operation IDs on both transports. `ReadFileFrom` and `OpenFileWrite` use the lowercase ID for relay authorization, retries and continuations.
