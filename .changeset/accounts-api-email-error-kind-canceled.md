---
"accounts-api": patch
---

Log a billing email send that its context canceled or expired with `error_kind` `canceled` in `billing_email_failed`. Before, the log reported `network` for every canceled send, because the HTTP client wraps the context error in a network error. An HTTP client timeout with a live context still logs `network`.
