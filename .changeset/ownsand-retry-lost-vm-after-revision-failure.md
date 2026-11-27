---
"ownsand-agent": patch
---

Retry cleanup of an unexpectedly exited VM when revision storage recovers. The agent keeps the last visible state and revision during the failure and preserves applicable TTL end reasons.
