---
"ownsand-sdk": patch
---

Keep the TypeScript sandbox handle on peer-to-peer when the access token callback fails during session setup. Return the credential error to the caller and retry setup on the next operation.
