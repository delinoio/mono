---
"ownsand-sdk": patch
---

Closing a sandbox handle makes in-flight peer exec and attach reads fail with `OwnSandHandleClosedError`. The SDK discards buffered output and terminal results without requesting a relay grant.
