---
"accounts-api": patch
---

Let a request that waits for a Logto Management API token refresh return when its context ends, instead of waiting for another request's refresh to finish, and keep the shared refresh running when the request that started it is canceled.
