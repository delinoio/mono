---
"ownsand-sdk": patch
---

Log one `ok` record when a direct `client.relay` consumer closes a `ReadFile`, `Exec`, or `AttachExec` stream after receiving its terminal event. Caller cancellation and wire errors that the consumer reads retain their Connect codes.
