---
"ownsand-api": patch
---

Reject a `WriteFile` result that is already queued when the call deadline passes. The call now fails with `deadline_exceeded` and sends the stream close frame before it counts the result in the `relay_rpc` record or returns the result's credit to the agent; before, the call failed with the same code only after it had counted the result and tried the credit return. A caller that canceled first keeps the cancellation.
