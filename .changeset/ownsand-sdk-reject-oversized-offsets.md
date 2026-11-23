---
"ownsand-sdk": patch
---

Reject a `readFile` offset or an `attachExec` stdout or stderr offset above 9223372036854775807 (the largest signed 64-bit value) with a synchronous `OwnSandValidationError` before any request starts.
