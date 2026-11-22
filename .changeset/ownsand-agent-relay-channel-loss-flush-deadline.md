---
"ownsand-agent": patch
---

Bound the shutdown of a relay connection after its agent channel was lost to 5 seconds, also when the writer is blocked behind a queued marker or the request close stalls, so a live grant of the new channel gets a new relay connection.
