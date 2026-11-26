---
"ownsand-sdk": patch
---

Keep the original exec recovery error when a sandbox state reply arrives at or after the deadline. End a paused exec with a protocol error if its resume poll receives a response without a sandbox.
