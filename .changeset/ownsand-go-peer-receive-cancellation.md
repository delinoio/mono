---
"ownsand-sdk": patch
---

Stop peer-to-peer delivery in the Go SDK when the receive detects cancellation or deadline expiry. A frame that arrives during cancellation can no longer trigger an exec callback, write file data to the caller, or complete the operation successfully. Active callers still receive queued frames in order before a stream failure or session end.
