---
"ownsand-sdk": patch
---

A `for await` loop over an exec handle that ends early (`break`, `return` or an exception in the loop body) no longer keeps the relay stream alive. The idle limit of 90 seconds applies again to the abandoned stream: the SDK cancels the call, releases its timer and its listener on the caller's `AbortSignal`, and a Node.js process with no other work can exit. `wait()` after the early exit still resumes the same stream, or continues with `AttachExec` from the delivered offsets after the cancellation.
