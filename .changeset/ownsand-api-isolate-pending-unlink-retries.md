---
"ownsand-api": patch
---

Run the retries of pending billing unlinks in their own batch apart from the other steps of the billing status worker. A slow `UnlinkOrganization` call no longer delays the status poll, the poll after a lease handover or the recording of time-based suspensions. One retry batch runs at a time, in `next_attempt_time` order; a batch stops at the next row when the instance loses the lease, and the worker waits for a running batch when it stops. The back-off bounds and the billing phases do not change.
