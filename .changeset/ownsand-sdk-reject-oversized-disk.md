---
"ownsand-sdk": patch
---

`createSandbox` rejects a `diskGib` above 8589934591 before it sends a request.
