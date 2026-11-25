---
"ownsand-agent": patch
---

Keep `LOST_ON_DEVICE` when retrying cleanup of a paused sandbox after its VM exits. The maximum TTL still takes precedence if it expires before the retry.
