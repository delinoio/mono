---
"ownsand-sdk": patch
---

`createSandbox` now retries a lost response within the 60-second window of the contract instead of four attempts: it retries `deadline_exceeded`, waits at least the `retry_after` of a `resource_exhausted` error that is not a billing error, cancels a retry that gets no response at the end of the window and then fails with the last failure of the window. A caller abort stays terminal and rejects with the caller's reason.
