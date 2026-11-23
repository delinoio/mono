---
"ownsand-sdk": patch
---

Go SDK: a peer-to-peer operation whose context is canceled no longer receives the frames that are already queued on its stream. An exec event callback and the writer of a file read stop at the cancellation, and a canceled write attempt no longer returns a queued write result. The receive returns the context error even when the queue is not empty. A caller that is still active continues to receive the frames that were queued before a stream failure or the end of the session.
