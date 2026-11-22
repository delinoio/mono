---
"ownsand-sdk": patch
---

A call whose `accessToken` function is still pending now ends when the caller cancels or when the 60-second `AuthorizeRelay` window ends, with the caller's abort reason or the existing `DataPathError` with the Connect code `unavailable`. A token that resolves after the abort starts no RPC.
