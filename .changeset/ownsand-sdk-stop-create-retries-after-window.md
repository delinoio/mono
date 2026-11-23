---
"ownsand-sdk": patch
---

`createSandbox` no longer sends a retry after its 60-second retry window has ended. When the back-off wakes late (for example in a suspended process or a starved event loop) and no time remains in the window, the create now rejects with the last failure of the window and sends no further `CreateSandbox` request. A retry that starts with time left still reuses the same idempotency key and is still cancelled when the window ends, and a caller abort still rejects with the caller's reason.
