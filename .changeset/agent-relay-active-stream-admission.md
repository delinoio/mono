---
"ownsand-agent": patch
---

The agent drops relay stream opens from retired or canceled connections. This keeps those connections from using stream slots on live grants after the relay reconnects.
