---
"accounts-api": patch
---

Let a request that waits for a Logto key set refresh return when its context ends, serve a cached signing key without waiting for another request's refresh, and keep the shared refresh running when the request that started it is canceled.
