---
"ownsand-sdk": patch
---

The Go SDK now returns token source errors that match `io.EOF` while opening an `Exec`, `AttachExec` or `ReadFile` relay stream. Callers can inspect the original cause with `errors.Is`. The operation ends without sending a relay stream request or retrying.
