---
"ownsand-api": patch
---

The billing status poll of the lease holder now starts every `Poll` interval (1 minute). Before, the interval ran from the end of the previous poll and was checked only at each 10-second step, so a poll that ended after its step began, and a step that ran a few milliseconds early, stretched the interval to 70 seconds.
