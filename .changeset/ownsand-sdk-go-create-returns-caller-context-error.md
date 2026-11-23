---
"ownsand-sdk": patch
---

Go SDK: `Client.CreateSandbox` now returns the caller's `context.Canceled` or `context.DeadlineExceeded` error when the caller's context has ended and an attempt (the first or a retry) failed, and sends no further attempt. Before, it returned the failure that the RPC reported, which did not have to wrap the context error. A retry window that ends while the caller's context is still live still returns the last failure of the window, and retries within the window still reuse one idempotency key.
