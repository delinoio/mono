---
"ownsand-api": patch
---

Keep the newest committed device limits as the last configuration that reaches an agent. A per-channel lock now covers the read of the limits and the queueing of the configuration message, so a limits change that commits while the channel opens is no longer overwritten by the older limits.
