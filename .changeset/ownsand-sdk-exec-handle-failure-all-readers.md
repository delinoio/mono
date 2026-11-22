---
"ownsand-sdk": patch
---

An exec that fails with a terminal error now reports the same error to every reader of its `ExecHandle`. Before, only the reader that owned the failing read got the error: a queued iterator ended silently or `wait()` threw "the exec ended without a result", and a later `wait()` lost the error. The handle now records the first failure and rethrows it from the iterator and from every `wait()` call, including a non-`Error` abort reason.
