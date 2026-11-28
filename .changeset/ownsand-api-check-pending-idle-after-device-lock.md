---
"ownsand-api": patch
---

Check how long a device has been in `PENDING_IDLE` after acquiring the placement locks. A create request that waits past the 24-hour cutoff can no longer reserve capacity on the draining device.
