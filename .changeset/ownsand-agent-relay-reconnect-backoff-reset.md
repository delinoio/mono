---
"ownsand-agent": patch
---

Restart the relay reconnect back-off at its 200-millisecond first step after a relay connection that was open for at least 60 seconds ended, so earlier failures do not delay the next dial by the maximum back-off.
