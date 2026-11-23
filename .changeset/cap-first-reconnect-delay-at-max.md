---
"ownsand-agent": patch
---

Cap the first reconnect delay at the configured maximum when the maximum is positive and below the minimum, so a reconnect delay never exceeds its maximum.
