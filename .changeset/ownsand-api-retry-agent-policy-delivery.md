---
"ownsand-api": patch
---

Retry agent update policy delivery after a transient evaluation error. Replay uses the latest committed policy and skips commands already queued on the same channel.
